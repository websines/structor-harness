package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/websines/structor-harness/harness/hx"
)

// The execution pipeline is an EXPLICIT MIDDLEWARE CHAIN, not an event bus
// (design §3). Execute builds one base ToolExecFn (lookup → timeout → validate →
// dispatch) and folds the registered []hx.ToolMiddleware over it: the first
// registered middleware ends up outermost, so a guard registered first can
// short-circuit before any later middleware or the tool body runs.

// Use registers a middleware and returns its disposer. Middleware wrap tool
// dispatch in registration order (first = outermost). Guards, timeouts, and
// metrics live here — the chain is the only extension seam, by design.
func (r *Runtime) Use(mw hx.ToolMiddleware) (dispose func()) {
	r.mu.Lock()
	r.seq++
	reg := &mwReg{mw: mw, id: r.seq}
	r.middleware = append(r.middleware, reg)
	r.mu.Unlock()

	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		for i, m := range r.middleware {
			if m.id == reg.id {
				r.middleware = append(r.middleware[:i], r.middleware[i+1:]...)
				return
			}
		}
	}
}

// Execute runs one call through the pipeline. It snapshots the middleware under
// the read lock, wraps the base dispatch fn with them (first registered =
// outermost), and calls the outer fn. Never returns an error — every failure is
// a ToolResult{IsError:true} so replay stays a total function of the log.
func (r *Runtime) Execute(in hx.ToolExecutionInput) hx.ToolResult {
	r.mu.RLock()
	mws := make([]hx.ToolMiddleware, len(r.middleware))
	for i, m := range r.middleware {
		mws[i] = m.mw
	}
	r.mu.RUnlock()

	fn := hx.ToolExecFn(r.baseExec)
	for i := len(mws) - 1; i >= 0; i-- {
		fn = mws[i](fn)
	}
	return fn(in)
}

// baseExec is the innermost dispatch fn the middleware chain wraps. It (a) looks
// up the tool, (b) applies a per-call timeout, (c) validates the args are JSON,
// and (d) calls the tool body, translating a returned error into an error result.
func (r *Runtime) baseExec(in hx.ToolExecutionInput) hx.ToolResult {
	// (a) lookup — an unknown name is a non-fatal error result, not a panic.
	def, ok := r.lookup(in.Name)
	if !ok {
		return errResult("tools: unknown tool %q", in.Name)
	}

	// (b) per-call timeout: the tool's own TimeoutMs, else the runtime default.
	parent := in.Ctx
	if parent == nil {
		parent = context.Background()
	}
	timeout := r.defaultTimeout
	if def.TimeoutMs > 0 {
		timeout = time.Duration(def.TimeoutMs) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	// (c) validate args are JSON. Empty args (a no-arg call) normalize to null.
	args := in.Args
	if len(args) == 0 {
		args = []byte("null")
	}
	if !json.Valid(args) {
		return errResult("tools: tool %q got invalid JSON arguments", in.Name)
	}

	// (d) dispatch. A returned error becomes an error result; a set EvidenceHint
	// is preserved onto that result so the evidence bridge still sees it.
	rc := hx.ToolRunContext{Ctx: ctx, CallID: in.CallID, Initiator: in.Initiator}
	res, err := def.Execute(rc, args)
	if err != nil {
		out := errResult("%v", err)
		out.EvidenceHint = res.EvidenceHint
		return out
	}
	return res
}

// errResult builds a model-visible error ToolResult whose Value is a JSON object
// carrying the message (so a downstream reader always gets valid JSON).
func errResult(format string, a ...any) hx.ToolResult {
	msg := fmt.Sprintf(format, a...)
	b, _ := json.Marshal(struct {
		Error string `json:"error"`
	}{Error: msg})
	return hx.ToolResult{IsError: true, Value: b}
}
