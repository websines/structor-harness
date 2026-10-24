package hx

import "context"

// The execution + I/O boundary (design §4 second wave, §8 H2): the capability
// interfaces for running real commands and editing real files, plus the sandbox
// seam that confines them. Concrete providers live in the subprocess/fs/shell/
// sandbox packages and are resolved by key; the fs and shell packages also
// register the model-facing tools (read_file/write_file/edit_file, bash).

// --- subprocess: spawn a local process, capture bounded output ---------------

// SpawnRequest is one process to run. Stdout/Stderr capture is bounded by the
// runtime; output beyond the cap is truncated (a real collector spills to CAS).
type SpawnRequest struct {
	Ctx       context.Context
	Argv      []string
	Dir       string
	Env       []string
	Stdin     []byte
	TimeoutMs int
	// OutputLimit overrides the per-stream capture limit in bytes. Non-positive
	// values use the spawner default; the spawner enforces a hard upper bound.
	OutputLimit int
	// Sandbox is the caller's DECLARED ACCESS: exactly the paths this command needs
	// to write and whether it needs egress. nil means the spawner derives a
	// default-deny policy from Dir instead.
	//
	// Declaring is the only sound way to reason about a confined run. A denial is
	// NOT recoverable from the outcome (see SpawnResult.Confinement), so "did the
	// sandbox get in the way?" cannot be answered after the fact — but "did the
	// command stay inside what it declared?" can be answered before it, and a
	// command that exceeds its own declaration is an attributable finding rather
	// than a mystery. Same shape as methodology.Requirement's Egress flag: declaring
	// more is allowed, and costs evidence grade.
	Sandbox *SandboxPolicy
	// Confinement declares what the CALLER has ALREADY done about confining Argv,
	// so the spawner neither confines it twice nor assumes it was confined at all.
	// The ZERO VALUE ("") means "nothing" — the spawner confines Argv itself, and
	// refuses to run it if it cannot; an omitted field is never an opt-out. The two
	// non-empty values are ConfineCaller (the caller wrapped Argv with a provider
	// whose Available() it checked) and ConfineNone (a deliberate, documented
	// opt-out for an argv that cannot run confined). Whatever is declared here, the
	// spawner reports what ACTUALLY happened in SpawnResult.Confinement.
	Confinement string
}

// SpawnResult is the outcome of a process. TimedOut and a non-empty Err are
// operational facts, not verdicts (Law 3): a tool turns them into evidence, the
// resolver never reads them directly.
type SpawnResult struct {
	ExitCode        int
	Stdout          []byte
	Stderr          []byte
	TimedOut        bool
	StdoutTruncated bool
	StderrTruncated bool
	StdoutBytes     int
	StderrBytes     int
	Err             string
	// Confinement records what was ACTUALLY in force when this process ran — one of
	// ConfineSpawner, ConfineCaller, ConfineNone, or ConfineRefused (nothing ran).
	// It exists so "it ran unconfined" can never be a silent outcome: a caller,
	// and the record built from it, can always tell a confined run from a degraded
	// one without re-deriving the host's capabilities.
	//
	// WHAT IT DOES NOT TELL YOU, and nothing else does either: whether a confined
	// process was DENIED something while it ran. Measured on this host under
	// seatbelt — a denied write surfaces as exit 1 with "Operation not permitted"
	// when the program reports errno, and as exit 0 when it ignores the error
	// (`… 2>/dev/null; exit 0`), while a genuine failure is an ordinary non-zero
	// exit; and `log show` yields no sandbox-deny entries to consult out of band.
	// So the inference runs wrong in BOTH directions: reading a denial as failure
	// fabricates a counterexample, and reading one as success discharges a check
	// that never ran. NEVER infer a denial from ExitCode. ConfineRefused (with a
	// non-empty Err) is the one confinement outcome that IS sound to branch on,
	// because then no process existed at all. To reason about the rest, declare
	// access up front (SpawnRequest.Sandbox) and canary the policy (SandboxCanary).
	Confinement string
}

// The confinement vocabulary shared by SpawnRequest.Confinement (what the caller
// already did) and SpawnResult.Confinement (what actually happened). One set of
// names for both directions, so a value read off a result can be handed straight
// back on a request without translation.
const (
	// ConfineSpawner: the spawner wrapped the argv itself with an available
	// SandboxProvider. Result-only.
	ConfineSpawner = "spawner"
	// ConfineCaller: the caller wrapped the argv before handing it over, having
	// checked SandboxProvider.Available() first. The spawner passes it through
	// unwrapped — nesting one mechanism inside another is not composition, it is
	// two profiles fighting (nested bwrap needs nested user namespaces and fails).
	ConfineCaller = "caller"
	// ConfineNone: the process ran WITHOUT OS confinement — either a caller
	// declared the opt-out for an argv that cannot run confined, or the host has no
	// mechanism and unconfined execution was explicitly enabled. It is a fact on
	// the result, never an omission.
	ConfineNone = "none"
	// ConfineRefused: nothing ran, because the spawner refused to run it — the
	// hard-deny floor tripped, or the argv had to be confined and could not be.
	// Result-only; Err carries the reason.
	ConfineRefused = "refused"
)

// Subprocess (ctx.Get(KeySubprocess)) runs local processes with a process-group
// tree-kill on cancel/timeout so no child outlives its call.
type Subprocess interface {
	Spawn(req SpawnRequest) SpawnResult
}

// SandboxCanary is the OPTIONAL companion of Subprocess (type-assert on the
// resolved service; a fake in a test need not implement it). Canary runs a
// trivial, known-good command under EXACTLY the policy proto would receive and
// returns its outcome.
//
// It answers one question and only one: is the policy itself functional on this
// host? It cannot detect a per-command denial — nothing can (see
// SpawnResult.Confinement) — but it does catch whole-policy breakage, which is a
// failure mode already observed here: the seatbelt profile denied the Keychain
// mach-lookup, so every confined agent run exited 1 reporting "Not logged in",
// indistinguishable from the agent simply failing. A caller building a verdict
// from a confined run should canary first, and when the canary fails the honest
// verdict is UNVERIFIABLE — never falsified.
type SandboxCanary interface {
	Canary(proto SpawnRequest) SpawnResult
}

// --- sandbox: confine an argv before it runs ---------------------------------

// SandboxPolicy declares what a confined process may touch. Default-deny: only
// the listed paths are reachable and network is off unless AllowNetwork.
type SandboxPolicy struct {
	ReadPaths    []string
	WritePaths   []string
	AllowNetwork bool
}

// SandboxProvider (ctx.Get(KeySandbox)) transforms an argv into a confined argv
// (e.g. wrapping with landlock-run/bwrap on Linux or sandbox-exec on macOS). On a
// platform with no supported mechanism, Available() is false and callers decide
// whether to proceed unconfined (recording that they did) or refuse.
type SandboxProvider interface {
	// Confine returns the argv to actually exec to run argv under policy.
	Confine(argv []string, policy SandboxPolicy) ([]string, error)
	// Available reports whether real confinement is in force on this host.
	Available() bool
}

// --- filesystem: read / write / literal-edit --------------------------------

// FileSystem (ctx.Get(KeyFS)) is the agent's file capability. Edits are LITERAL
// oldString→newString replacements (never diffs), and a write follows a
// read-before-write policy so an agent cannot blindly clobber a file.
type FileSystem interface {
	ReadText(path string) (string, error)
	WriteText(path, content string) error
	// EditText replaces the UNIQUE occurrence of oldStr with newStr; a zero or
	// ambiguous match is an error (never a silent partial edit).
	EditText(path, oldStr, newStr string) error
}

// --- shell: run a bash command (through subprocess + sandbox) -----------------

// ShellRequest is one shell command.
type ShellRequest struct {
	Ctx       context.Context
	Env       []string
	Command   string
	Dir       string
	TimeoutMs int
}

// ShellExecutor (ctx.Get(KeyShell)) runs a bash command via the subprocess
// capability, confined by the sandbox capability when one is available.
type ShellExecutor interface {
	Run(req ShellRequest) SpawnResult
}
