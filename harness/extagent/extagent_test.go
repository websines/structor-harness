package extagent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSupportedListsKnownAgents(t *testing.T) {
	got := Supported()
	want := map[string]bool{"claude-code": true, "codex": true, "opencode": true}
	if len(got) != len(want) {
		t.Fatalf("Supported() = %v, want the 3 known agents", got)
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("unexpected agent %q in Supported()", g)
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Fatalf("Supported() not sorted: %v", got)
		}
	}
}

func TestAvailableUnknownIsFalse(t *testing.T) {
	if Available("definitely-not-an-agent") {
		t.Error("Available should be false for an unknown agent")
	}
}

func TestBuilderUnknownAgentErrors(t *testing.T) {
	if _, err := Builder("nope", t.TempDir()); err == nil {
		t.Fatal("Builder should error on an unknown agent")
	}
}

func TestAliasResolvesToCanonical(t *testing.T) {
	// "claude" is an alias for "claude-code": they resolve to the same availability.
	if Available("claude") != Available("claude-code") {
		t.Error("alias 'claude' should resolve to 'claude-code'")
	}
}

func TestBuilderConsistentWithAvailable(t *testing.T) {
	// For each known agent, Builder succeeds iff its binary is on PATH (Available) — so
	// the run fails fast with a clear message rather than mid-loop.
	for _, name := range Supported() {
		_, err := Builder(name, t.TempDir())
		switch {
		case Available(name) && err != nil:
			t.Errorf("Builder(%q) errored though Available=true: %v", name, err)
		case !Available(name) && err == nil:
			t.Errorf("Builder(%q) succeeded though its binary is absent", name)
		}
	}
}

// TestBuilderRunsRealAgent exercises the real subprocess when an agent CLI is present,
// skipping cleanly otherwise (the cosign/oras/prep adapters' skip-when-absent convention).
// Opt in with FORGE_TEST_AGENT=claude-code|codex|opencode.
func TestBuilderRunsRealAgent(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: skipping real-agent subprocess")
	}
	name := os.Getenv("FORGE_TEST_AGENT")
	if name == "" || !Available(name) {
		t.Skip("set FORGE_TEST_AGENT to an installed agent to run the live subprocess")
	}
	dir := t.TempDir()
	build, err := Builder(name, dir)
	if err != nil {
		t.Fatalf("Builder: %v", err)
	}
	if _, err := build(context.Background(),
		"Create a file named marker.txt in the working directory containing exactly: ok", "test"); err != nil {
		t.Fatalf("build turn: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "marker.txt")); err != nil {
		t.Errorf("agent did not create marker.txt in the workspace: %v", err)
	}
}
