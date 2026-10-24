package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/websines/structor-harness/harness/hx"
)

// Transport is a bidirectional JSON-RPC line channel (stdio, an in-memory pipe,
// etc.). The Client owns neither its dialing nor its lifetime beyond Close.
type Transport interface {
	io.ReadWriteCloser
}

// Protocol constants. protocolVersion is the MCP handshake version; the JSON-RPC
// method names are the wire vocabulary the bridge speaks.
const (
	jsonRPCVersion     = "2.0"
	protocolVersion    = "2024-11-05"
	clientName         = "forge-mcp"
	clientVersion      = "0.1.0"
	methodInitialize   = "initialize"
	methodListTools    = "tools/list"
	methodCallTool     = "tools/call"
	notifyInitialized  = "notifications/initialized"
	notifyToolsChanged = "notifications/tools/list_changed"
	// maxLineBytes caps a single JSON-RPC frame; a bigger line fails the scan
	// rather than growing the buffer without bound.
	maxLineBytes = 16 << 20
)

// errClosed is the cause a call sees when the client (or its transport) is gone.
var errClosed = errors.New("mcp: client closed")

// rpcRequest is an outbound JSON-RPC 2.0 request (id-bearing, expects a reply).
type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// rpcNotification is an outbound JSON-RPC 2.0 notification (no id, no reply).
type rpcNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// rpcError is a JSON-RPC 2.0 error object; it doubles as a Go error.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("mcp: rpc error %d: %s", e.Code, e.Message)
}

// rpcResponse is the settled outcome of one request, routed back to its caller.
type rpcResponse struct {
	Result json.RawMessage
	Err    *rpcError
}

// Client speaks JSON-RPC 2.0 to one MCP server over a Transport. A single read
// loop drains the transport and fans responses back to the goroutine that issued
// each request (keyed by id) and notifications to the change handler. The Client
// is safe for concurrent use.
type Client struct {
	server string
	t      Transport

	// writeMu serializes whole-message writes so two frames never interleave.
	writeMu sync.Mutex

	// mu guards the id counter and the pending-request table.
	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan rpcResponse

	// genMu guards the current tool generation and its change callback.
	genMu    sync.Mutex
	tools    []hx.ToolDefinition
	onChange func([]hx.ToolDefinition)

	// closeOnce makes shutdown idempotent across Close and read-loop exit.
	closeOnce sync.Once
	closed    chan struct{}
	closeErr  error
	errMu     sync.Mutex
	readErr   error
}

// NewClient constructs a Client over t and starts its read loop. The caller runs
// Initialize before listing or calling tools.
func NewClient(server string, t Transport) *Client {
	c := &Client{
		server:  server,
		t:       t,
		pending: make(map[int64]chan rpcResponse),
		closed:  make(chan struct{}),
	}
	go c.readLoop()
	return c
}

// Initialize runs the MCP initialize handshake, then fires the initialized
// notification (best effort — a server that ignores it is fine).
func (c *Client) Initialize(ctx context.Context) error {
	params := map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    clientName,
			"version": clientVersion,
		},
	}
	if _, err := c.call(ctx, methodInitialize, params); err != nil {
		return fmt.Errorf("mcp: initialize %q: %w", c.server, err)
	}
	_ = c.writeMessage(rpcNotification{JSONRPC: jsonRPCVersion, Method: notifyInitialized})
	return nil
}

// ListTools fetches the server's tool list, maps each MCP tool to an
// hx.ToolDefinition named mcp__<server>__<rawName>, records the result as the
// current generation, and returns it.
func (c *Client) ListTools(ctx context.Context) ([]hx.ToolDefinition, error) {
	defs, err := c.fetchTools(ctx)
	if err != nil {
		return nil, err
	}
	c.setTools(defs)
	return defs, nil
}

// Tools returns a copy of the current generation — the latest listing, which a
// list_changed notification replaces wholesale.
func (c *Client) Tools() []hx.ToolDefinition {
	c.genMu.Lock()
	defer c.genMu.Unlock()
	out := make([]hx.ToolDefinition, len(c.tools))
	copy(out, c.tools)
	return out
}

// OnChange installs (or clears, with nil) the callback fired after a
// notification-driven re-list replaces the generation. It is not called for the
// initial ListTools; set it after that first listing to observe only changes.
func (c *Client) OnChange(fn func([]hx.ToolDefinition)) {
	c.genMu.Lock()
	c.onChange = fn
	c.genMu.Unlock()
}

// Close shuts the client down: it stops the read loop, closes the transport, and
// unblocks any in-flight call. It is idempotent.
func (c *Client) Close() error {
	c.shutdown(errClosed)
	return c.closeErr
}

// fetchTools drains the (paginated) tools/list response and builds the next
// generation of definitions under deterministic public names. A within-server
// public-name collision earns a hash suffix (see publicToolName).
func (c *Client) fetchTools(ctx context.Context) ([]hx.ToolDefinition, error) {
	taken := make(map[string]struct{})
	var defs []hx.ToolDefinition
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.call(ctx, methodListTools, params)
		if err != nil {
			return nil, fmt.Errorf("mcp: tools/list %q: %w", c.server, err)
		}
		var res struct {
			Tools []struct {
				Name        string         `json:"name"`
				Description string         `json:"description"`
				InputSchema map[string]any `json:"inputSchema"`
			} `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return nil, fmt.Errorf("mcp: tools/list %q decode: %w", c.server, err)
		}
		for _, t := range res.Tools {
			name := publicToolName(c.server, t.Name, taken)
			taken[name] = struct{}{}
			defs = append(defs, c.toolDefinition(name, t.Name, t.Description, t.InputSchema))
		}
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	return defs, nil
}

// toolDefinition builds one hx.ToolDefinition whose Execute closes over the RAW
// MCP name (the public name is never sent on the wire) and calls tools/call.
func (c *Client) toolDefinition(public, raw, description string, schema map[string]any) hx.ToolDefinition {
	if schema == nil {
		schema = map[string]any{"type": "object"}
	}
	return hx.ToolDefinition{
		Schema: hx.ToolSchema{
			Name:        public,
			Description: description,
			Parameters:  schema,
		},
		Execute: func(rc hx.ToolRunContext, args []byte) (hx.ToolResult, error) {
			return c.callTool(rc.Ctx, raw, args)
		},
	}
}

// callTool marshals {"name":raw,"arguments":args} into a tools/call request,
// waits for the reply, and returns the MCP result content as ToolResult.Value
// (JSON) with IsError taken from the result's isError. A body never panics and
// never returns a Go error: a transport or decode failure becomes an isError
// ToolResult so the result surface stays a total function of its input.
func (c *Client) callTool(ctx context.Context, raw string, args []byte) (hx.ToolResult, error) {
	arguments := json.RawMessage(args)
	if len(args) == 0 {
		arguments = json.RawMessage("{}")
	}
	result, err := c.call(ctx, methodCallTool, map[string]any{
		"name":      raw,
		"arguments": arguments,
	})
	if err != nil {
		return toolErr("mcp: call %q: %v", raw, err), nil
	}
	var res struct {
		Content json.RawMessage `json:"content"`
		IsError bool            `json:"isError"`
	}
	if err := json.Unmarshal(result, &res); err != nil {
		return toolErr("mcp: call %q: decode result: %v", raw, err), nil
	}
	value := res.Content
	if len(value) == 0 {
		value = json.RawMessage("null")
	}
	return hx.ToolResult{Value: value, IsError: res.IsError}, nil
}

// setTools swaps in a new generation and, if a change callback is installed,
// invokes it (outside the lock) with the new set.
func (c *Client) setTools(defs []hx.ToolDefinition) {
	c.genMu.Lock()
	c.tools = defs
	cb := c.onChange
	c.genMu.Unlock()
	if cb != nil {
		cb(defs)
	}
}

// call issues one request and blocks until its reply arrives, the context is
// canceled, or the client closes.
func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-c.closed:
		return nil, c.readError()
	default:
	}

	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan rpcResponse, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.writeMessage(rpcRequest{JSONRPC: jsonRPCVersion, ID: id, Method: method, Params: params}); err != nil {
		c.drop(id)
		return nil, err
	}

	select {
	case <-ctx.Done():
		c.drop(id)
		return nil, ctx.Err()
	case <-c.closed:
		c.drop(id)
		return nil, c.readError()
	case resp := <-ch:
		if resp.Err != nil {
			return nil, resp.Err
		}
		return resp.Result, nil
	}
}

// drop forgets a pending request (its reply, if it still arrives, is discarded).
func (c *Client) drop(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// writeMessage marshals v to one newline-terminated JSON frame and writes it
// under the write lock.
func (c *Client) writeMessage(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.t.Write(b)
	return err
}

// readLoop drains the transport line by line until it errors or closes, then
// shuts the client down so pending calls unblock.
func (c *Client) readLoop() {
	sc := bufio.NewScanner(c.t)
	sc.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	for sc.Scan() {
		line := sc.Bytes()
		if strings.TrimSpace(string(line)) == "" {
			continue
		}
		// Scanner reuses its buffer across Scan calls; copy before handing off.
		frame := make([]byte, len(line))
		copy(frame, line)
		c.dispatch(frame)
	}
	err := sc.Err()
	if err == nil {
		err = io.EOF
	}
	c.shutdown(err)
}

// dispatch routes one inbound frame: a reply (id, no method) to its waiting call,
// a notification (method, no id) to the change handler. Server-to-client requests
// and unrecognized frames are ignored.
func (c *Client) dispatch(frame []byte) {
	var env struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	if err := json.Unmarshal(frame, &env); err != nil {
		return
	}
	switch {
	case env.Method == "" && len(env.ID) > 0:
		var id int64
		if err := json.Unmarshal(env.ID, &id); err != nil {
			return
		}
		c.deliver(id, rpcResponse{Result: env.Result, Err: env.Error})
	case env.Method != "" && len(env.ID) == 0:
		c.handleNotification(env.Method)
	}
}

// deliver hands a settled response to its waiting caller. The pending channel is
// buffered (size 1), so this never blocks and never depends on the caller.
func (c *Client) deliver(id int64, resp rpcResponse) {
	c.mu.Lock()
	ch, ok := c.pending[id]
	if ok {
		delete(c.pending, id)
	}
	c.mu.Unlock()
	if ok {
		ch <- resp
	}
}

// handleNotification re-lists on tools/list_changed. The re-list runs on a fresh
// goroutine, never inline: it awaits a tools/list reply that THIS read loop must
// read, so listing inline would deadlock.
func (c *Client) handleNotification(method string) {
	if method != notifyToolsChanged {
		return
	}
	go c.relist()
}

// relist fetches a fresh generation and replaces the current one. A fetch failure
// leaves the previous generation in place. It runs with a background context;
// fetchTools' own call selects on c.closed, so a stuck re-list unblocks on Close.
func (c *Client) relist() {
	defs, err := c.fetchTools(context.Background())
	if err != nil {
		return
	}
	c.setTools(defs)
}

// shutdown records the cause, closes the transport, and closes c.closed exactly
// once — whether triggered by Close or by the read loop ending.
func (c *Client) shutdown(cause error) {
	c.closeOnce.Do(func() {
		c.errMu.Lock()
		c.readErr = cause
		c.errMu.Unlock()
		c.closeErr = c.t.Close()
		close(c.closed)
	})
}

// readError reports why the client is closed, for a call that lost its transport.
func (c *Client) readError() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	if c.readErr != nil {
		return c.readErr
	}
	return errClosed
}

// toolErr builds an isError ToolResult whose Value is a JSON object carrying the
// message, so a downstream reader always gets valid JSON — never a panic.
func toolErr(format string, a ...any) hx.ToolResult {
	b, _ := json.Marshal(struct {
		Error string `json:"error"`
	}{Error: fmt.Sprintf(format, a...)})
	return hx.ToolResult{IsError: true, Value: b}
}
