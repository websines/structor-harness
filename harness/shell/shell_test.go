package shell

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/runtime"
)

// --- in-test fakes for the hx interfaces the shell consumes -------------------

// fakeSub is an hx.Subprocess that records the request it was handed and returns
// a canned result — so a test can assert exactly which argv reached the runner
// without launching a real process.
type fakeSub struct {
	got    hx.SpawnRequest
	called bool
	ret    hx.SpawnResult
}

func (f *fakeSub) Spawn(req hx.SpawnRequest) hx.SpawnResult {
	f.got = req
	f.called = true
	return f.ret
}

// fakeSandbox is an hx.SandboxProvider whose Available() is configurable and whose
// Confine wraps the argv with a visible marker (recording the argv + policy it
// saw), so a test can prove confinement was — or was not — applied.
type fakeSandbox struct {
	available  bool
	confineErr error
	gotArgv    []string
	gotPolicy  hx.SandboxPolicy
	called     bool
}

func (s *fakeSandbox) Available() bool { return s.available }

func (s *fakeSandbox) Confine(argv []string, policy hx.SandboxPolicy) ([]string, error) {
	s.called = true
	s.gotArgv = argv
	s.gotPolicy = policy
	if s.confineErr != nil {
		return nil, s.confineErr
	}
	return append([]string{"sandbox-exec", "-p"}, argv...), nil
}

// fakeTools is a minimal hx.ToolRuntime that only records registered tool names —
// enough to prove the component wired the bash tool in.
type fakeTools struct{ registered []string }

func (f *fakeTools) Register(def hx.ToolDefinition) (func(), error) {
	f.registered = append(f.registered, def.Schema.Name)
	return func() {}, nil
}
func (f *fakeTools) Execute(in hx.ToolExecutionInput) hx.ToolResult { return hx.ToolResult{} }
func (f *fakeTools) Schemas() []hx.ToolSchema                       { return nil }
func (f *fakeTools) Concurrent(name string) bool                    { return false }

// --- Run: argv construction and sandbox composition ---------------------------

func TestRunUnconfinedArgv(t *testing.T) {
	sub := &fakeSub{ret: hx.SpawnResult{ExitCode: 0, Stdout: []byte("hi\n")}}
	sb := &fakeSandbox{available: false}
	b := New(sub, sb)

	res := b.Run(hx.ShellRequest{Command: "echo hi", Dir: "/work", TimeoutMs: 500})

	want := []string{"bash", "-c", "echo hi"}
	if !reflect.DeepEqual(sub.got.Argv, want) {
		t.Fatalf("argv = %q, want %q", sub.got.Argv, want)
	}
	if sb.called {
		t.Fatalf("Confine must not be called when the sandbox is unavailable")
	}
	// And it must not CLAIM to have confined: the empty declaration is what makes
	// the subprocess capability apply its own (fail-closed) decision instead of
	// taking this executor's word for an argv nothing wrapped.
	if sub.got.Confinement != "" {
		t.Fatalf("Confinement = %q, want empty when nothing was confined", sub.got.Confinement)
	}
	if sub.got.Dir != "/work" || sub.got.TimeoutMs != 500 {
		t.Fatalf("Dir/TimeoutMs not passed through: %+v", sub.got)
	}
	if res.ExitCode != 0 || string(res.Stdout) != "hi\n" {
		t.Fatalf("result not returned verbatim: %+v", res)
	}
}

func TestRunConfinedArgvWhenAvailable(t *testing.T) {
	sub := &fakeSub{}
	sb := &fakeSandbox{available: true}
	b := New(sub, sb)

	b.Run(hx.ShellRequest{Command: "echo hi", Dir: "/work"})

	// The CONFINED (wrapped) argv is what reaches the subprocess.
	want := []string{"sandbox-exec", "-p", "bash", "-c", "echo hi"}
	if !reflect.DeepEqual(sub.got.Argv, want) {
		t.Fatalf("argv = %q, want confined %q", sub.got.Argv, want)
	}
	// And Confine saw the plain argv plus a conservative policy.
	if !reflect.DeepEqual(sb.gotArgv, []string{"bash", "-c", "echo hi"}) {
		t.Fatalf("Confine got argv %q", sb.gotArgv)
	}
	if sb.gotPolicy.AllowNetwork {
		t.Fatalf("policy must deny network")
	}
	// Read the working dir; write the working dir and /dev/null (a (deny default)
	// profile otherwise denies `2>/dev/null` and kills commands that were fine).
	if !reflect.DeepEqual(sb.gotPolicy.ReadPaths, []string{"/work"}) ||
		!reflect.DeepEqual(sb.gotPolicy.WritePaths, []string{"/dev/null", "/work"}) {
		t.Fatalf("policy must read+write only the working dir (+/dev/null): %+v", sb.gotPolicy)
	}
	// The declaration tells the spawner not to wrap an already-wrapped argv.
	if sub.got.Confinement != hx.ConfineCaller {
		t.Fatalf("Confinement = %q, want %q", sub.got.Confinement, hx.ConfineCaller)
	}
}

func TestRunConfineFailureIsAnErr(t *testing.T) {
	sub := &fakeSub{}
	sb := &fakeSandbox{available: true, confineErr: errors.New("boom")}
	b := New(sub, sb)

	res := b.Run(hx.ShellRequest{Command: "echo hi", Dir: "/work"})

	if res.Err == "" {
		t.Fatalf("a Confine failure must surface as a non-empty Err")
	}
	if sub.called {
		t.Fatalf("the subprocess must not run when confinement fails")
	}
}

func TestRunNilSandboxRunsUnconfined(t *testing.T) {
	sub := &fakeSub{}
	b := New(sub, nil)

	b.Run(hx.ShellRequest{Command: "echo hi"})

	want := []string{"bash", "-c", "echo hi"}
	if !reflect.DeepEqual(sub.got.Argv, want) {
		t.Fatalf("argv = %q, want %q", sub.got.Argv, want)
	}
	if sub.got.Confinement != "" {
		t.Fatalf("Confinement = %q, want empty with no sandbox wired", sub.got.Confinement)
	}
}

// --- bash tool ----------------------------------------------------------------

func TestBashToolSurfacesExitAndStdout(t *testing.T) {
	sub := &fakeSub{ret: hx.SpawnResult{ExitCode: 3, Stdout: []byte("out"), Stderr: []byte("err")}}
	def := BashTool(New(sub, &fakeSandbox{}))

	res, err := def.Execute(hx.ToolRunContext{Ctx: context.Background()}, []byte(`{"command":"exit 3"}`))
	if err != nil {
		t.Fatalf("Execute returned an error: %v", err)
	}
	if res.IsError {
		t.Fatalf("a non-zero exit is DATA, not an IsError result")
	}

	var out bashResult
	if err := json.Unmarshal(res.Value, &out); err != nil {
		t.Fatalf("result value is not JSON: %v", err)
	}
	if out.ExitCode != 3 || out.Stdout != "out" || out.Stderr != "err" {
		t.Fatalf("result did not surface exit/stdout/stderr: %+v", out)
	}
}

func TestBashToolMarksSpawnFailureAsError(t *testing.T) {
	sub := &fakeSub{ret: hx.SpawnResult{ExitCode: -1, Err: "exec: \"nope\": not found"}}
	def := BashTool(New(sub, &fakeSandbox{}))

	res, err := def.Execute(hx.ToolRunContext{Ctx: context.Background()}, []byte(`{"command":"nope"}`))
	if err != nil {
		t.Fatalf("Execute returned an error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("a spawn failure must be reported as ToolResult{IsError:true}")
	}
	var out bashResult
	if err := json.Unmarshal(res.Value, &out); err != nil {
		t.Fatalf("result value is not JSON: %v", err)
	}
	if out.Error == "" {
		t.Fatalf("error result must carry the spawn error message")
	}
}

func TestBashToolPassesCommandAndTimeout(t *testing.T) {
	sub := &fakeSub{}
	def := BashTool(New(sub, &fakeSandbox{}))

	if _, err := def.Execute(hx.ToolRunContext{Ctx: context.Background()},
		[]byte(`{"command":"ls -la","timeout_ms":250}`)); err != nil {
		t.Fatalf("Execute returned an error: %v", err)
	}
	if want := []string{"bash", "-c", "ls -la"}; !reflect.DeepEqual(sub.got.Argv, want) {
		t.Fatalf("argv = %q, want %q", sub.got.Argv, want)
	}
	if sub.got.TimeoutMs != 250 {
		t.Fatalf("timeout_ms = %d, want 250", sub.got.TimeoutMs)
	}
}

func TestBashToolRejectsEmptyCommand(t *testing.T) {
	def := BashTool(New(&fakeSub{}, &fakeSandbox{}))
	if _, err := def.Execute(hx.ToolRunContext{Ctx: context.Background()}, []byte(`{}`)); err == nil {
		t.Fatalf("an empty command must be rejected")
	}
}

// --- component composition ----------------------------------------------------

// TestComponentComposesByKey drives the real Component through a runtime.Registry:
// fake subprocess/sandbox/tools providers are loaded under their keys, and the
// shell component must resolve them by key, register the bash tool, and provide a
// *Bash under KeyShell.
func TestComponentComposesByKey(t *testing.T) {
	reg := runtime.New()
	sub := &fakeSub{}
	sb := &fakeSandbox{}
	tr := &fakeTools{}

	provide := func(name, key string, val any) {
		reg.Load(&runtime.Component{
			Name:    name,
			Provide: []string{key},
			Apply:   func(ctx *runtime.Context) error { return ctx.Provide(key, val) },
		}, "")
	}
	provide("test/subprocess", hx.KeySubprocess, hx.Subprocess(sub))
	provide("test/sandbox", hx.KeySandbox, hx.SandboxProvider(sb))
	provide("test/tools", hx.KeyTools, hx.ToolRuntime(tr))

	reg.Load(Component(), "")

	v, ok := reg.Lookup(hx.KeyShell)
	if !ok {
		t.Fatalf("shell component did not activate — KeyShell not provided")
	}
	if _, ok := v.(*Bash); !ok {
		t.Fatalf("KeyShell = %T, want *Bash", v)
	}
	if !reflect.DeepEqual(tr.registered, []string{bashToolName}) {
		t.Fatalf("registered tools = %q, want [%q]", tr.registered, bashToolName)
	}
}
