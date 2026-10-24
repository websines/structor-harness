// Package shell is the Forge harness bash capability (forge-harness-design-v0.1.md
// §4 execution boundary, §8 H2): it runs a shell command through the subprocess
// capability, confined by the sandbox capability whenever real confinement is in
// force on the host, and registers the model-facing `bash` tool over that
// executor.
//
// The package imports no sibling harness package (subprocess/fs/sandbox): the
// subprocess runner and sandbox provider are resolved from the runtime.Context by
// key (hx.KeySubprocess, hx.KeySandbox) and consumed only through the hx
// interfaces — the cordis "everything by key" discipline that keeps the spine
// decoupled at compile time. A no-op-with-record sandbox (Available() == false)
// is a normal state here, not an error: this executor simply leaves the argv
// unwrapped and declares nothing, which hands the decision to the subprocess
// capability — the last gate before a process exists, and the one that refuses to
// run a model-chosen command that nothing can confine.
package shell

import (
	"fmt"

	"github.com/websines/structor-harness/harness/hx"
)

// Bash is the hx.ShellExecutor: it runs one command line through a Subprocess,
// wrapping the argv with a SandboxProvider first when confinement is available.
// It holds no mutable state — the two collaborators are resolved once at
// composition time — so a single Bash is safe to share across concurrent calls.
type Bash struct {
	sub     hx.Subprocess
	sandbox hx.SandboxProvider
}

// New builds a Bash over a subprocess runner and a sandbox provider. sandbox may
// be nil or a provider whose Available() is false (no supported confinement on
// this host); either way Run hands the unwrapped argv to the subprocess runner
// with no confinement declared, and that runner decides whether it may execute.
func New(sub hx.Subprocess, sandbox hx.SandboxProvider) *Bash {
	return &Bash{sub: sub, sandbox: sandbox}
}

// Compile-time proof Bash satisfies the shared contract.
var _ hx.ShellExecutor = (*Bash)(nil)

// Run executes req.Command as `bash -lc <command>` and returns the subprocess
// outcome. The login shell (`-l`) loads the profile so PATH and friends are set
// before `-c` runs the command string; `/bin/sh -c` would be more portable but
// `bash` is what the harness targets, so we spell it explicitly.
//
// When a sandbox is available, the argv is wrapped via Confine under a
// conservative default-deny policy — read and write the working directory, no
// network — and the WRAPPED argv is what the subprocess runs. Whether confinement
// was applied is recorded by that wrapping: an unconfined call hands the plain
// argv straight through. A Confine failure is an operational fact (a non-empty
// Err), never a verdict, so it returns as a SpawnResult rather than panicking.
func (b *Bash) Run(req hx.ShellRequest) hx.SpawnResult {
	// Non-login (`-c`, not `-lc`): a tool command must not depend on or run the
	// user's login profile (/etc/profile, ~/.bash_profile) — those add nondeterminism
	// and, under the sandbox, force profile scripts to fork for no benefit.
	argv := []string{"bash", "-c", req.Command}

	// declared is what the SPAWNER is told about confinement. It stays "" unless
	// this executor actually wrapped the argv: the zero value means "nothing done,
	// confine it yourself", so on a host where Available() is false the spawner —
	// not this optimistic fallback — decides whether an unconfined bash command
	// runs at all. Claiming ConfineCaller here without wrapping would be a lie that
	// disarms that decision.
	declared := ""
	if b.sandbox != nil && b.sandbox.Available() {
		confined, err := b.sandbox.Confine(argv, confinePolicy(req.Dir))
		if err != nil {
			return hx.SpawnResult{ExitCode: -1, Err: fmt.Sprintf("shell: confine argv: %v", err)}
		}
		argv = confined
		declared = hx.ConfineCaller
	}

	return b.sub.Spawn(hx.SpawnRequest{
		Ctx:         req.Ctx,
		Argv:        argv,
		Dir:         req.Dir,
		Env:         req.Env,
		TimeoutMs:   req.TimeoutMs,
		Confinement: declared,
	})
}

// confinePolicy is the conservative default-deny policy a shell command runs
// under: the working directory is the only path it may read or write, and network
// is off. An empty dir yields no path of its own (the provider applies its own
// defaults) rather than a spurious "" path.
//
// /dev/null is always writable. Under a (deny default) profile it is not, and
// `2>/dev/null` — which a large share of real shell commands carry — then fails
// the whole command with "/dev/null: Operation not permitted" and exit 1, measured
// on this host. It reads as the command failing, not as the sandbox refusing.
// (Duplicated from subprocess.alwaysWritable on purpose and temporarily: the real
// home for it is the provider that builds the profile, one place for both
// platforms, which is under concurrent change.)
func confinePolicy(dir string) hx.SandboxPolicy {
	p := hx.SandboxPolicy{AllowNetwork: false, WritePaths: []string{"/dev/null"}}
	if dir != "" {
		p.ReadPaths = []string{dir}
		p.WritePaths = append(p.WritePaths, dir)
	}
	return p
}
