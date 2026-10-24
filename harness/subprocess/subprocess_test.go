package subprocess

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/websines/structor-harness/harness/hx"
	harnesssandbox "github.com/websines/structor-harness/harness/sandbox"
)

func TestSpawnOutputLimitOverrideIsBounded(t *testing.T) {
	tests := []struct {
		name                          string
		requested, produced, retained int
	}{
		{name: "default", produced: 300000, retained: 256 << 10},
		{name: "negative uses default", requested: -1, produced: 300000, retained: 256 << 10},
		{name: "caller chooses larger capture", requested: 400000, produced: 500000, retained: 400000},
		{name: "caller cannot exceed hard maximum", requested: 1 << 30, produced: 9 << 20, retained: 8 << 20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			script := fmt.Sprintf("head -c %d /dev/zero; head -c %d /dev/zero >&2", tt.produced, tt.produced)
			res := New(nil).Spawn(hx.SpawnRequest{Argv: []string{"/bin/sh", "-c", script}, Confinement: hx.ConfineNone, OutputLimit: tt.requested})
			if res.Err != "" || res.ExitCode != 0 || !res.StdoutTruncated || !res.StderrTruncated {
				t.Fatalf("capture fixture failed: exit=%d error=%s stdoutTruncated=%v stderrTruncated=%v", res.ExitCode, res.Err, res.StdoutTruncated, res.StderrTruncated)
			}
			if res.StdoutBytes != tt.produced || res.StderrBytes != tt.produced || strings.IndexByte(string(res.Stdout), '\n') != tt.retained || strings.IndexByte(string(res.Stderr), '\n') != tt.retained {
				t.Fatalf("capture limit lost accounting: stdout=%d/%d stderr=%d/%d retained=%d", len(res.Stdout), res.StdoutBytes, len(res.Stderr), res.StderrBytes, tt.retained)
			}
		})
	}
}

// --- test doubles ------------------------------------------------------------

// fakeProvider is an hx.SandboxProvider whose Available() is fixed and whose
// Confine REPLACES the argv with a visible marker command. Replacing rather than
// decorating is deliberate: it means a test can tell from the process's own OUTPUT
// whether the confined argv or the raw one was executed, which is what makes the
// confinement assertions mutation-sensitive (delete the Confine call and the raw
// command runs, printing the wrong thing).
type fakeProvider struct {
	available  bool
	confineTo  []string // argv Confine returns; nil = pass the input through
	confineErr error
	called     bool
	gotArgv    []string
	gotPolicy  hx.SandboxPolicy
}

func (f *fakeProvider) Available() bool { return f.available }

func (f *fakeProvider) Confine(argv []string, policy hx.SandboxPolicy) ([]string, error) {
	f.called = true
	f.gotArgv = argv
	f.gotPolicy = policy
	if f.confineErr != nil {
		return nil, f.confineErr
	}
	if f.confineTo != nil {
		return f.confineTo, nil
	}
	return argv, nil
}

// passthrough is the provider the NON-confinement tests (capture, tree-kill, exit
// codes) run under: it reports a mechanism and hands the argv back untouched, so
// those tests exercise the spawner's own invariants on every host — including CI,
// where no real mechanism exists and a fail-closed Local would refuse everything.
func passthrough() *fakeProvider { return &fakeProvider{available: true} }

// alive reports whether pid is still a live process (signal 0 probes existence
// without delivering anything; ESRCH means it is gone).
func alive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// --- process semantics (unchanged behaviour, now under a confinement seam) ----

// TestSpawnEcho: a successful command exits 0 with its output captured, no Err.
func TestSpawnEcho(t *testing.T) {
	res := New(passthrough()).Spawn(hx.SpawnRequest{Argv: []string{"/bin/echo", "hello"}})
	if res.Err != "" {
		t.Fatalf("unexpected Err: %q", res.Err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", res.ExitCode)
	}
	if res.TimedOut {
		t.Fatalf("TimedOut = true, want false")
	}
	if !strings.Contains(string(res.Stdout), "hello") {
		t.Fatalf("Stdout = %q, want it to contain %q", res.Stdout, "hello")
	}
}

func TestSpawnReturnsAfterCancellationWatcherStops(t *testing.T) {
	// With one processor, closing the process-completion signal cannot hide an
	// unjoined ready watcher by allowing it to finish on a different processor.
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	for range 8 {
		res := New(nil).Spawn(hx.SpawnRequest{Argv: []string{"/usr/bin/true"}, Confinement: hx.ConfineNone})
		if res.Err != "" || res.ExitCode != 0 {
			t.Fatalf("local process failed: %+v", res)
		}
		stacks := make([]byte, 64<<10)
		n := runtime.Stack(stacks, true)
		if strings.Contains(string(stacks[:n]), "(*Local).Spawn.func1") {
			t.Fatalf("Spawn returned while its cancellation watcher was still owned:\n%s", stacks[:n])
		}
	}
}

// TestStdinIsFed: stdin bytes reach the child.
func TestStdinIsFed(t *testing.T) {
	res := New(passthrough()).Spawn(hx.SpawnRequest{
		Argv:  []string{"/bin/sh", "-c", "cat"},
		Stdin: []byte("ping-through-stdin"),
	})
	if res.Err != "" || res.ExitCode != 0 {
		t.Fatalf("unexpected outcome: ExitCode=%d Err=%q", res.ExitCode, res.Err)
	}
	if !strings.Contains(string(res.Stdout), "ping-through-stdin") {
		t.Fatalf("Stdout = %q, want stdin echoed back", res.Stdout)
	}
}

// TestNonZeroExitIsData: a non-zero exit is reported as the code, never as Err.
func TestNonZeroExitIsData(t *testing.T) {
	res := New(passthrough()).Spawn(hx.SpawnRequest{Argv: []string{"/bin/sh", "-c", "exit 3"}})
	if res.Err != "" {
		t.Fatalf("non-zero exit set Err = %q, want empty (exit is data)", res.Err)
	}
	if res.ExitCode != 3 {
		t.Fatalf("ExitCode = %d, want 3", res.ExitCode)
	}
	if res.TimedOut {
		t.Fatalf("TimedOut = true, want false")
	}
}

// TestSpawnFailureSetsErr: an unspawnable command sets Err (a spawn failure, the
// one thing that is NOT data).
func TestSpawnFailureSetsErr(t *testing.T) {
	res := New(passthrough()).Spawn(hx.SpawnRequest{Argv: []string{"/no/such/binary/at/all"}})
	if res.Err == "" {
		t.Fatalf("expected Err for a missing binary, got none (ExitCode=%d)", res.ExitCode)
	}
}

// TestTimeoutTreeKill: a command that exceeds a short timeout returns promptly
// with TimedOut set, and the grandchild it forked is killed with the group — it
// does not outlive the call.
func TestTimeoutTreeKill(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")

	// sh (the child) backgrounds a long sleep (the grandchild), records its PID,
	// then waits — so the whole group is alive when the 200ms timeout fires.
	script := "sleep 30 & echo $! > " + pidFile + "; wait"

	start := time.Now()
	// Dir is the temp dir the script writes into: under a REAL confiner that is what
	// puts the pid file inside the writable set, so this test does not depend on the
	// spawner's policy being toothless.
	res := New(passthrough()).Spawn(hx.SpawnRequest{
		Argv:      []string{"/bin/sh", "-c", script},
		Dir:       dir,
		TimeoutMs: 200,
	})
	elapsed := time.Since(start)

	if !res.TimedOut {
		t.Fatalf("TimedOut = false, want true")
	}
	// Prompt return: it must not have blocked for the full 30s sleep.
	if elapsed > 3*time.Second {
		t.Fatalf("Spawn took %v, want it to return promptly after the ~200ms timeout", elapsed)
	}

	// Read the grandchild's PID (written near-immediately by the script).
	var pid int
	for i := 0; i < 50; i++ {
		if b, err := os.ReadFile(pidFile); err == nil {
			if n, convErr := strconv.Atoi(strings.TrimSpace(string(b))); convErr == nil && n > 0 {
				pid = n
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid <= 0 {
		t.Fatalf("could not read grandchild PID from %s", pidFile)
	}

	// The tree-kill must reap the grandchild too. Poll briefly for it to die.
	deadline := time.Now().Add(2 * time.Second)
	for alive(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if alive(pid) {
		t.Fatalf("grandchild pid %d still alive — tree-kill did not reach the whole group", pid)
	}
}

// TestParentCancelKills: cancelling the request context stops the process too, and
// TimedOut stays false (a cancel is not a timeout).
func TestParentCancelKills(t *testing.T) {
	res := New(passthrough()).Spawn(hx.SpawnRequest{
		Argv:      []string{"/bin/sh", "-c", "sleep 30"},
		TimeoutMs: 150, // safety net; the point is it returns without waiting 30s
	})
	if !res.TimedOut {
		t.Fatalf("expected TimedOut for the 150ms deadline")
	}
}

// TestOutputTruncatedAtCap: output beyond maxCapture is truncated and noted, never
// grown unbounded.
func TestOutputTruncatedAtCap(t *testing.T) {
	l := &Local{maxCapture: 16, confiner: passthrough()} // tiny cap to force truncation
	// Emit 1000 'A's, far past the 16-byte cap.
	res := l.Spawn(hx.SpawnRequest{
		Argv: []string{"/bin/sh", "-c", "printf 'A%.0s' $(seq 1 1000)"},
	})
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", res.ExitCode)
	}
	// Captured payload (before the note) is capped; total returned stays small.
	if !strings.HasPrefix(string(res.Stdout), strings.Repeat("A", 16)) {
		t.Fatalf("Stdout should start with the 16 captured bytes, got %q", res.Stdout)
	}
	if len(res.Stdout) >= 1000 {
		t.Fatalf("Stdout len = %d, want it bounded well under the 1000 bytes produced", len(res.Stdout))
	}
	if !strings.Contains(string(res.Stdout), "truncated") {
		t.Fatalf("Stdout = %q, want a truncation note", res.Stdout)
	}
}

// --- confinement -------------------------------------------------------------

// TestConfinedArgvIsWhatRuns proves the spawner execs the CONFINED argv, not the
// one it was handed, and stamps the result accordingly. The fake rewrites the argv
// to a different command, so the assertion is on the process's real output:
// deleting the Confine call in confine() makes /bin/echo print "raw" and this
// test fail on stdout, not on a recorded flag.
func TestConfinedArgvIsWhatRuns(t *testing.T) {
	sb := &fakeProvider{available: true, confineTo: []string{"/bin/echo", "CONFINED"}}
	dir := t.TempDir()

	res := New(sb).Spawn(hx.SpawnRequest{Argv: []string{"/bin/echo", "raw"}, Dir: dir})

	if !sb.called {
		t.Fatal("Confine was never called — the argv reached exec unconfined")
	}
	if got := string(res.Stdout); !strings.Contains(got, "CONFINED") || strings.Contains(got, "raw") {
		t.Fatalf("Stdout = %q, want the CONFINED argv to be what ran", got)
	}
	if res.Confinement != hx.ConfineSpawner {
		t.Fatalf("Confinement = %q, want %q", res.Confinement, hx.ConfineSpawner)
	}
	// Default-deny, derived from Dir: read and write the working directory, no
	// egress — plus /dev/null, which every generated policy grants (see
	// alwaysWritable) because a (deny default) profile otherwise kills `2>/dev/null`.
	want := hx.SandboxPolicy{ReadPaths: []string{dir}, WritePaths: []string{"/dev/null", dir}}
	if !reflect.DeepEqual(sb.gotPolicy, want) {
		t.Fatalf("policy = %+v, want %+v", sb.gotPolicy, want)
	}
}

// TestRefusesWhenNoMechanism is the degraded-host decision: with no confinement in
// force and no opt-in, the command does NOT run. The assertion is the ABSENCE of a
// side effect the command would have had — flip the refusal (or make Available()
// true) and the file appears, which is what makes this a guard rather than a
// restatement of the code.
func TestRefusesWhenNoMechanism(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "it-ran")

	res := New(&fakeProvider{available: false}).Spawn(hx.SpawnRequest{
		Argv: []string{"/bin/sh", "-c", "echo ran > " + marker},
		Dir:  dir,
	})

	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("UNCONFINED EXECUTION — the refused command ran and created %s", marker)
	}
	if res.Err == "" {
		t.Fatal("Err is empty — a refusal must be observable to the caller")
	}
	if res.ExitCode != -1 {
		t.Fatalf("ExitCode = %d, want -1 (nothing ran)", res.ExitCode)
	}
	if res.Confinement != hx.ConfineRefused {
		t.Fatalf("Confinement = %q, want %q", res.Confinement, hx.ConfineRefused)
	}
	// The refusal names the escape hatch, so an operator is not left guessing.
	if !strings.Contains(res.Err, "FORGE_ALLOW_UNCONFINED_EXEC") {
		t.Fatalf("Err = %q, want it to name the opt-in", res.Err)
	}
}

// TestNilConfinerRefuses: a Local wired with no provider at all is treated exactly
// like a host with no mechanism — a forgotten seam must fail closed, not fail open.
func TestNilConfinerRefuses(t *testing.T) {
	res := New(nil).Spawn(hx.SpawnRequest{Argv: []string{"/bin/echo", "hi"}})
	if res.Confinement != hx.ConfineRefused || res.Err == "" {
		t.Fatalf("nil confiner did not refuse: %+v", res)
	}
	if strings.Contains(string(res.Stdout), "hi") {
		t.Fatalf("the command ran with no confiner wired: %q", res.Stdout)
	}
}

// TestDegradedRunsAndIsStamped: the operator opt-in runs the command on a
// mechanism-less host AND records that it was unconfined. Both halves matter — the
// stamp is the only thing that separates this from silently proceeding.
func TestDegradedRunsAndIsStamped(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "it-ran")

	res := NewDegraded(&fakeProvider{available: false}).Spawn(hx.SpawnRequest{
		Argv: []string{"/bin/sh", "-c", "echo ran > " + marker},
		Dir:  dir,
	})

	if res.ExitCode != 0 || res.Err != "" {
		t.Fatalf("degraded spawn did not run: ExitCode=%d Err=%q", res.ExitCode, res.Err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("degraded spawn produced no side effect: %v", err)
	}
	if res.Confinement != hx.ConfineNone {
		t.Fatalf("Confinement = %q, want %q — an unconfined run must say so", res.Confinement, hx.ConfineNone)
	}
}

// TestCallerConfinedIsNotWrappedTwice: a caller that already confined the argv
// (shell, the §4.6 driver) is taken at its word and the provider is not invoked
// again — nesting one mechanism inside another breaks the mechanism.
func TestCallerConfinedIsNotWrappedTwice(t *testing.T) {
	sb := &fakeProvider{available: true, confineTo: []string{"/bin/echo", "DOUBLE-WRAPPED"}}

	res := New(sb).Spawn(hx.SpawnRequest{
		Argv:        []string{"/bin/echo", "already-confined"},
		Confinement: hx.ConfineCaller,
	})

	if sb.called {
		t.Fatal("the spawner confined an argv the caller had already confined (double wrap)")
	}
	if !strings.Contains(string(res.Stdout), "already-confined") {
		t.Fatalf("Stdout = %q, want the caller's argv to run untouched", res.Stdout)
	}
	if res.Confinement != hx.ConfineCaller {
		t.Fatalf("Confinement = %q, want %q", res.Confinement, hx.ConfineCaller)
	}
}

// TestDeclaredUnconfinedRuns: the explicit per-call opt-out (research/tools' ddgs)
// runs even on a fail-closed Local, and is stamped none.
func TestDeclaredUnconfinedRuns(t *testing.T) {
	sb := &fakeProvider{available: true, confineTo: []string{"/bin/echo", "WRAPPED"}}

	res := New(sb).Spawn(hx.SpawnRequest{
		Argv:        []string{"/bin/echo", "unwrapped"},
		Confinement: hx.ConfineNone,
	})

	if sb.called {
		t.Fatal("a declared opt-out was confined anyway")
	}
	if !strings.Contains(string(res.Stdout), "unwrapped") {
		t.Fatalf("Stdout = %q, want the declared-unconfined argv to run", res.Stdout)
	}
	if res.Confinement != hx.ConfineNone {
		t.Fatalf("Confinement = %q, want %q", res.Confinement, hx.ConfineNone)
	}
}

// TestUnknownDeclarationRefuses: an unrecognised declaration is a programming
// error, and the safe reading of "I don't know what you meant" is not "run it".
func TestUnknownDeclarationRefuses(t *testing.T) {
	res := New(passthrough()).Spawn(hx.SpawnRequest{
		Argv:        []string{"/bin/echo", "hi"},
		Confinement: "definitely-not-a-real-value",
	})
	if res.Confinement != hx.ConfineRefused || res.Err == "" {
		t.Fatalf("unknown declaration was not refused: %+v", res)
	}
}

// TestConfineErrorRefuses: a provider that fails to build a confined argv is a
// refusal, not a reason to run the raw one.
func TestConfineErrorRefuses(t *testing.T) {
	sb := &fakeProvider{available: true, confineErr: os.ErrPermission}
	res := New(sb).Spawn(hx.SpawnRequest{Argv: []string{"/bin/echo", "hi"}})
	if res.Confinement != hx.ConfineRefused || res.Err == "" {
		t.Fatalf("a Confine error did not refuse: %+v", res)
	}
	if strings.Contains(string(res.Stdout), "hi") {
		t.Fatalf("the raw argv ran after Confine failed: %q", res.Stdout)
	}
}

// --- the hard-deny floor -----------------------------------------------------

// TestHardDenyFloorRefusesBeforeSpawn proves the §4.6 floor holds in the spawner
// itself, and holds BEFORE anything is executed. The tree it would delete is real:
// remove the HardDeny call and `rm -rf` runs for real, the directory disappears,
// and the test fails on the missing file rather than on a flag.
//
// It also proves the floor is independent of confinement — the provider here
// reports a mechanism, which is the case where it would be tempting to assume the
// sandbox has it covered. It does not: a policy that lets a command write its
// workspace also lets it delete the workspace.
func TestHardDenyFloorRefusesBeforeSpawn(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.MkdirAll(victim, 0o750); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(victim, "keep.txt")
	if err := os.WriteFile(keep, []byte("KEEP"), 0o600); err != nil {
		t.Fatal(err)
	}

	res := New(passthrough()).Spawn(hx.SpawnRequest{Argv: []string{"rm", "-rf", victim}, Dir: dir})

	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("HARD-DENY BREACH — rm -rf actually ran and removed %s: %v", keep, err)
	}
	if res.Confinement != hx.ConfineRefused {
		t.Fatalf("Confinement = %q, want %q", res.Confinement, hx.ConfineRefused)
	}
	if !strings.Contains(res.Err, "hard-deny") || !strings.Contains(res.Err, "recursive-delete") {
		t.Fatalf("Err = %q, want the typed hard-deny category in the refusal", res.Err)
	}
}

// TestHardDenyFloorAppliesToDeclaredOptOut: the floor is a floor. A caller that
// declares hx.ConfineNone (the web tools) does not get past it either.
func TestHardDenyFloorAppliesToDeclaredOptOut(t *testing.T) {
	res := New(passthrough()).Spawn(hx.SpawnRequest{
		Argv:        []string{"/bin/cat", os.Getenv("HOME") + "/.ssh/id_rsa"},
		Confinement: hx.ConfineNone,
	})
	if res.Confinement != hx.ConfineRefused {
		t.Fatalf("Confinement = %q, want a refusal for a credential path", res.Confinement)
	}
	if !strings.Contains(res.Err, "credential-access") {
		t.Fatalf("Err = %q, want the credential-access category", res.Err)
	}
}

// --- live: the REAL host mechanism, not a fake -------------------------------

// TestRealConfinementHasTeeth runs the actual seatbelt mechanism (no fakes) and
// proves the spawner's own default policy binds: a write INSIDE the request's Dir
// succeeds, a write OUTSIDE it is blocked by the kernel. Deleting the Confine call
// in confine() makes the escape write succeed and fails this test.
//
// darwin-only for the same reason internal/sandbox's equivalent is: seatbelt is
// always present here, whereas a linux container may have no mechanism at all and
// the test would assert nothing. Skipped rather than faked when unavailable.
func TestRealConfinementHasTeeth(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("seatbelt-specific confinement test (darwin only)")
	}
	provider := harnesssandbox.New()
	if !provider.Available() {
		t.Skip("no real confiner on this host")
	}
	// EvalSymlinks: seatbelt canonicalizes write targets (/var -> /private/var), so
	// an unresolved TempDir would be denied even for writes made inside it.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l := New(provider)

	inside := filepath.Join(dir, "inside.txt")
	res := l.Spawn(hx.SpawnRequest{Argv: []string{"/bin/sh", "-c", "echo ok > " + inside}, Dir: dir})
	if res.ExitCode != 0 {
		t.Fatalf("in-workspace write was blocked: exit=%d stderr=%q err=%q", res.ExitCode, res.Stderr, res.Err)
	}
	if res.Confinement != hx.ConfineSpawner {
		t.Fatalf("Confinement = %q, want %q", res.Confinement, hx.ConfineSpawner)
	}

	escape := filepath.Join(filepath.Dir(dir), "ESCAPE_SHOULD_FAIL")
	res = l.Spawn(hx.SpawnRequest{Argv: []string{"/bin/sh", "-c", "echo pwned > " + escape}, Dir: dir})
	if _, statErr := os.Stat(escape); statErr == nil {
		_ = os.Remove(escape)
		t.Fatalf("CONFINEMENT BREACH — write outside the writable set succeeded at %s (exit=%d)", escape, res.ExitCode)
	}
}

// TestRealConfinementKeepsSpawnFailureAsErr guards the LookPath preflight against
// the REAL wrapper: with sandbox-exec in front of the argv, a missing target binary
// would otherwise surface as the WRAPPER's non-zero exit and Err would be empty —
// silently reclassifying a spawn failure as an ordinary command failure, which is
// the signal research/tools reads to detect a dead backend. Remove the preflight
// and this test fails with an empty Err.
func TestRealConfinementKeepsSpawnFailureAsErr(t *testing.T) {
	provider := harnesssandbox.New()
	if !provider.Available() {
		t.Skip("no real confiner on this host")
	}
	res := New(provider).Spawn(hx.SpawnRequest{Argv: []string{"/no/such/binary/at/all"}, Dir: t.TempDir()})
	if res.Err == "" {
		t.Fatalf("a missing binary under confinement did not set Err (ExitCode=%d stderr=%q)", res.ExitCode, res.Stderr)
	}
}

// --- declared access ---------------------------------------------------------

// TestDeclaredPolicyIsWhatConfines proves a caller's declaration reaches the
// provider verbatim (writes and egress as declared), with Dir unioned into the
// READABLE set only — a declaration that says nothing about writing the working
// directory does not get to write it.
func TestDeclaredPolicyIsWhatConfines(t *testing.T) {
	sb := &fakeProvider{available: true}
	dir := t.TempDir()
	declared := &hx.SandboxPolicy{WritePaths: []string{"/tmp/declared-out"}, AllowNetwork: true}

	New(sb).Spawn(hx.SpawnRequest{Argv: []string{"/bin/echo", "hi"}, Dir: dir, Sandbox: declared})

	if !sb.gotPolicy.AllowNetwork {
		t.Fatal("declared egress did not reach the provider")
	}
	if !containsPath(sb.gotPolicy.WritePaths, "/tmp/declared-out") {
		t.Fatalf("WritePaths = %v, want the declared path", sb.gotPolicy.WritePaths)
	}
	if containsPath(sb.gotPolicy.WritePaths, dir) {
		t.Fatalf("WritePaths = %v — Dir must not be granted for WRITING by a declaration that did not ask", sb.gotPolicy.WritePaths)
	}
	if !containsPath(sb.gotPolicy.ReadPaths, dir) {
		t.Fatalf("ReadPaths = %v, want the working dir readable", sb.gotPolicy.ReadPaths)
	}
	// The caller's slices are never mutated.
	if len(declared.WritePaths) != 1 || len(declared.ReadPaths) != 0 {
		t.Fatalf("the caller's policy was mutated: %+v", declared)
	}
}

// TestDevNullIsAlwaysWritable: every generated policy grants /dev/null, declared or
// not. Under (deny default) it is denied like any other path, and `2>/dev/null`
// then kills commands that were fine — measured on this host.
func TestDevNullIsAlwaysWritable(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  hx.SpawnRequest
	}{
		{"default policy", hx.SpawnRequest{Argv: []string{"/bin/echo", "hi"}, Dir: "/work"}},
		{"declared policy", hx.SpawnRequest{Argv: []string{"/bin/echo", "hi"}, Dir: "/work", Sandbox: &hx.SandboxPolicy{}}},
		{"no working dir", hx.SpawnRequest{Argv: []string{"/bin/echo", "hi"}}},
	} {
		sb := &fakeProvider{available: true}
		New(sb).Spawn(tc.req)
		if !containsPath(sb.gotPolicy.WritePaths, "/dev/null") {
			t.Errorf("%s: WritePaths = %v, want /dev/null writable", tc.name, sb.gotPolicy.WritePaths)
		}
	}
}

func containsPath(paths []string, want string) bool {
	return slices.Contains(paths, want)
}

// --- the policy canary -------------------------------------------------------

// TestCanaryPassesUnderRealPolicy runs the canary against the REAL host mechanism:
// a functional policy must let a trivial command exec, write /dev/null, and write
// its workspace. This is the assertion that would have caught the Keychain-class
// outage, where the profile itself was broken and every run looked like the agent
// failing.
func TestCanaryPassesUnderRealPolicy(t *testing.T) {
	realSB := harnesssandbox.New()
	if !realSB.Available() {
		t.Skip("no real confiner on this host")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	res := New(realSB).Canary(hx.SpawnRequest{Dir: dir})
	if res.ExitCode != 0 || res.Err != "" {
		t.Fatalf("canary failed under a policy that should work: exit=%d err=%q stderr=%q", res.ExitCode, res.Err, res.Stderr)
	}
	if res.Confinement != hx.ConfineSpawner {
		t.Fatalf("Confinement = %q, want the canary to run confined", res.Confinement)
	}
	if _, statErr := os.Stat(filepath.Join(dir, canaryFile)); statErr == nil {
		t.Fatal("the canary left its file behind")
	}
}

// TestCanaryFailsWhenPolicyIsBroken is the other half, and the one that makes the
// canary worth having: a policy that cannot write the workspace must FAIL the
// canary rather than silently mislabelling every later run. The broken policy is
// produced honestly — a real seatbelt profile with the workspace left out.
func TestCanaryFailsWhenPolicyIsBroken(t *testing.T) {
	realSB := harnesssandbox.New()
	if !realSB.Available() {
		t.Skip("no real confiner on this host")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Declared access that grants /dev/null but NOT the workspace: the canary's
	// workspace write is denied, which is exactly the whole-policy breakage case.
	res := New(realSB).Canary(hx.SpawnRequest{Dir: dir, Sandbox: &hx.SandboxPolicy{}})
	if res.ExitCode == 0 {
		t.Fatalf("canary passed under a policy that cannot write the workspace (exit=0) — it would certify a broken sandbox as usable")
	}
}

// TestCanaryRefusedWhenNothingCanConfine: on a mechanism-less fail-closed host the
// canary is refused, which is the correct signal ("cannot run under policy") and
// distinguishable from a command that ran and failed.
func TestCanaryRefusedWhenNothingCanConfine(t *testing.T) {
	res := New(&fakeProvider{available: false}).Canary(hx.SpawnRequest{Dir: t.TempDir()})
	if res.Confinement != hx.ConfineRefused || res.Err == "" {
		t.Fatalf("canary was not refused on a host with no mechanism: %+v", res)
	}
}

// TestRealConfinedRedirectToDevNull is the live regression for the /dev/null grant:
// `2>/dev/null` is in a large share of real commands and, without the grant, the
// whole command dies with "Operation not permitted" and exit 1 — a failure that
// reads as the command's own. Drop /dev/null from alwaysWritable and this fails.
func TestRealConfinedRedirectToDevNull(t *testing.T) {
	realSB := harnesssandbox.New()
	if !realSB.Available() {
		t.Skip("no real confiner on this host")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	res := New(realSB).Spawn(hx.SpawnRequest{
		Argv: []string{"/bin/sh", "-c", "ls . 2>/dev/null && echo done"},
		Dir:  dir,
	})
	if res.ExitCode != 0 {
		t.Fatalf("a confined `2>/dev/null` command failed: exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(string(res.Stdout), "done") {
		t.Fatalf("Stdout = %q, want the command to have completed", res.Stdout)
	}
}
