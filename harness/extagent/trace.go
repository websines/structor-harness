package extagent

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/harness/subprocess"
	"github.com/websines/structor-harness/record"
)

type Trace struct {
	ID              string            `json:"id"`
	PhaseID         string            `json:"phaseId"`
	Agent           string            `json:"agent"`
	SessionID       string            `json:"sessionId,omitempty"`
	Model           string            `json:"model,omitempty"`
	Usage           []json.RawMessage `json:"usage,omitempty"`
	Stdout          string            `json:"stdout"`
	Stderr          string            `json:"stderr"`
	ExitCode        int               `json:"exitCode"`
	Cancelled       bool              `json:"cancelled"`
	TimedOut        bool              `json:"timedOut"`
	ElapsedMS       int64             `json:"elapsedMs"`
	TimeoutMS       int64             `json:"timeoutMs"`
	PromptTransport string            `json:"promptTransport,omitempty"`
	PromptBytes     int               `json:"promptBytes,omitempty"`
	PromptDigest    string            `json:"promptDigest,omitempty"`
	Confinement     string            `json:"confinement"`
	Error           string            `json:"error,omitempty"`
	StdoutTruncated bool              `json:"stdoutTruncated"`
	StderrTruncated bool              `json:"stderrTruncated"`
	StdoutBytes     int               `json:"stdoutBytes"`
	StderrBytes     int               `json:"stderrBytes"`
}

// BuilderWithTrace keeps the installed CLI account and records its emitted
// events. The outer Forge admission layer owns the local unconfined opt-in.
// Reusing the existing process runner gives this seam bounded capture and whole
// process-group cancellation without adding another executor.
func BuilderWithTrace(name, workDir string, save func(Trace) error) (func(context.Context, string, string) (string, error), error) {
	return BuilderWithTraceConfig(name, workDir, Config{}, save)
}

// BuilderWithTraceConfig runs the same installed CLI and preserves transcript
// capture and owned process-group cancellation with a bounded operator budget.
func BuilderWithTraceConfig(name, workDir string, cfg Config, save func(Trace) error) (func(context.Context, string, string) (string, error), error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	timeout := cfg.TurnTimeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	sp, ok := lookup(name)
	if !ok {
		return nil, fmt.Errorf("extagent: unknown agent %q (have %v)", name, Supported())
	}
	if _, err := exec.LookPath(sp.Bin); err != nil {
		return nil, fmt.Errorf("extagent: %q not on PATH: %w", sp.Bin, err)
	}
	return func(ctx context.Context, task, phase string) (string, error) {
		var extraArgs, env []string
		if cfg.Prepare != nil {
			var cleanup func()
			var err error
			extraArgs, env, cleanup, err = cfg.Prepare(ctx)
			if cleanup != nil {
				defer cleanup()
			}
			if err != nil {
				return "", err
			}
		}
		started := time.Now()
		prompt := []byte(task)
		transport := "argv"
		var stdin []byte
		if sp.stdin {
			transport = "stdin"
			stdin = prompt
		}
		argv := append([]string{sp.Bin}, sp.args(workDir, task)...)
		argv = append(argv, extraArgs...)
		result := subprocess.New(nil).Spawn(hx.SpawnRequest{Ctx: ctx, Dir: workDir, Argv: argv, Env: env, Stdin: stdin, TimeoutMs: int(timeout.Milliseconds()), OutputLimit: 4 << 20, Confinement: hx.ConfineNone})
		trace := Trace{ID: "external:" + rand.Text(), PhaseID: phase, Agent: sp.Name, Stdout: string(result.Stdout), Stderr: string(result.Stderr), ExitCode: result.ExitCode, Cancelled: ctx.Err() != nil, TimedOut: result.TimedOut, ElapsedMS: time.Since(started).Milliseconds(), TimeoutMS: timeout.Milliseconds(), Confinement: result.Confinement, Error: result.Err}
		trace.PromptTransport, trace.PromptBytes, trace.PromptDigest = transport, len(prompt), string(record.ComputeDigest(prompt))
		text, parseErr := trace.parse()
		trace.StdoutTruncated, trace.StderrTruncated = result.StdoutTruncated, result.StderrTruncated
		trace.StdoutBytes, trace.StderrBytes = result.StdoutBytes, result.StderrBytes
		runErr := parseErr
		if ctx.Err() != nil {
			runErr = ctx.Err()
		} else if result.TimedOut {
			runErr = context.DeadlineExceeded
		} else if result.Err != "" {
			runErr = errors.New(result.Err)
		} else if result.ExitCode != 0 {
			runErr = fmt.Errorf("extagent %s (%s): exit %d\n%s", sp.Name, phase, result.ExitCode, tail(trace.Stderr, 1500))
		}
		if result.StdoutTruncated {
			runErr = errors.Join(runErr, fmt.Errorf("extagent %s (%s): stdout capture truncated (%d bytes)", sp.Name, phase, result.StdoutBytes))
		}
		if runErr != nil {
			trace.Error = runErr.Error()
		}
		if save != nil {
			if err := save(trace); err != nil {
				runErr = errors.Join(runErr, fmt.Errorf("extagent: persist transcript: %w", err))
			}
		}
		return trunc(strings.TrimSpace(text), 12000), runErr
	}, nil
}

func (trace *Trace) parse() (string, error) {
	var summary string
	var parseErr error
	for line := range strings.SplitSeq(trace.Stdout, "\n") {
		// Agent CLIs can report a failed turn in their JSON protocol even when
		// the process exits successfully. Preserve that terminal failure.
		var failure struct {
			Type    string          `json:"type"`
			IsError bool            `json:"is_error"`
			Result  string          `json:"result"`
			Message json.RawMessage `json:"message"`
			Error   struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(line), &failure) == nil &&
			(failure.Type == "error" || failure.Type == "turn.failed" || (failure.Type == "result" && failure.IsError)) {
			message := failure.Result
			if message == "" {
				message = failure.Error.Message
			}
			if message == "" {
				_ = json.Unmarshal(failure.Message, &message)
			}
			if message == "" {
				message = failure.Type
			}
			parseErr = errors.Join(parseErr, fmt.Errorf("extagent %s: %s", trace.Agent, message))
		}
		var event struct {
			Type    string          `json:"type"`
			Session string          `json:"session_id"`
			Thread  string          `json:"thread_id"`
			Model   string          `json:"model"`
			Result  string          `json:"result"`
			Usage   json.RawMessage `json:"usage"`
			Message struct {
				Model   string `json:"model"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
			Item struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		if event.Session != "" {
			trace.SessionID = event.Session
		}
		if event.Thread != "" {
			trace.SessionID = event.Thread
		}
		if event.Model != "" {
			trace.Model = event.Model
		}
		if event.Message.Model != "" {
			trace.Model = event.Message.Model
		}
		if len(event.Usage) > 0 && string(event.Usage) != "null" {
			trace.Usage = append(trace.Usage, append(json.RawMessage{}, event.Usage...))
		}
		if event.Result != "" {
			summary = event.Result
		}
		if event.Type == "assistant" {
			for _, block := range event.Message.Content {
				if block.Type == "text" {
					summary = block.Text
				}
			}
		}
		if event.Item.Type == "agent_message" && event.Item.Text != "" {
			summary = event.Item.Text
		}
	}
	if summary == "" {
		summary = trace.Stdout
	}
	return summary, parseErr
}
