package sandbox

import (
	"reflect"
	"strings"
	"testing"

	"github.com/websines/structor-harness/harness/hx"
)

// forced constructs a Local with an injected mechanism so the test is
// deterministic regardless of what the CI host actually resolves on PATH.
func forced(goos, wrapper string) *Local { return &Local{goos: goos, wrapper: wrapper} }

// TestConfineDarwinAvailable asserts the fully-built degraded path: on darwin
// with sandbox-exec present, Confine prefixes "sandbox-exec -p <profile>" and the
// profile denies by default, allows the read/write paths, and (with AllowNetwork
// false) does NOT allow network.
func TestConfineDarwinAvailable(t *testing.T) {
	l := forced("darwin", seatbeltTool)
	if !l.Available() {
		t.Fatal("Available() = false, want true for a forced darwin mechanism")
	}

	argv := []string{"/bin/echo", "hello"}
	policy := hx.SandboxPolicy{
		ReadPaths:    []string{"/repo/src", "/etc/config"},
		WritePaths:   []string{"/repo/out"},
		AllowNetwork: false,
	}
	got, err := l.Confine(argv, policy)
	if err != nil {
		t.Fatalf("Confine returned error: %v", err)
	}

	// Prefix shape: sandbox-exec -p <profile> <argv...>
	if len(got) != 3+len(argv) {
		t.Fatalf("Confine len = %d, want %d; got=%v", len(got), 3+len(argv), got)
	}
	if got[0] != seatbeltTool || got[1] != "-p" {
		t.Fatalf("Confine prefix = %q %q, want %q -p", got[0], got[1], seatbeltTool)
	}
	if tail := got[3:]; !reflect.DeepEqual(tail, argv) {
		t.Fatalf("Confine tail = %v, want original argv %v", tail, argv)
	}

	profile := got[2]
	mustContain := []string{
		"(version 1)",
		"(deny default)",
		"(allow file-read*)", // broad read: a process cannot load without it
		"(allow process-exec)",
		"(allow process-fork)",
		`(allow file-write* (subpath "/repo/out"))`, // writes scoped to the declared path
	}
	for _, s := range mustContain {
		if !strings.Contains(profile, s) {
			t.Errorf("profile missing %q\nprofile: %s", s, profile)
		}
	}
	if strings.Contains(profile, "network") {
		t.Errorf("profile allows network but AllowNetwork was false\nprofile: %s", profile)
	}
}

// TestConfineDarwinAllowNetwork asserts network egress is opened only when the
// policy asks for it.
func TestConfineDarwinAllowNetwork(t *testing.T) {
	l := forced("darwin", seatbeltTool)
	got, err := l.Confine([]string{"/bin/curl"}, hx.SandboxPolicy{AllowNetwork: true})
	if err != nil {
		t.Fatalf("Confine returned error: %v", err)
	}
	if !strings.Contains(got[2], "(allow network*)") {
		t.Errorf("profile missing (allow network*) with AllowNetwork true\nprofile: %s", got[2])
	}
}

// TestConfineUnavailable asserts the honest pass-through: with no mechanism the
// argv is returned UNCHANGED, without error, and is not mutated.
func TestConfineUnavailable(t *testing.T) {
	l := forced("darwin", "") // no wrapper resolved → Available() false
	if l.Available() {
		t.Fatal("Available() = true, want false for an empty mechanism")
	}

	argv := []string{"/bin/echo", "hi"}
	orig := append([]string(nil), argv...)
	got, err := l.Confine(argv, hx.SandboxPolicy{ReadPaths: []string{"/x"}})
	if err != nil {
		t.Fatalf("Confine returned error: %v", err)
	}
	if !reflect.DeepEqual(got, argv) {
		t.Fatalf("Confine = %v, want unchanged argv %v", got, argv)
	}
	if !reflect.DeepEqual(argv, orig) {
		t.Fatalf("Confine mutated its input argv: %v, want %v", argv, orig)
	}
}

// TestConfineLinuxBwrap locks the degraded linux fallback shape: bwrap binds the
// policy paths, unshares the network when egress is denied, and passes the
// original argv after the "--" separator.
func TestConfineLinuxBwrap(t *testing.T) {
	l := forced("linux", bwrapTool)
	if !l.Available() {
		t.Fatal("Available() = false, want true for a forced linux mechanism")
	}

	argv := []string{"/bin/echo", "hi"}
	got, err := l.Confine(argv, hx.SandboxPolicy{
		ReadPaths:  []string{"/repo/src"},
		WritePaths: []string{"/repo/out"},
	})
	if err != nil {
		t.Fatalf("Confine returned error: %v", err)
	}
	if got[0] != bwrapTool {
		t.Fatalf("Confine[0] = %q, want %q", got[0], bwrapTool)
	}
	joined := strings.Join(got, " ")
	for _, s := range []string{"--ro-bind /repo/src /repo/src", "--bind /repo/out /repo/out", "--unshare-net", "--"} {
		if !strings.Contains(joined, s) {
			t.Errorf("bwrap argv missing %q\nargv: %v", s, got)
		}
	}
	if tail := got[len(got)-len(argv):]; !reflect.DeepEqual(tail, argv) {
		t.Fatalf("Confine tail = %v, want original argv %v", tail, argv)
	}
}

// TestConfineLinuxLandlockRunWholly asserts the customer-profile swap: the
// landlock-run C binary is invoked as a whole (prefix only) — the Go side does
// not reinvent its policy plumbing.
func TestConfineLinuxLandlockRunWholly(t *testing.T) {
	l := forced("linux", "/usr/bin/"+landlockRun)
	argv := []string{"/bin/echo", "hi"}
	got, err := l.Confine(argv, hx.SandboxPolicy{ReadPaths: []string{"/x"}})
	if err != nil {
		t.Fatalf("Confine returned error: %v", err)
	}
	want := append([]string{"/usr/bin/" + landlockRun}, argv...)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Confine = %v, want %v", got, want)
	}
}

// TestNewNeverErrors is a host-tolerant smoke test: whatever the CI host
// resolves, New()+Confine must not error, and an unavailable provider must pass
// the argv through unchanged.
func TestNewNeverErrors(t *testing.T) {
	l := New()
	argv := []string{"/bin/echo", "ok"}
	got, err := l.Confine(argv, hx.SandboxPolicy{})
	if err != nil {
		t.Fatalf("Confine returned error: %v", err)
	}
	if !l.Available() && !reflect.DeepEqual(got, argv) {
		t.Fatalf("unavailable provider changed argv: %v, want %v", got, argv)
	}
}
