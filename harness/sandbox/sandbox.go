// Package sandbox is the execution-confinement provider (design §4 sandbox row,
// system design §4.6 default-deny egress): it transforms a tool's argv into a
// CONFINED argv that a supported OS mechanism enforces before the process runs.
// It is a pure argv transform — it never spawns anything. Process execution is
// the subprocess package's concern; this package only detects a mechanism and
// rewrites the argv, so it deliberately does NOT import os/exec.
//
// This is the DECLARED-DEGRADED profile (system design §17). Two things are
// honest here and must stay honest:
//
//   - The darwin path is FULLY BUILT: sandbox-exec (seatbelt / SBPL) with a
//     conservative (deny default) profile. Seatbelt profile FIDELITY is
//     best-effort on the degraded profile — the profile is built defensively
//     from the policy, not audited against every seatbelt operation class.
//   - The linux path is the CUSTOMER-PROFILE SWAP. On linux the mechanism that
//     confines is a C `landlock-run` binary used WHOLLY (it carries its own
//     default-deny landlock ruleset), with `bwrap` (bubblewrap) as a
//     stdlib-buildable degraded fallback. The landlock-run binary REPLACES this
//     provider's linux wrapping for a customer deployment WITHOUT changing
//     callers — it is named here, not reimplemented.
//
// When no supported mechanism is present, Confine returns the argv UNCHANGED and
// does not error: the caller runs the process unconfined and Available()==false
// is the observable signal that it did. A caller that must not run unconfined
// reads Available() and refuses; this package never silently pretends to confine.
package sandbox

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/websines/structor-harness/harness/hx"
)

// Wrapper binary names — the mechanism each platform confines with. sandbox-exec
// is macOS seatbelt; bwrap is bubblewrap; landlock-run is the C binary that is
// the customer-profile swap (used wholly) on linux.
const (
	seatbeltTool = "sandbox-exec"
	bwrapTool    = "bwrap"
	landlockRun  = "landlock-run"
)

// Local is the hx.SandboxProvider for this host. Its behaviour is fully
// determined by two fields fixed at construction, so Confine/Available are pure
// functions of state (deterministic in a test that constructs a Local directly).
type Local struct {
	// goos is the platform the transform targets: runtime.GOOS in production, an
	// overridden value in a test that exercises a platform other than the host.
	goos string
	// wrapper is the resolved confinement binary (name or absolute path), or ""
	// when no supported mechanism is on PATH. Empty means the provider is a
	// pass-through and Available() is false.
	wrapper string
}

// New returns a Local whose mechanism is detected for the current host:
// sandbox-exec on darwin, then bwrap or landlock-run on linux. When none is
// resolvable the provider is a pass-through (Available()==false).
func New() *Local {
	return &Local{goos: runtime.GOOS, wrapper: detect(runtime.GOOS)}
}

// detect resolves the confinement binary for goos, or "" if none is present.
func detect(goos string) string {
	switch goos {
	case "darwin":
		return lookPath(seatbeltTool)
	case "linux":
		// Prefer the stdlib-buildable bwrap fallback; landlock-run (the C-binary
		// swap) still marks the mechanism as present when it is the one on PATH.
		if p := lookPath(bwrapTool); p != "" {
			return p
		}
		return lookPath(landlockRun)
	default:
		return ""
	}
}

// Available reports whether real confinement is in force on this host — true iff
// a supported mechanism binary was resolved. When false, Confine is a
// pass-through and the caller decides whether to proceed unconfined or refuse.
func (l *Local) Available() bool { return l.wrapper != "" }

// Confine returns the argv to actually exec to run argv under policy. With no
// mechanism it returns argv UNCHANGED and no error (Available()==false is the
// signal). The input argv is never mutated: a fresh slice is always returned.
func (l *Local) Confine(argv []string, policy hx.SandboxPolicy) ([]string, error) {
	if l.wrapper == "" {
		return argv, nil // no mechanism → run unconfined; Available()==false says so
	}
	switch l.goos {
	case "darwin":
		// sandbox-exec -p <profile> <argv...>
		return append([]string{l.wrapper, "-p", seatbeltProfile(policy)}, argv...), nil
	case "linux":
		return linuxConfine(l.wrapper, argv, policy)
	default:
		return argv, nil
	}
}

// seatbeltProfile builds a conservative SBPL profile from policy: deny by
// default, then allow read on ReadPaths, read+write on WritePaths, process-exec
// always, and network ONLY when AllowNetwork (default-deny egress, §4.6). Fidelity
// is best-effort on the degraded profile — see the package doc.
func seatbeltProfile(p hx.SandboxPolicy) string {
	var b strings.Builder
	b.WriteString("(version 1)")
	b.WriteString("(deny default)")
	// Read is allowed broadly: on macOS a process cannot even load (its binary,
	// dylibs, /dev, dyld cache) without wide read access, so a read-restricted
	// profile aborts every real command. The security this profile actually
	// enforces is on WRITES and NETWORK — the two ways a tool escapes its scope.
	// ReadPaths is retained as documentation of intent; write scoping is the teeth.
	b.WriteString("(allow file-read*)")
	// Running a command needs to exec its binary and fork subprocesses (a shell
	// runs pipelines and builtins by forking); both are process operations, not
	// filesystem or network reach.
	b.WriteString("(allow process-exec)")
	b.WriteString("(allow process-fork)")
	b.WriteString("(allow sysctl-read)")
	// Reaching the macOS Keychain. `(deny default)` blocks the mach-lookup the
	// Security framework needs to talk to securityd, and without this line a
	// confined tool that authenticates via the Keychain fails in a way that does
	// not look like a confinement failure at all. Measured, not theorised: Claude
	// Code run under this profile exits 1 with "Not logged in · Please run /login",
	// which reads as a user misconfiguration. Exactly this one global-name fixes it.
	//
	// `(allow file-read*)` above is NOT sufficient, and that is the trap: the
	// credential exists BOTH in ~/.claude/.credentials.json and in the login
	// keychain, and the client prefers the keychain — so a reader reasoning only
	// about the filesystem will conclude this line is redundant and delete it. It
	// is not. Scoped to SecurityServer deliberately: a blanket `(allow mach-lookup)`
	// also works but needlessly widens the sandbox.
	b.WriteString(`(allow mach-lookup (global-name "com.apple.SecurityServer"))`)
	// Writes are default-denied and re-allowed ONLY under the declared paths —
	// this is what keeps a tool inside its working directory.
	for _, path := range p.WritePaths {
		b.WriteString("(allow file-write* (subpath ")
		b.WriteString(quoteSeatbelt(path))
		b.WriteString("))")
	}
	// Egress is denied by default (§4.6); re-allowed only on explicit opt-in.
	if p.AllowNetwork {
		b.WriteString("(allow network*)")
	}
	return b.String()
}

// quoteSeatbelt renders path as an SBPL string literal, escaping backslashes and
// double quotes so a path can never break out of the literal.
func quoteSeatbelt(path string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(path) + `"`
}

// linuxConfine wraps argv for a linux mechanism. When the resolved binary is the
// landlock-run C binary it is used WHOLLY — the customer-profile swap carries its
// own default-deny ruleset and reads the policy out-of-band, so the degraded Go
// side only invokes it as a prefix. Otherwise it builds a conservative bwrap
// (bubblewrap) invocation: an empty mount namespace with only the policy's paths
// bound in, and the network namespace unshared unless AllowNetwork (default-deny
// egress). This whole branch is what a customer landlock-run deployment replaces.
func linuxConfine(wrapper string, argv []string, p hx.SandboxPolicy) ([]string, error) {
	if filepath.Base(wrapper) == landlockRun {
		return append([]string{wrapper}, argv...), nil
	}
	runtimeBindings, err := linuxRuntimeBindings("/")
	if err != nil {
		return nil, err
	}
	// Bubblewrap starts with an empty root. The fixed read-only OS runtime is
	// needed to execute the command, not permission to read the host home/root.
	out := append([]string{wrapper, "--unshare-pid", "--proc", "/proc", "--dev", "/dev", "--new-session"}, runtimeBindings...)
	for _, path := range p.ReadPaths {
		if path == "/dev/null" {
			continue // --dev already provides the private null device.
		}
		out = append(out, "--ro-bind", path, path)
	}
	for _, path := range p.WritePaths {
		if path == "/dev/null" {
			continue // Never replace the private device with the host device.
		}
		out = append(out, "--bind", path, path)
	}
	if !p.AllowNetwork {
		out = append(out, "--unshare-net") // default-deny egress
	}
	out = append(out, "--die-with-parent", "--")
	return append(out, argv...), nil
}

// lookPath resolves file against $PATH, returning the first executable match or
// "" if none. A minimal stdlib PATH scan — this package does NOT import os/exec.
func lookPath(file string) string {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		p := filepath.Join(dir, file)
		if info, err := os.Stat(p); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

// Compile-time proof that Local is a full hx.SandboxProvider.
var _ hx.SandboxProvider = (*Local)(nil)
