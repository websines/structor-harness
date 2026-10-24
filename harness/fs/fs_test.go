package fs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/websines/structor-harness/harness/hx"
)

// --- capability (Local) tests ------------------------------------------------

// A new file may be created without a prior read, and reads back verbatim.
func TestWriteNewThenRead(t *testing.T) {
	l := &Local{Root: t.TempDir()}
	const want = "hello\nworld\n"
	if err := l.WriteText("a.txt", want); err != nil {
		t.Fatalf("WriteText(new): %v", err)
	}
	got, err := l.ReadText("a.txt")
	if err != nil {
		t.Fatalf("ReadText: %v", err)
	}
	if got != want {
		t.Fatalf("ReadText = %q, want %q", got, want)
	}
}

// EditText replaces the single unique occurrence.
func TestEditUniqueOccurrence(t *testing.T) {
	l := &Local{Root: t.TempDir()}
	if err := l.WriteText("c.txt", "alpha BETA gamma\n"); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	if err := l.EditText("c.txt", "BETA", "delta"); err != nil {
		t.Fatalf("EditText: %v", err)
	}
	got, err := l.ReadText("c.txt")
	if err != nil {
		t.Fatalf("ReadText: %v", err)
	}
	if want := "alpha delta gamma\n"; got != want {
		t.Fatalf("after edit = %q, want %q", got, want)
	}
}

// EditText on zero matches is a "not found" error.
func TestEditZeroMatchesErrors(t *testing.T) {
	l := &Local{Root: t.TempDir()}
	if err := l.WriteText("c.txt", "only this\n"); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	err := l.EditText("c.txt", "absent", "x")
	if err == nil {
		t.Fatalf("expected error on zero matches")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("error = %v, want it to mention 'not found'", err)
	}
}

// EditText on more than one match is an "ambiguous" error and does not write.
func TestEditAmbiguousErrors(t *testing.T) {
	l := &Local{Root: t.TempDir()}
	const original = "x and x\n"
	if err := l.WriteText("c.txt", original); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	err := l.EditText("c.txt", "x", "y")
	if err == nil {
		t.Fatalf("expected error on ambiguous match")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("error = %v, want it to mention 'ambiguous'", err)
	}
	// The file must be untouched by a refused edit.
	got, err := l.ReadText("c.txt")
	if err != nil {
		t.Fatalf("ReadText: %v", err)
	}
	if got != original {
		t.Fatalf("ambiguous edit mutated file: %q", got)
	}
}

// Overwriting an existing-but-unread file is refused; reading it first unblocks
// the write.
func TestReadBeforeWrite(t *testing.T) {
	root := t.TempDir()
	l := &Local{Root: root}

	// Create the file on disk WITHOUT going through this capability, so it exists
	// but has never been read.
	if err := os.WriteFile(filepath.Join(root, "d.txt"), []byte("orig\n"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	if err := l.WriteText("d.txt", "clobber\n"); err == nil {
		t.Fatalf("expected refusal writing an existing unread file")
	}

	if _, err := l.ReadText("d.txt"); err != nil {
		t.Fatalf("ReadText: %v", err)
	}
	if err := l.WriteText("d.txt", "clobber\n"); err != nil {
		t.Fatalf("WriteText after read should succeed: %v", err)
	}
	got, err := l.ReadText("d.txt")
	if err != nil {
		t.Fatalf("ReadText: %v", err)
	}
	if got != "clobber\n" {
		t.Fatalf("content = %q, want %q", got, "clobber\n")
	}
}

// A path escaping Root via `..` is refused for every operation.
func TestPathEscapeRefused(t *testing.T) {
	l := &Local{Root: t.TempDir()}
	if _, err := l.ReadText("../escape.txt"); err == nil {
		t.Fatalf("ReadText should refuse an escaping path")
	}
	if err := l.WriteText("../escape.txt", "x"); err == nil {
		t.Fatalf("WriteText should refuse an escaping path")
	}
	if err := l.EditText("../escape.txt", "a", "b"); err == nil {
		t.Fatalf("EditText should refuse an escaping path")
	}
	if _, err := l.ReadText("sub/../../escape.txt"); err == nil {
		t.Fatalf("ReadText should refuse a nested escaping path")
	}
}

// An atomic write leaves no temp file behind, only the target.
func TestAtomicWriteNoDebris(t *testing.T) {
	root := t.TempDir()
	l := &Local{Root: root}
	if err := l.WriteText("e.txt", "content\n"); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		names := make([]string, len(entries))
		for i, en := range entries {
			names[i] = en.Name()
		}
		t.Fatalf("expected exactly one file, got %v", names)
	}
	if entries[0].Name() != "e.txt" {
		t.Fatalf("unexpected file %q left behind", entries[0].Name())
	}
}

// --- tool tests --------------------------------------------------------------

// fakeRuntime is a minimal hx.ToolRuntime that captures Register calls so a test
// can invoke a registered tool's body directly.
type fakeRuntime struct {
	defs map[string]hx.ToolDefinition
}

func newFakeRuntime() *fakeRuntime { return &fakeRuntime{defs: map[string]hx.ToolDefinition{}} }

func (f *fakeRuntime) Register(def hx.ToolDefinition) (func(), error) {
	name := def.Schema.Name
	f.defs[name] = def
	return func() { delete(f.defs, name) }, nil
}

func (f *fakeRuntime) Execute(in hx.ToolExecutionInput) hx.ToolResult {
	def, ok := f.defs[in.Name]
	if !ok {
		return hx.ToolResult{IsError: true}
	}
	res, err := def.Execute(hx.ToolRunContext{Ctx: context.Background(), CallID: in.CallID}, in.Args)
	if err != nil {
		return hx.ToolResult{IsError: true}
	}
	return res
}

func (f *fakeRuntime) Schemas() []hx.ToolSchema {
	out := make([]hx.ToolSchema, 0, len(f.defs))
	for _, d := range f.defs {
		out = append(out, d.Schema)
	}
	return out
}

func (f *fakeRuntime) Concurrent(name string) bool {
	d, ok := f.defs[name]
	return ok && d.Concurrent
}

var _ hx.ToolRuntime = (*fakeRuntime)(nil)

// RegisterTools installs exactly the three file tools, and its disposer removes
// them all.
func TestRegisterToolsInstallsThree(t *testing.T) {
	fs := &Local{Root: t.TempDir()}
	tr := newFakeRuntime()
	dispose, err := RegisterTools(fs, tr)
	if err != nil {
		t.Fatalf("RegisterTools: %v", err)
	}
	for _, name := range []string{"read_file", "write_file", "edit_file"} {
		if _, ok := tr.defs[name]; !ok {
			t.Fatalf("tool %q not registered", name)
		}
	}
	dispose()
	if len(tr.defs) != 0 {
		t.Fatalf("dispose left %d tools registered", len(tr.defs))
	}
}

// The write/read/edit tools flow through the runtime and return JSON results.
func TestToolsHappyPath(t *testing.T) {
	fs := &Local{Root: t.TempDir()}
	tr := newFakeRuntime()
	if _, err := RegisterTools(fs, tr); err != nil {
		t.Fatalf("RegisterTools: %v", err)
	}

	// write a new file
	res := tr.Execute(hx.ToolExecutionInput{Name: "write_file", Args: []byte(`{"path":"t.txt","content":"one two two\n"}`)})
	if res.IsError {
		t.Fatalf("write_file errored: %s", res.Value)
	}

	// read it back
	res = tr.Execute(hx.ToolExecutionInput{Name: "read_file", Args: []byte(`{"path":"t.txt"}`)})
	if res.IsError {
		t.Fatalf("read_file errored: %s", res.Value)
	}
	var rd struct {
		Content   string `json:"content"`
		Bytes     int    `json:"bytes"`
		Truncated bool   `json:"truncated"`
	}
	if err := json.Unmarshal(res.Value, &rd); err != nil {
		t.Fatalf("read_file result not JSON: %v", err)
	}
	if rd.Content != "one two two\n" || rd.Truncated {
		t.Fatalf("read_file returned %+v", rd)
	}

	// edit the unique occurrence of "one"
	res = tr.Execute(hx.ToolExecutionInput{Name: "edit_file", Args: []byte(`{"path":"t.txt","old_string":"one","new_string":"1"}`)})
	if res.IsError {
		t.Fatalf("edit_file errored: %s", res.Value)
	}
	got, err := fs.ReadText("t.txt")
	if err != nil {
		t.Fatalf("ReadText: %v", err)
	}
	if got != "1 two two\n" {
		t.Fatalf("after edit_file = %q", got)
	}
}

// A filesystem error surfaces as ToolResult{IsError:true} carrying the message —
// never a panic, never a raw Go error.
func TestToolsSurfaceFsErrors(t *testing.T) {
	fs := &Local{Root: t.TempDir()}
	tr := newFakeRuntime()
	if _, err := RegisterTools(fs, tr); err != nil {
		t.Fatalf("RegisterTools: %v", err)
	}

	// edit a missing file -> read error, surfaced as an error result.
	res := tr.Execute(hx.ToolExecutionInput{Name: "edit_file", Args: []byte(`{"path":"missing.txt","old_string":"a","new_string":"b"}`)})
	if !res.IsError {
		t.Fatalf("edit_file on missing file should be an error result")
	}
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(res.Value, &e); err != nil {
		t.Fatalf("error result not JSON: %v", err)
	}
	if e.Error == "" {
		t.Fatalf("error result carried no message")
	}

	// ambiguous edit -> error result mentioning "ambiguous".
	if err := fs.WriteText("amb.txt", "x x\n"); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	res = tr.Execute(hx.ToolExecutionInput{Name: "edit_file", Args: []byte(`{"path":"amb.txt","old_string":"x","new_string":"y"}`)})
	if !res.IsError {
		t.Fatalf("ambiguous edit_file should be an error result")
	}
	_ = json.Unmarshal(res.Value, &e)
	if !strings.Contains(e.Error, "ambiguous") {
		t.Fatalf("error = %q, want it to mention 'ambiguous'", e.Error)
	}

	// invalid JSON arguments -> error result, not a panic.
	res = tr.Execute(hx.ToolExecutionInput{Name: "write_file", Args: []byte(`{not json`)})
	if !res.IsError {
		t.Fatalf("invalid args should be an error result")
	}
}
