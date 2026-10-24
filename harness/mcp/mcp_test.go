package mcp

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/harness/tools"
	"github.com/websines/structor-harness/runtime"
)

// The tests drive a real Client over a net.Pipe against an in-process fake MCP
// server (a goroutine reading JSON-RPC requests off the other pipe end). No
// subprocess, no timers beyond the context deadlines that make a hang fail fast.

// fakeTool is one tool the fake server advertises.
type fakeTool struct {
	name        string
	description string
	schema      map[string]any
}

// fakeServer answers initialize, tools/list, and tools/call over conn. Its
// advertised tool set is swappable so a test can simulate list_changed.
type fakeServer struct {
	conn  net.Conn
	mu    sync.Mutex
	tools []fakeTool
	wmu   sync.Mutex
}

// newFakeServer starts a fake server serving initial over conn.
func newFakeServer(conn net.Conn, initial []fakeTool) *fakeServer {
	s := &fakeServer{conn: conn, tools: initial}
	go s.serve()
	return s
}

func (s *fakeServer) setTools(ts []fakeTool) {
	s.mu.Lock()
	s.tools = ts
	s.mu.Unlock()
}

func (s *fakeServer) snapshot() []fakeTool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]fakeTool, len(s.tools))
	copy(out, s.tools)
	return out
}

// serve reads newline-delimited JSON-RPC frames and answers each request.
// Notifications (no id) — e.g. the client's initialized — are ignored.
func (s *fakeServer) serve() {
	sc := bufio.NewScanner(s.conn)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if strings.TrimSpace(string(line)) == "" {
			continue
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}
		if len(req.ID) == 0 {
			continue // client notification
		}
		s.handle(req.ID, req.Method, req.Params)
	}
}

func (s *fakeServer) handle(id json.RawMessage, method string, params json.RawMessage) {
	switch method {
	case "initialize":
		s.reply(id, map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": true}},
			"serverInfo":      map[string]any{"name": "fake", "version": "0.0.1"},
		})
	case "tools/list":
		ts := s.snapshot()
		list := make([]map[string]any, 0, len(ts))
		for _, t := range ts {
			schema := t.schema
			if schema == nil {
				schema = map[string]any{"type": "object"}
			}
			list = append(list, map[string]any{
				"name":        t.name,
				"description": t.description,
				"inputSchema": schema,
			})
		}
		s.reply(id, map[string]any{"tools": list})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		_ = json.Unmarshal(params, &p)
		args := p.Arguments
		if len(args) == 0 {
			args = json.RawMessage("{}")
		}
		// Echo the arguments back as the single text content block.
		s.reply(id, map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": string(args)},
			},
			"isError": false,
		})
	default:
		s.replyErr(id, -32601, "method not found: "+method)
	}
}

func (s *fakeServer) reply(id json.RawMessage, result any) {
	s.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (s *fakeServer) replyErr(id json.RawMessage, code int, msg string) {
	s.write(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}})
}

func (s *fakeServer) notify(method string) {
	s.write(map[string]any{"jsonrpc": "2.0", "method": method})
}

func (s *fakeServer) write(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	b = append(b, '\n')
	s.wmu.Lock()
	_, _ = s.conn.Write(b)
	s.wmu.Unlock()
}

// testContext returns a context that fails a hung test fast.
func testContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), 5*time.Second)
}

func toolNames(defs []hx.ToolDefinition) []string {
	out := make([]string, len(defs))
	for i, d := range defs {
		out[i] = d.Schema.Name
	}
	return out
}

func schemaNames(ss []hx.ToolSchema) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.Name
	}
	return out
}

func hashHex(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])[:12]
}

//  1. Initialize, list a single "echo" tool, and round-trip an argument through
//     its Execute (which calls tools/call).
func TestClient_ListAndCallEcho(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	newFakeServer(serverConn, []fakeTool{{
		name:        "echo",
		description: "echoes its arguments",
		schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"msg": map[string]any{"type": "string"},
			},
		},
	}})
	c := NewClient("fake", clientConn)
	defer c.Close()

	ctx, cancel := testContext(t)
	defer cancel()

	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	defs, err := c.ListTools(ctx)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(defs) != 1 {
		t.Fatalf("want 1 tool, got %d: %v", len(defs), toolNames(defs))
	}
	if got := defs[0].Schema.Name; got != "mcp__fake__echo" {
		t.Fatalf("want name mcp__fake__echo, got %q", got)
	}

	res, err := defs[0].Execute(hx.ToolRunContext{Ctx: ctx}, []byte(`{"msg":"hi"}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", res.Value)
	}

	// Value is the MCP content array; the echo tool put the arguments JSON into
	// its single text block, so "hi" round-trips through it.
	var content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(res.Value, &content); err != nil {
		t.Fatalf("decode content: %v (value=%s)", err, res.Value)
	}
	if len(content) != 1 || content[0].Type != "text" {
		t.Fatalf("unexpected content: %s", res.Value)
	}
	var echoed struct {
		Msg string `json:"msg"`
	}
	if err := json.Unmarshal([]byte(content[0].Text), &echoed); err != nil {
		t.Fatalf("decode echoed args: %v (text=%q)", err, content[0].Text)
	}
	if echoed.Msg != "hi" {
		t.Fatalf("round-trip failed: want hi, got %q", echoed.Msg)
	}
}

//  2. Two tools whose public names collide (distinct raw names normalizing to the
//     same mcp__fake__wiki_search) → the second gets the 12-hex suffix, and the
//     two registered names are distinct.
func TestClient_NameCollisionGetsHashSuffix(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	newFakeServer(serverConn, []fakeTool{
		{name: "wiki.search", description: "a"},
		{name: "wiki/search", description: "b"},
	})
	c := NewClient("fake", clientConn)
	defer c.Close()

	ctx, cancel := testContext(t)
	defer cancel()

	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	defs, err := c.ListTools(ctx)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(defs) != 2 {
		t.Fatalf("want 2 tools, got %d: %v", len(defs), toolNames(defs))
	}
	n0, n1 := defs[0].Schema.Name, defs[1].Schema.Name
	if n0 == n1 {
		t.Fatalf("names collided: both %q", n0)
	}
	if n0 != "mcp__fake__wiki_search" {
		t.Fatalf("first tool: want clean name mcp__fake__wiki_search, got %q", n0)
	}
	wantSuffix := "_" + hashHex("wiki/search")
	if !strings.HasSuffix(n1, wantSuffix) {
		t.Fatalf("second tool: want hash suffix %q, got %q", wantSuffix, n1)
	}
}

//  3. A tools/list_changed notification re-lists and replaces the whole
//     generation; Tools() reflects the new set.
func TestClient_ListChangedReplacesGeneration(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	srv := newFakeServer(serverConn, []fakeTool{{name: "echo", description: "e"}})
	c := NewClient("fake", clientConn)
	defer c.Close()

	ctx, cancel := testContext(t)
	defer cancel()

	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if _, err := c.ListTools(ctx); err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if got := toolNames(c.Tools()); len(got) != 1 || got[0] != "mcp__fake__echo" {
		t.Fatalf("initial generation wrong: %v", got)
	}

	// Swap the server's tools, then signal the change.
	srv.setTools([]fakeTool{
		{name: "search", description: "s"},
		{name: "fetch", description: "f"},
	})
	srv.notify("notifications/tools/list_changed")

	// The client re-lists off its read loop; poll until Tools() reflects it.
	deadline := time.Now().Add(5 * time.Second)
	for {
		names := toolNames(c.Tools())
		sort.Strings(names)
		if len(names) == 2 && names[0] == "mcp__fake__fetch" && names[1] == "mcp__fake__search" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("generation not replaced after list_changed: %v", names)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

//  4. Component activation registers the server's tools on the resolved
//     ToolRuntime, and unloading withdraws them (a revertible effect).
func TestComponent_RegistersAndReverts(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	newFakeServer(serverConn, []fakeTool{{name: "echo", description: "e"}})

	reg := runtime.New()
	tr := tools.New(tools.Config{})

	// A provider component publishes the ToolRuntime under hx.KeyTools so the
	// mcp component's injected dependency is satisfied.
	reg.Load(&runtime.Component{
		Name:    "tools-provider",
		Provide: []string{hx.KeyTools},
		Apply: func(ctx *runtime.Context) error {
			return ctx.Provide(hx.KeyTools, tr)
		},
	}, "")

	reg.Load(Component("fake", clientConn), "")

	f := reg.Fiber("harness/mcp/fake")
	if f == nil || f.State() != runtime.StateActive {
		t.Fatalf("mcp component did not activate: %v", f)
	}
	if got := schemaNames(tr.Schemas()); len(got) != 1 || got[0] != "mcp__fake__echo" {
		t.Fatalf("want mcp__fake__echo registered, got %v", got)
	}

	reg.Remove("harness/mcp/fake")
	if got := tr.Schemas(); len(got) != 0 {
		t.Fatalf("tools not withdrawn on unload: %v", schemaNames(got))
	}
}
