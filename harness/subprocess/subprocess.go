// Package subprocess is the local-process execution capability of the Forge
// harness (design §4 subprocess row, §8 H2): it runs a real command on this host
// and returns its outcome as data, never as a verdict. It is the concrete
// hx.Subprocess a shell/sandbox layer builds on and the loop resolves by
// hx.KeySubprocess.
//
// Four invariants shape the implementation, all of them memory- or safety-driven
// (design §5.5):
//
//   - Confinement is not optional. This is the last code that runs before a
//     MODEL-CHOSEN argv becomes a real process on the user's machine, so it is
//     where "was this confined?" has to be answered rather than assumed. Every
//     Spawn passes the §4.6 hard-deny floor, then either runs confined or does not
//     run — see Spawn and the degraded-host argument on confine().
//   - Bounded capture. stdout/stderr are drained into fixed-size buffers; output
//     past the cap is counted and dropped, never grown into RAM (a real collector
//     spills to CAS — here we truncate and note it).
//   - Tree-kill. The child starts as the leader of its OWN process group; on
//     cancel or timeout we signal the whole group, so a grandchild the command
//     forked cannot outlive the call.
//   - Exit is data. A non-zero exit code, a signal death, a timeout — these are
//     operational facts on the result (Law 3), not errors. Only a genuine failure
//     to SPAWN (bad binary, bad dir) or a REFUSAL to spawn sets Err.
//
// Like the shell package, it consumes confinement only through hx.SandboxProvider
// and never imports a sibling harness package: the provider is resolved by key
// (hx.KeySandbox) in component.go and injected here. The one concrete dependency
// is the hard-deny floor (internal/sandbox/floor), which is a stdlib-only leaf of
// pure rules — the single definition §4.6 requires, not a second copy.
package subprocess

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/sandbox/floor"
)

// Defaults applied when a request or a Local leaves a field zero. The capture cap
// is deliberately small — bounded memory is the point; a caller that needs the
// full stream references it out of band.
const (
	defaultMaxCapture = 256 << 10        // 256 KiB per stream
	defaultTimeout    = 30 * time.Second // when TimeoutMs <= 0
)

// Local runs processes on the host machine. It carries no per-call state, so a
// single Local is safe to share across concurrent Spawn calls, and every field is
// fixed at construction — the confinement decision is therefore a pure function of
// injected state, which is what makes it testable without a host that happens to
// have (or lack) a mechanism.
//
// The zero value spawns NOTHING: no confiner and no degraded opt-in means every
// Spawn is refused. That is deliberate. A struct literal that forgot to wire
// confinement must fail closed, not run a model's command raw.
type Local struct {
	maxCapture     int           // per-stream capture cap in bytes; 0 = default
	defaultTimeout time.Duration // wall clock when a request sets no timeout; 0 = default
	// confiner turns an argv into a confined argv (hx.SandboxProvider, resolved by
	// key in component.go). nil means no confinement is wired at all, which is
	// treated exactly like a provider that reports no mechanism.
	confiner hx.SandboxProvider
	// allowUnconfined is the OPERATOR's opt-in to run on a host where confinement
	// is unavailable. Only NewDegraded sets it; nothing reachable from a model can.
	allowUnconfined bool
}

// New returns a Local that confines every process it spawns with confiner, and
// REFUSES to spawn when confiner reports no mechanism on this host. This is the
// constructor drive mode uses.
//
// The confiner is a required argument, not an option with a benign default,
// because there is no benign default: a Local built without one would run
// model-chosen commands raw. Making it positional turns "I forgot to confine" from
// a runtime property of a host into a compile error at every call site.
func New(confiner hx.SandboxProvider) *Local {
	return &Local{maxCapture: defaultMaxCapture, defaultTimeout: defaultTimeout, confiner: confiner}
}

// NewDegraded returns a Local that still runs when confiner reports no mechanism —
// hard-deny floor only, every result stamped hx.ConfineNone. It is the explicitly
// degraded profile (system design §17) and its use is a decision someone made in
// code or in the host's environment, never a fallback taken silently at runtime.
//
// Two callers legitimately want it: the §4.6 local sandbox driver, which already
// declares itself not-a-boundary through its own Available(), and an operator on a
// host with no mechanism who has accepted the risk (component.go, via
// FORGE_ALLOW_UNCONFINED_EXEC).
func NewDegraded(confiner hx.SandboxProvider) *Local {
	l := New(confiner)
	l.allowUnconfined = true
	return l
}

func (l *Local) cap(requested int) int {
	limit := defaultMaxCapture
	if l.maxCapture > 0 {
		limit = l.maxCapture
	}
	if requested > 0 {
		limit = requested
	}
	// Callers may retain larger structured transcripts without making capture
	// unbounded. Both streams are bounded independently.
	return min(limit, 8<<20)
}

func (l *Local) timeout(ms int) time.Duration {
	if ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	if l.defaultTimeout > 0 {
		return l.defaultTimeout
	}
	return defaultTimeout
}

// Spawn runs req.Argv and returns its outcome. It folds req.Ctx with the request
// timeout into one deadline context; whichever fires first tree-kills the child's
// process group so nothing it forked survives. stdout/stderr are captured into
// bounded buffers. A non-zero ExitCode is reported without Err — only a spawn
// failure (empty argv, missing binary, unreadable Dir) or a refusal sets Err.
//
// Two gates run BEFORE anything is executed, in this order:
//
//  1. The §4.6 hard-deny floor, unconditionally and with no knob: a recursive
//     delete, a credential path, or destructive SQL is refused and NO process is
//     started. It runs on every path — including the confined one, because a
//     seatbelt/bwrap policy that permits writes in the workspace still permits
//     `rm -rf .` inside it, and including the opted-out one, because that is the
//     whole point of a floor.
//  2. Confinement, per confine() below. Whatever it decides is reported on the
//     result as Confinement, so "it ran unconfined" is always a fact a caller can
//     read and never an omission it has to infer.
func (l *Local) Spawn(req hx.SpawnRequest) hx.SpawnResult {
	if len(req.Argv) == 0 {
		return hx.SpawnResult{ExitCode: -1, Err: "subprocess: empty argv"}
	}
	// The floor first, ahead of the confinement decision: a denied command must be
	// refused identically whether or not this host can confine it (§4.6 — postures
	// govern friction, the hard policy governs damage). The refusal text carries
	// the typed *floor.DenyError's category and match for the record.
	if err := floor.HardDeny(req.Argv, ""); err != nil {
		return hx.SpawnResult{ExitCode: -1, Confinement: hx.ConfineRefused, Err: "subprocess: " + err.Error()}
	}
	argv, confinement, err := l.confine(req)
	if err != nil {
		return hx.SpawnResult{ExitCode: -1, Confinement: hx.ConfineRefused, Err: fmt.Sprintf("subprocess: %v: %v", req.Argv, err)}
	}
	// Resolve the TARGET binary before the wrapper hides it. Under confinement the
	// argv we exec starts with sandbox-exec/bwrap, so a missing target would come
	// back as the WRAPPER's non-zero exit — silently reclassifying a spawn failure
	// as ordinary command failure and breaking the invariant that only a failure to
	// spawn sets Err (research/tools reads exactly that to detect a dead backend).
	// LookPath here reproduces what exec.Command already does for the unconfined
	// case: it resolves against this process's PATH, not req.Env's.
	if _, lookErr := exec.LookPath(req.Argv[0]); lookErr != nil {
		return hx.SpawnResult{ExitCode: -1, Err: fmt.Sprintf("subprocess: start %v: %v", req.Argv, lookErr)}
	}

	base := req.Ctx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(base, l.timeout(req.TimeoutMs))
	defer cancel()

	// Plain exec.Command (not CommandContext): we own cancellation so we can kill
	// the whole GROUP, not just the direct child, on ctx.Done(). argv (not req.Argv)
	// is what actually runs — under ConfineSpawner it is the wrapped form. noctx is suppressed
	// for the same reason: CommandContext kills only the direct child, which is the documented
	// "grandchild holds the pipe" hang (a 300ms deadline taking 30s).
	//nolint:gosec,noctx // G204 is inherent to a spawner: argv is model-chosen by design. Mitigations, in
	// order: the hard-deny floor is applied unconditionally BEFORE any confinement decision, and
	// confine() REFUSES by default when no mechanism is available (degrading only on the
	// composition-time operator opt-in, which is unreachable from the model).
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = req.Dir
	if req.Env != nil { // nil = inherit this process's environment
		cmd.Env = req.Env
	}
	if len(req.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(req.Stdin)
	}
	outW := &capWriter{max: l.cap(req.OutputLimit)}
	errW := &capWriter{max: l.cap(req.OutputLimit)}
	cmd.Stdout = outW
	cmd.Stderr = errW
	// Setpgid makes the child the leader of a new process group (pgid == its pid),
	// so a negative-pid signal reaches every descendant it spawns (tree-kill).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return hx.SpawnResult{ExitCode: -1, Err: fmt.Sprintf("subprocess: start %v: %v", req.Argv, err)}
	}

	// Kill the group on cancel/timeout. The watcher exits when Wait completes
	// (done closed), so a normally-finishing process leaves no lingering goroutine.
	pgid := cmd.Process.Pid // == pgid, because Setpgid made the child the leader
	done := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-ctx.Done():
			// Negative pid targets the whole group. ESRCH (already reaped) is fine.
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		case <-done:
		}
	}()

	waitErr := cmd.Wait()
	close(done)
	// A returned result relinquishes the process group; no watcher may signal it later.
	<-watcherDone

	res := hx.SpawnResult{
		Stdout:          outW.result(),
		Stderr:          errW.result(),
		TimedOut:        errors.Is(ctx.Err(), context.DeadlineExceeded),
		Confinement:     confinement,
		StdoutTruncated: outW.truncated,
		StderrTruncated: errW.truncated,
		StdoutBytes:     outW.total,
		StderrBytes:     errW.total,
	}
	if cmd.ProcessState != nil {
		// ExitCode() is -1 for a signal death (our SIGKILL on timeout/cancel).
		res.ExitCode = cmd.ProcessState.ExitCode()
	} else {
		res.ExitCode = -1
	}
	// waitErr for an *exec.ExitError is expected data (non-zero exit / signal), not
	// a spawn failure — the exit code already carries it, so Err stays empty. Any
	// other Wait error is genuinely operational.
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) {
		res.Err = fmt.Sprintf("subprocess: wait %v: %v", req.Argv, waitErr)
	}
	return res
}

// canaryFile is the dot-file the canary writes inside the workspace, and
// canaryTimeoutMs bounds it — a canary that hangs is a canary nobody runs.
const (
	canaryFile      = ".forge-canary"
	canaryTimeoutMs = 5_000
)

// Canary runs a trivial, known-good command under the policy proto would receive
// and returns its outcome (hx.SandboxCanary). Exit 0 with an empty Err means the
// policy is FUNCTIONAL on this host; anything else means the confinement itself is
// broken and any verdict drawn from a run under it is UNVERIFIABLE, not falsified.
//
// The command is chosen to exercise the two things that silently break a policy
// rather than the command: process exec, and the two writes every real command
// makes — one to /dev/null, one into the workspace. It cleans up its own file.
//
// What it CANNOT do is detect a per-command denial; nothing can (see
// hx.SpawnResult.Confinement). It answers "is this policy usable at all?", which
// is the question behind the Keychain outage where every confined agent run
// reported "Not logged in" and looked like an agent failure.
//
// One honest limit: proto.Confinement is deliberately IGNORED. A request the
// CALLER confined was wrapped with a policy this package never sees, and the
// canary's argv is a different argv, so it could not be wrapped the same way even
// in principle — the canary then measures the spawner's equivalent policy, which
// is a close proxy (shell's policy is this policy) but not the caller's exact
// profile. A caller that wraps its own argv should canary its own wrapping.
func (l *Local) Canary(proto hx.SpawnRequest) hx.SpawnResult {
	req := hx.SpawnRequest{
		Ctx:       proto.Ctx,
		Dir:       proto.Dir,
		Env:       proto.Env,
		Sandbox:   proto.Sandbox,
		TimeoutMs: canaryTimeoutMs,
	}
	// The workspace path travels as $0, not interpolated into the script: a caller's
	// directory can contain spaces or quotes, and a canary that breaks on the path
	// would report a broken policy that is fine.
	if proto.Dir == "" {
		req.Argv = []string{"/bin/sh", "-c", `echo forge-canary > /dev/null`}
		return l.Spawn(req)
	}
	path := filepath.Join(proto.Dir, canaryFile)
	req.Argv = []string{"/bin/sh", "-c", `echo forge-canary > /dev/null && echo forge-canary > "$0"`, path}
	res := l.Spawn(req)
	// Removed from HERE, not by the canary script: a shell `rm` in the argv would
	// be the hard-deny floor's business, and cleanup is not the thing being probed.
	_ = os.Remove(path)
	return res
}

// errNoConfinement is the refusal when this host offers no confinement mechanism
// and unconfined execution was not explicitly enabled. It names the escape hatch
// so an operator hitting it does not have to read the source to find it.
var errNoConfinement = errors.New(
	"refusing to spawn unconfined: no sandbox mechanism on this host " +
		"(install bwrap or landlock-run, or set FORGE_ALLOW_UNCONFINED_EXEC=1 to accept running unconfined)")

// confine decides how req's argv is executed: it returns the argv to actually
// exec, the confinement fact to stamp on the result, and an error when the answer
// is "it does not run at all".
//
// A caller's declaration is honoured first, because only the caller knows it:
//
//   - ConfineCaller — the caller already wrapped the argv with a provider whose
//     Available() it checked (shell, and the §4.6 driver). Wrapping it AGAIN is not
//     defence in depth: nested bwrap needs nested user namespaces and simply fails,
//     and a doubly-applied seatbelt profile is two policies intersecting by
//     accident rather than by design.
//   - ConfineNone — a deliberate opt-out for an argv that cannot run confined at
//     all. Measured, not assumed: research/tools' `ddgs` under this profile aborts
//     with exit 134 before it searches, panicking inside the `system-configuration`
//     crate's dynamic_store ("Attempted to create a NULL object") — the reading is
//     that (deny default) refuses the mach-lookup its HTTP layer uses to read the
//     system proxy. The floor still applied above, and the result says none.
//
// THE DEGRADED-HOST DECISION (the last branch): when nothing is declared and this
// host has no mechanism, drive mode REFUSES rather than running the command.
//
// The reasoning, and it is specific to drive mode: here the MODEL chose the argv
// and FORGE is the process that executes it, so the confinement is not a courtesy
// to the user's machine, it is the only thing standing between a sampled token
// sequence and that machine. Available()==false does not mean "weaker
// confinement", it means there is NO boundary — writes go anywhere the user can
// write, egress is open. The hard-deny floor is emphatically NOT a substitute: it
// is a denylist of three named classes, and a denylist cannot bound an open-ended
// command space (`python -c "shutil.rmtree(...)"` and `curl … | sh` walk straight
// past every rule in it). Running anyway and merely recording the degradation
// would put the honest label on an unbounded action AFTER it happened, which is
// the wrong order: the record is for what was allowed, not an apology for it. So
// the default is fail-closed.
//
// It is a default rather than an absolute because a hard refusal with no way out
// is not actually safer — an operator on a mechanism-less host (a plain container:
// this repo's own CI is one) who cannot run Forge will run the agent outside Forge
// instead, which is strictly worse. The way out is deliberately awkward and
// operator-scoped: it is set once at composition, in code (NewDegraded) or in the
// host's environment (FORGE_ALLOW_UNCONFINED_EXEC), never per call and never by
// anything the model can reach; the floor still applies; and every result is
// stamped ConfineNone so the degradation is visible in the record for as long as
// it lasts.
func (l *Local) confine(req hx.SpawnRequest) (argv []string, confinement string, err error) {
	switch req.Confinement {
	case "": // nothing declared — the spawner decides, below
	case hx.ConfineCaller, hx.ConfineNone:
		return req.Argv, req.Confinement, nil
	default:
		// An unrecognised declaration is a programming error, and the safe reading of
		// "I don't know what you meant" is not "run it anyway".
		return nil, "", fmt.Errorf("unknown confinement declaration %q", req.Confinement)
	}
	if l.confiner != nil && l.confiner.Available() {
		confined, cErr := l.confiner.Confine(req.Argv, policyFor(req))
		if cErr != nil {
			return nil, "", fmt.Errorf("confine argv: %w", cErr)
		}
		return confined, hx.ConfineSpawner, nil
	}
	if l.allowUnconfined {
		return req.Argv, hx.ConfineNone, nil
	}
	return nil, "", errNoConfinement
}

// policyFor returns the policy a request is confined under: the caller's DECLARED
// access when it supplied one, otherwise the default-deny policy derived from Dir.
//
// A declaration is authoritative for writes and egress — that is what makes it a
// declaration — but Dir is always unioned into the readable set, because a process
// that cannot read its own working directory is not a policy, it is a bug. The
// caller's slices are never mutated.
//
// FOLLOW-UP (named, not done here): today only the paths reach this seam, and only
// per call. The methodology layer has started declaring the same thing durably —
// methodology.Requirement carries Egress, default-deny, and declaring it drops the
// evidence grade from deterministic to replayable. Declared WRITE PATHS belong
// there too, flowing down into SpawnRequest.Sandbox, so that "this check needed to
// write outside its workspace" is a property of the obligation and costs the same
// grade, rather than being an argument one call site passes and no record sees.
func policyFor(req hx.SpawnRequest) hx.SandboxPolicy {
	if req.Sandbox != nil {
		p := hx.SandboxPolicy{
			ReadPaths:    append([]string(nil), req.Sandbox.ReadPaths...),
			WritePaths:   append([]string(nil), req.Sandbox.WritePaths...),
			AllowNetwork: req.Sandbox.AllowNetwork,
		}
		if req.Dir != "" {
			p.ReadPaths = append(p.ReadPaths, req.Dir)
		}
		p.WritePaths = append(p.WritePaths, alwaysWritable...)
		return p
	}
	return defaultPolicy(req.Dir)
}

// alwaysWritable are paths granted to EVERY confined process regardless of what
// was declared. /dev/null is not an access grant in any meaningful sense — it is
// the standard way to discard output, and `2>/dev/null` appears in a large share
// of real commands — but under a (deny default) profile it is denied like any
// other path, and the failure is brutal and misleading: measured here, a confined
// `ls dir 2>/dev/null` dies with "/dev/null: Operation not permitted" and exit 1,
// which reads as the command failing rather than as the sandbox refusing.
//
// It belongs one layer down, in the provider that builds the profile (one place,
// both platforms) rather than in every policy generator. It is here because that
// file is being changed concurrently; the generators that still lack it are
// shell.confinePolicy and sandbox.DefaultPolicy.
var alwaysWritable = []string{"/dev/null"}

// defaultPolicy is the conservative default-deny policy the spawner confines an
// undeclared request under — the same shape the shell has always used: the working
// directory is the only path it may read or write, and egress is off (§4.6). An
// empty Dir yields no path of its own rather than a spurious "" path, which means a
// request that declared no working directory gets no writable path but /dev/null;
// a caller that needs one says where it is.
func defaultPolicy(dir string) hx.SandboxPolicy {
	p := hx.SandboxPolicy{AllowNetwork: false, WritePaths: append([]string(nil), alwaysWritable...)}
	if dir != "" {
		p.ReadPaths = []string{dir}
		p.WritePaths = append(p.WritePaths, dir)
	}
	return p
}

// capWriter drains a stream into at most max bytes and counts the rest. Write
// always reports the full length so the child is never blocked by a short write —
// bytes past the cap are consumed and discarded, keeping memory bounded no matter
// how much the process emits.
type capWriter struct {
	max       int
	buf       []byte
	total     int
	truncated bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	w.total += len(p)
	if room := w.max - len(w.buf); room > 0 {
		if len(p) > room {
			w.buf = append(w.buf, p[:room]...)
			w.truncated = true
		} else {
			w.buf = append(w.buf, p...)
		}
	} else if len(p) > 0 {
		w.truncated = true
	}
	return len(p), nil
}

// result returns the captured bytes, appending a truncation note when the stream
// overflowed the cap so the fact that output was dropped is never silent.
func (w *capWriter) result() []byte {
	if !w.truncated {
		return w.buf
	}
	note := fmt.Sprintf("\n[subprocess: output truncated at %d bytes; %d produced]", w.max, w.total)
	out := make([]byte, 0, len(w.buf)+len(note))
	out = append(out, w.buf...)
	return append(out, note...)
}

// Compile-time proof that Local is a full hx.Subprocess, and that it carries the
// optional policy canary a verdict-building caller type-asserts for.
var (
	_ hx.Subprocess    = (*Local)(nil)
	_ hx.SandboxCanary = (*Local)(nil)
)
