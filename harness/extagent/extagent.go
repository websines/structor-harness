// Package extagent plugs a THIRD-PARTY coding agent (Claude Code, Codex, opencode)
// into the feature loop as the untrusted BUILD proposer — the "who proposes" seam,
// sibling to the pluggable methodology ("who plans") over the fixed proof kernel
// ("who proves"). The external agent runs headless in the workspace and writes real
// files; Forge's methodology still owns spec/probe/verify/gate, and the resolver still
// derives the verdict. The agent's private way of working is invisible to the kernel
// (Law 3, no fiat) — only the diff it leaves is real, and the SYSTEM proves that.
//
// This is the loop-level builder seam (feature.Config.Builder): the smallest honest
// integration — Forge hands the external agent a task, it edits WorkDir, Forge verifies.
// A future ACP adapter realises the same intent at the hx.AgentFactory seam for OBSERVED
// tool calls (streaming, permissions) instead of outcome-only. Adds no Go module deps.
package extagent

import (
	"context"
	"errors"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// DefaultTimeout bounds one build turn — one external-agent invocation. Generous
// because a coding agent may make several model calls per turn.
const DefaultTimeout = 8 * time.Minute

// MaxTimeout bounds an operator-selected external agent turn.
const MaxTimeout = time.Hour

// Config controls the time budget for a real installed agent invocation. A zero
// TurnTimeout preserves DefaultTimeout; it never means an unbounded invocation.
type Config struct {
	TurnTimeout time.Duration
	// Prepare supplies invocation-scoped capabilities, disposed after the process
	// joins. It is a trusted composition seam, never model-supplied configuration.
	Prepare func(context.Context) (args, env []string, cleanup func(), err error)
}

// Validate refuses durations that cannot be represented by the millisecond
// subprocess budget or that exceed the maximum owned turn duration.
func (cfg Config) Validate() error {
	if cfg.TurnTimeout != 0 && (cfg.TurnTimeout < time.Millisecond || cfg.TurnTimeout > MaxTimeout) {
		return errors.New("extagent: turn timeout must be zero or between one millisecond and one hour")
	}
	return nil
}

// Spec describes how to invoke one external coding agent non-interactively in a
// workspace directory. args builds the argv after Bin for a task; the process cwd is
// set to workDir so the agent edits the same tree Forge's acceptance check verifies.
type Spec struct {
	Name string
	Bin  string
	args func(workDir, task string) []string
	// stdin carries freeform task text separately from the executable command.
	stdin bool
}

// specs is the built-in registry of supported agents. Each invocation is headless and
// auto-applies edits within the workspace: Claude Code `-p` + bypassPermissions, Codex
// `exec` with the workspace-write sandbox, opencode `run`.
//
// bypassPermissions grants NO privilege the user has not already granted: this is the
// bring-your-own-agent seam, so Forge is invoking a binary the user already runs
// unconfined on their own machine, as themselves, with their own credentials. The
// prompt it suppresses is the agent's own UX, not a boundary Forge is entitled to
// claim — nothing here confines the agent (extagent spawns it with a plain
// exec.CommandContext), and the run screen says so.
//
// What makes that acceptable is the trust model, not a sandbox: the external agent is
// the UNTRUSTED PROPOSER. Its output is a CLAIM, never evidence. Forge's methodology
// still owns spec/probe/verify/gate and the resolver still derives the verdict from
// evidence the SYSTEM produced, so an agent that lies, or that edits outside the
// workspace, cannot manufacture a discharge — it can only fail to earn one.
var specs = map[string]Spec{
	"claude-code": {Name: "claude-code", Bin: "claude", stdin: true, args: func(_, _ string) []string {
		return []string{"-p", "--permission-mode", "bypassPermissions", "--output-format", "stream-json", "--verbose"}
	}},
	"codex": {Name: "codex", Bin: "codex", stdin: true, args: func(workDir, _ string) []string {
		return []string{"exec", "--json", "--sandbox", "workspace-write", "--skip-git-repo-check", "-C", workDir, "-"}
	}},
	"opencode": {Name: "opencode", Bin: "opencode", args: func(_, task string) []string {
		return []string{"run", task}
	}},
}

// aliases maps friendly selectors to a canonical spec key.
var aliases = map[string]string{"claude": "claude-code", "claudecode": "claude-code"}

func lookup(name string) (Spec, bool) {
	if c, ok := aliases[name]; ok {
		name = c
	}
	sp, ok := specs[name]
	return sp, ok
}

// Supported lists the registered agent selectors, sorted.
func Supported() []string {
	out := make([]string, 0, len(specs))
	for k := range specs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Available reports whether the named agent is known and its binary is on PATH.
func Available(name string) bool {
	sp, ok := lookup(name)
	if !ok {
		return false
	}
	_, err := exec.LookPath(sp.Bin)
	return err == nil
}

// Builder returns a feature.Config.Builder that drives the named external agent for
// every build/author/specify turn, running it headless in workDir. The returned string
// is the agent's stdout (trimmed) — used only for the run's summary/log. The SYSTEM
// re-runs the acceptance check to decide the verdict, so a chatty or over-eager agent
// cannot forge a pass: the agent proposes, the kernel proves.
func Builder(name, workDir string) (func(ctx context.Context, task, sessID string) (string, error), error) {
	return BuilderWithTrace(name, workDir, nil)
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
