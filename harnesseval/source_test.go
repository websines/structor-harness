package harnesseval

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/harness/shell"
	"github.com/websines/structor-harness/harness/subprocess"
)

func TestCaptureAfterNativeShellCheck(t *testing.T) {
	root := t.TempDir()
	source := Source{"main.go": "package fixture\nfunc Value()int{return 1}\n", "public_test.go": "package fixture\nimport \"testing\"\nfunc TestPublic(t *testing.T){if Value()!=1{t.Fatal(\"value\")}}\n"}
	if err := source.Materialize(root); err != nil {
		t.Fatal(err)
	}
	if err := seedToolchain(t.Context(), root, ".forge/tool-cache/go"); err != nil {
		t.Fatal(err)
	}
	env, err := shell.WorkspaceEnvironmentFrom(root, Environment(root))
	if err != nil {
		t.Fatal(err)
	}
	confiner, err := NewConfiner(root)
	if err != nil {
		t.Fatal(err)
	}
	run := subprocess.New(confiner).Spawn(hx.SpawnRequest{Ctx: t.Context(), Argv: []string{filepath.Join(runtime.GOROOT(), "bin", "go"), "test", "./..."}, Dir: root, Env: env, TimeoutMs: 60000})
	if run.Err != "" || run.ExitCode != 0 || run.TimedOut {
		t.Fatalf("native check: %+v", run)
	}
	captured, err := Capture(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(captured) != len(source) {
		t.Fatalf("captured runtime files: %v", captured)
	}
	for name, text := range source {
		if captured[name] != text {
			t.Fatalf("source changed: %s", name)
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".forge")); err != nil {
		t.Fatal("test did not exercise a runtime cache")
	}
}

func TestProtectedSourceRequiresOriginalFilesIncludingEmptyOnes(t *testing.T) {
	original := Source{"main.go": "code", "public_test.go": "trusted", "go.sum": ""}
	for _, tc := range []struct {
		name      string
		candidate Source
		valid     bool
	}{
		{"source edit", Source{"main.go": "changed", "public_test.go": "trusted", "go.sum": ""}, true},
		{"test edit", Source{"main.go": "code", "public_test.go": "changed", "go.sum": ""}, false},
		{"empty dependency deleted", Source{"main.go": "code", "public_test.go": "trusted"}, false},
		{"reserved test", Source{"main.go": "code", "public_test.go": "trusted", "go.sum": "", "forge_acceptance_test.go": "replacement"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if valid := protectedSource(original, tc.candidate) == ""; valid != tc.valid {
				t.Fatalf("integrity result: valid=%v", valid)
			}
		})
	}
}
