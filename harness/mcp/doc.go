// Package mcp is a Go port of dsh's MCP (Model Context Protocol) client bridge.
// It connects to ONE MCP server over a Transport, runs the JSON-RPC 2.0
// initialize handshake, lists the server's tools, and registers each as a native
// harness tool under a server-qualified name mcp__<server>__<rawName>.
//
// Server-qualified namespacing. Every mapped tool carries two names: the raw MCP
// name sent on the wire in tools/call, and the public name mcp__<server>__<raw>
// registered on the harness ToolRuntime. Namespacing by server means two servers
// that publish the same raw name (say "search") coexist. A collision WITHIN one
// server — two distinct raw names whose public names normalize to the same string
// — is broken by appending "_" plus the first 12 hex characters of
// sha256(rawName), so distinct tools never collapse onto one name.
//
// Generation-replace on list_changed. When the server emits
// notifications/tools/list_changed the client re-lists and REPLACES the whole
// generation (never a partial set); Tools() reflects the latest generation. The
// re-list runs off the read loop, because the read loop is the goroutine that
// must read the tools/list response it would otherwise block awaiting.
//
// Revertible registration. Component wires a Client onto the cordis runtime: it
// injects hx.KeyTools, runs the handshake and the initial listing, and registers
// each tool on the resolved ToolRuntime as a revertible effect — the disposer
// unregisters every tool, and a list_changed re-list swaps the registered
// generation in place. If the initialize handshake fails, the effect returns the
// error and the component does not activate.
//
// Transport is any bidirectional JSON-RPC line channel (an io.ReadWriteCloser).
// Framing defaults to newline-delimited JSON: exactly one JSON-RPC message per
// line. Real stdio MCP servers use this or Content-Length framing, so the framing
// is deliberately isolated to readLoop and writeMessage and easy to swap.
//
// Stdlib only: the package pulls in nothing outside encoding/json, bufio, io,
// sync, fmt, errors, crypto/sha256, encoding/hex, strings, and context.
package mcp
