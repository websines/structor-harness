package shell

import (
	"encoding/json"
	"fmt"

	"github.com/websines/structor-harness/harness/hx"
)

// bashToolName is the model-facing name of the shell tool.
const bashToolName = "bash"

// maxToolOutput bounds the stdout/stderr each surfaced to the model (16 KiB per
// stream). The subprocess collector already caps and spills the full stream to
// CAS; this is a second, tool-level cap so a single result value stays small and
// the model sees a truncation flag rather than a wall of bytes.
const maxToolOutput = 16 * 1024

// bashArgs is the JSON argument shape the model produces. Command is required;
// TimeoutMs is optional (0 = the runtime's default per-call timeout).
type bashArgs struct {
	Command   string `json:"command"`
	TimeoutMs int    `json:"timeout_ms"`
}

// bashResult is the model-visible JSON result of a bash call. A non-zero exit is
// ordinary DATA here (ExitCode carries it); Error is set only when the process
// could not be run at all (a spawn failure), which the tool also marks IsError.
type bashResult struct {
	ExitCode        int    `json:"exit_code"`
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	TimedOut        bool   `json:"timed_out,omitempty"`
	StdoutTruncated bool   `json:"stdout_truncated,omitempty"`
	StderrTruncated bool   `json:"stderr_truncated,omitempty"`
	Error           string `json:"error,omitempty"`
}

// BashTool is the hx.ToolDefinition for `bash`: it runs the model's command line
// through sh and returns the summary. A non-zero exit code is reported as data (a
// command that ran and failed is evidence, not a tool error, Law 3); only a spawn
// failure — the process could not be started — is surfaced as ToolResult{IsError}.
// A malformed args payload is the one case that returns a Go error (the pipeline
// turns it into an error result).
func BashTool(sh hx.ShellExecutor) hx.ToolDefinition {
	return BashToolAt(sh, "")
}

// BashToolAt binds cwd at composition time; a model cannot widen the workspace.
func BashToolAt(sh hx.ShellExecutor, workDir string) hx.ToolDefinition {
	return BashToolAtWithEnvironment(sh, workDir, nil)
}

func BashToolAtWithEnvironment(sh hx.ShellExecutor, workDir string, base []string) hx.ToolDefinition {
	if base != nil {
		base = append([]string{}, base...)
	}
	return hx.ToolDefinition{
		Schema: hx.ToolSchema{
			Name:        bashToolName,
			Description: "Run a shell command with bash and return its exit code and (bounded) stdout/stderr.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{
						"type":        "string",
						"description": "The shell command line to run.",
					},
					"timeout_ms": map[string]any{
						"type":        "integer",
						"description": "Optional per-call timeout in milliseconds (0 = runtime default).",
					},
				},
				"required": []any{"command"},
			},
		},
		Execute: func(rc hx.ToolRunContext, raw []byte) (hx.ToolResult, error) {
			var args bashArgs
			if err := json.Unmarshal(raw, &args); err != nil {
				return hx.ToolResult{}, fmt.Errorf("shell: bash arguments: %w", err)
			}
			if args.Command == "" {
				return hx.ToolResult{}, fmt.Errorf("shell: bash requires a non-empty command")
			}
			var environment []string
			var err error
			if base == nil {
				environment, err = WorkspaceEnvironment(workDir)
			} else {
				environment, err = WorkspaceEnvironmentFrom(workDir, base)
			}
			if err != nil {
				return hx.ToolResult{}, err
			}

			res := sh.Run(hx.ShellRequest{
				Ctx:       rc.Ctx,
				Dir:       workDir,
				Env:       environment,
				Command:   args.Command,
				TimeoutMs: args.TimeoutMs,
			})

			value, err := json.Marshal(summarize(res))
			if err != nil {
				return hx.ToolResult{}, fmt.Errorf("shell: marshal bash result: %w", err)
			}
			// A spawn failure (Err set) is a tool error; a plain non-zero exit is not.
			return hx.ToolResult{IsError: res.Err != "", Value: value}, nil
		},
	}
}

// RegisterTool registers the bash tool on tr and returns its disposer — the
// effect the shell component tracks so the tool withdraws LIFO on unload.
func RegisterTool(sh hx.ShellExecutor, tr hx.ToolRuntime) (dispose func(), err error) {
	return tr.Register(BashTool(sh))
}

// summarize projects a SpawnResult into the bounded model-visible result.
func summarize(res hx.SpawnResult) bashResult {
	stdout, stdoutTrunc := boundOutput(res.Stdout)
	stderr, stderrTrunc := boundOutput(res.Stderr)
	return bashResult{
		ExitCode:        res.ExitCode,
		Stdout:          stdout,
		Stderr:          stderr,
		TimedOut:        res.TimedOut,
		StdoutTruncated: stdoutTrunc,
		StderrTruncated: stderrTrunc,
		Error:           res.Err,
	}
}

// boundOutput caps b at maxToolOutput bytes, reporting whether it was truncated.
func boundOutput(b []byte) (string, bool) {
	if len(b) <= maxToolOutput {
		return string(b), false
	}
	return string(b[:maxToolOutput]), true
}
