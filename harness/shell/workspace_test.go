package shell

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/websines/structor-harness/harness/hx"
	hsandbox "github.com/websines/structor-harness/harness/sandbox"
	"github.com/websines/structor-harness/harness/subprocess"
)

func TestBashToolBindsWorkspace(t *testing.T) {
	sub := &fakeSub{}
	dir := t.TempDir()
	def := BashToolAt(New(sub, &fakeSandbox{}), dir)
	if _, err := def.Execute(hx.ToolRunContext{Ctx: context.Background()}, []byte(`{"command":"pwd"}`)); err != nil {
		t.Fatal(err)
	}
	if sub.got.Dir != dir {
		t.Fatalf("shell lost its composed workspace: %+v", sub.got)
	}
}

func TestBashToolRealWorkspaceAndBoundary(t *testing.T) {
	sb := hsandbox.New()
	if !sb.Available() {
		t.Skip("real confined shell requires an available OS mechanism")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "forbidden")
	def := BashToolAt(New(subprocess.New(sb), sb), dir)
	args, err := json.Marshal(map[string]any{"command": "pwd\nprintf 'workspace' > allowed.txt\nprintf 'escape' > '" + strings.ReplaceAll(outside, "'", "'\\''") + "'"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := def.Execute(hx.ToolRunContext{Ctx: context.Background()}, args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(res.Value), dir) {
		t.Fatalf("actual shell used another cwd: %s", res.Value)
	}
	data, err := os.ReadFile(filepath.Join(dir, "allowed.txt"))
	if err != nil || string(data) != "workspace" {
		t.Fatalf("declared workspace write failed: %q, %v; %s", data, err, res.Value)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("out-of-scope file exists or cannot be inspected: %v", err)
	}
}
