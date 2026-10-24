package harnesseval

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/harness/improve"
	"github.com/websines/structor-harness/harness/subprocess"
	"github.com/websines/structor-harness/record"
)

type Fixture struct {
	Task         improve.Task `json:"task"`
	Split        string       `json:"split"`
	Provenance   string       `json:"provenance"`
	Source       Source       `json:"source"`
	Gold         Source       `json:"gold"`
	Mutations    []Source     `json:"mutations"`
	PrivateTests string       `json:"privateTests"`
}
type Evaluation struct {
	Artifact      record.Digest `json:"artifact"`
	Checker       record.Digest `json:"checker"`
	Scored        bool          `json:"scored"`
	Passed        bool          `json:"passed"`
	Integrity     bool          `json:"integrity"`
	Reason        string        `json:"reason,omitempty"`
	Diagnostics   string        `json:"diagnostics,omitempty"`
	DiagnosticRef record.Digest `json:"diagnosticRef,omitempty"`
}

var ErrInfrastructure = errors.New("harnesseval: evaluation infrastructure unavailable")

func TestSource(ctx context.Context, source Source) (hx.SpawnResult, error) {
	var result hx.SpawnResult
	dir, err := os.MkdirTemp("", "forge-behavior-*")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(dir)
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return result, err
	}
	if err = source.Materialize(dir); err != nil {
		return result, err
	}
	if err = seedToolchain(ctx, dir, ".forge/go-cache"); err != nil {
		return result, err
	}
	confiner, err := NewConfiner(dir)
	if err != nil {
		return result, err
	}
	if err = confiner.Preflight(ctx); err != nil {
		return result, err
	}
	if err = os.MkdirAll(filepath.Join(dir, ".forge", "go-cache"), 0700); err != nil {
		return result, err
	}
	result = subprocess.New(confiner).Spawn(hx.SpawnRequest{Ctx: ctx, Argv: []string{filepath.Join(runtime.GOROOT(), "bin", "go"), "test", "-json", "-count=1", "-timeout=20s", "."}, Dir: dir, Env: Environment(dir), TimeoutMs: 60000})
	if result.Err != "" {
		return result, errors.New(result.Err)
	}
	return result, nil
}

func (f Fixture) Evaluate(ctx context.Context, cas record.CAS, artifact record.Digest) (Evaluation, error) {
	checker, _ := json.Marshal(struct{ Tests, Command, Go, Confinement string }{f.PrivateTests, "protected go test; input-only go build; isolated program query; host comparison", runtime.Version(), "forge-pilot-strict-v1+blackbox-v1"})
	e := Evaluation{Artifact: artifact, Checker: record.ComputeDigest(checker)}
	source, err := LoadSource(cas, artifact)
	if err != nil {
		return e, errors.Join(ErrInfrastructure, err)
	}
	e.Integrity = true
	if reason := protectedSource(f.Source, source); reason != "" {
		e.Scored = true
		e.Integrity = false
		e.Reason = reason
		return e, nil
	}
	cases, err := behaviorCases(f.PrivateTests)
	if err != nil {
		e.Reason = err.Error()
		return e, errors.Join(ErrInfrastructure, err)
	}
	public, err := TestSource(ctx, source)
	if err != nil {
		e.Reason = err.Error()
		return e, errors.Join(ErrInfrastructure, err)
	}
	e.Scored = true
	e.Passed = public.ExitCode == 0 && !public.TimedOut && passedTest(public.Stdout, "TestPublic")
	var diagnostics strings.Builder
	diagnostics.Write(public.Stdout)
	diagnostics.Write(public.Stderr)
	if e.Passed {
		expressions := make([]string, len(cases))
		for i, c := range cases {
			expressions[i] = c.Expression
		}
		program, build, buildErr := buildProgram(ctx, source, expressions)
		if buildErr != nil {
			e.Scored = false
			e.Reason = buildErr.Error()
			return e, errors.Join(ErrInfrastructure, buildErr)
		}
		diagnostics.Write(build.Stdout)
		diagnostics.Write(build.Stderr)
		if program == nil {
			e.Passed = false
		} else {
			defer program.Close()
			for i, c := range cases {
				actual, completed, run := program.query(ctx, i)
				if run.Err != "" {
					e.Scored = false
					e.Reason = run.Err
					return e, errors.Join(ErrInfrastructure, errors.New(run.Err))
				}
				if !completed || actual != c.Want {
					e.Passed = false
					diagnostics.WriteString(fmt.Sprintf("case %d: completed=%v, exit=%d, timeout=%v, got %q, expected %q\n", i, completed, run.ExitCode, run.TimedOut, actual, c.Want))
					diagnostics.Write(run.Stderr)
				}
			}
		}
	}
	e.Diagnostics = diagnostics.String()
	e.DiagnosticRef, err = cas.Put([]byte(e.Diagnostics))
	if err != nil {
		e.Scored = false
		return e, err
	}
	e.Diagnostics, _ = excerpt(e.Diagnostics, 4096)
	if !e.Passed {
		e.Reason = "behavioral checks failed"
	}
	return e, nil
}

func protectedSource(original, candidate Source) string {
	for path, text := range original {
		actual, exists := candidate[path]
		if (strings.HasSuffix(path, "_test.go") || path == "go.mod" || path == "go.sum") && (!exists || actual != text) {
			return "protected public test/dependency changed"
		}
	}
	if _, exists := candidate["forge_acceptance_test.go"]; exists {
		return "reserved private test filename"
	}
	return ""
}

func passedTest(output []byte, name string) bool {
	s := bufio.NewScanner(bytes.NewReader(output))
	s.Buffer(make([]byte, 4096), 1<<20)
	for s.Scan() {
		var event struct {
			Action string
			Test   string
		}
		if json.Unmarshal(s.Bytes(), &event) == nil && event.Action == "pass" && event.Test == name {
			return true
		}
	}
	return false
}

// Validate proves the grader distinguishes the original bug, the gold repair and
// each declared negative mutation. It does not call a language model.
func (f Fixture) Validate(ctx context.Context, cas record.CAS) error {
	if f.Task.ID == "" || f.Task.Prompt == "" || f.Provenance == "" || (f.Split != "development" && f.Split != "evaluation") || len(f.Mutations) == 0 {
		return errors.New("harnesseval: incomplete fixture")
	}
	for _, check := range []struct {
		source Source
		passed bool
	}{{f.Source, false}, {f.Gold, true}} {
		d, err := check.source.Store(cas)
		if err != nil {
			return err
		}
		e, err := f.Evaluate(ctx, cas, d)
		if err != nil {
			return err
		}
		if !e.Scored || e.Passed != check.passed {
			return errors.New("harnesseval: original/gold result did not match fixture contract: " + f.Task.ID + " " + e.Diagnostics)
		}
	}
	for _, bad := range f.Mutations {
		d, err := bad.Store(cas)
		if err != nil {
			return err
		}
		e, err := f.Evaluate(ctx, cas, d)
		if err != nil {
			return err
		}
		if !e.Scored || e.Passed {
			return errors.New("harnesseval: negative mutation survived: " + f.Task.ID)
		}
	}
	return nil
}
