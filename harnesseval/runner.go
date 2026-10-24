package harnesseval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/websines/structor-harness/flg"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/websines/structor-harness/harness/compaction"
	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/harness/improve"
	"github.com/websines/structor-harness/harness/native"
	"github.com/websines/structor-harness/harness/session"
	"github.com/websines/structor-harness/harness/skill"
	"github.com/websines/structor-harness/harness/spine"
	"github.com/websines/structor-harness/record"
)

type Runner struct {
	CAS              record.CAS
	Adapter          hx.LlmAdapter
	Model            string
	StateDir         string
	Principal        string
	Timeout          time.Duration
	MaxSteps         int
	BaseInstructions string
}

func (r *Runner) Execute(ctx context.Context, task improve.Task, p improve.Profile, id string) (improve.Run, error) {
	profileDigest, err := improve.Digest(p)
	result := improve.Run{ID: id, Source: task.Source, Profile: profileDigest, PublicTotal: 1}
	if err != nil {
		return result, err
	}
	if err = p.Validate(p.Base); err != nil {
		return result, err
	}
	if r.CAS == nil || r.Adapter == nil || !filepath.IsAbs(r.StateDir) || r.Model == "" || r.Principal == "" || r.Timeout <= 0 || r.MaxSteps < 1 {
		return result, errors.New("harnesseval: incomplete native runner")
	}
	source, err := LoadSource(r.CAS, task.Source)
	if err != nil {
		return result, err
	}
	work, err := os.MkdirTemp(r.StateDir, "execution-*")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(work)
	work, err = filepath.EvalSymlinks(work)
	if err != nil {
		return result, err
	}
	if err = source.Materialize(work); err != nil {
		return result, err
	}
	if err = seedToolchain(ctx, work, ".forge/tool-cache/go"); err != nil {
		return result, err
	}
	confiner, err := NewConfiner(work)
	if err != nil {
		return result, err
	}
	if err = confiner.Preflight(ctx); err != nil {
		return result, err
	}
	skills, err := os.MkdirTemp(r.StateDir, "profile-*")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(skills)
	for _, s := range p.Skills {
		// Validated names never become arbitrary paths. The model's file/shell
		// tools cannot mutate this directory outside the execution workspace.
		dir := filepath.Join(skills, s.Name)
		if err = os.Mkdir(dir, 0700); err != nil {
			return result, err
		}
		text := fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n%s\n", s.Name, s.Description, s.Body)
		if err = os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(text), 0400); err != nil {
			return result, err
		}
	}
	sections := []spine.Section{{Order: 0, Text: r.BaseInstructions}, {Order: 800, Text: p.Guidance}, {Order: 810, Text: p.ToolGuidance}, {Order: 1100, Text: "Your repository workspace is " + work + "."}}
	execution, runErr := native.Run(ctx, native.Request{
		Options: spine.Options{CAS: r.CAS, Adapter: r.Adapter, Actor: session.Config{PrincipalID: r.Principal}, Exec: true, WorkDir: work,
			ContextStore: flg.NewStore(r.CAS), ContextQuery: func() string { return task.Prompt }, ContextOrder: 1000,
			ExecSandbox: confiner, ExecEnvironment: Environment(work), SelfCorrect: true, Sections: sections, SkillRoots: []skill.Root{{Dir: skills, Rank: 10}},
			Compaction: &compaction.Engine{MaxTokens: 48000}, SummaryProvider: r.Adapter.Route(), SummaryModel: r.Model},
		Task: task.Prompt, Model: r.Model, Principal: r.Principal, SessionID: id, MaxSteps: r.MaxSteps, Timeout: r.Timeout,
	})
	if runErr != nil {
		result.Error = runErr.Error()
		var terminal *hx.TurnError
		switch {
		case errors.Is(runErr, improve.ErrBudget):
			result.Fault = "budget"
		case errors.Is(runErr, context.DeadlineExceeded) || errors.Is(runErr, context.Canceled):
			result.Fault = "timeout"
		case errors.As(runErr, &terminal) && terminal.Reason == "provider_error":
			result.Fault = "provider"
		default:
			result.Fault = "execution"
		}
	}
	transcript, err := json.Marshal(execution)
	if err != nil {
		return result, err
	}
	result.Transcript, err = r.CAS.Put(transcript)
	if err != nil {
		return result, err
	}
	output, err := Capture(work)
	if err != nil {
		result.Error = "invalid output artifact: " + err.Error()
		result.Fault = "invalid"
		return result, nil
	}
	result.Artifact, err = output.Store(r.CAS)
	if err != nil {
		return result, err
	}
	// Public checks run against protected original tests in another environment.
	if reason := protectedSource(source, output); reason != "" {
		result.IntegrityViolation = true
		if result.Error == "" {
			result.Error = reason
			result.Fault = "integrity"
		}
		return result, nil
	}
	check, err := TestSource(ctx, output)
	if err != nil {
		return result, err
	}
	result.BuildOK = check.ExitCode == 0 && passedTest(check.Stdout, "TestPublic")
	if result.BuildOK {
		result.PublicPassed = 1
	}
	return result, nil
}

// ModelEditor sees only the explicitly supplied public records. It has no file
// tools or private evaluator reference and cannot publish its output.
type ModelEditor struct {
	Adapter hx.LlmAdapter
	Model   string
	CAS     record.CAS
	Timeout time.Duration
}

func (e *ModelEditor) Edit(ctx context.Context, request improve.EditRequest) ([]byte, error) {
	if e.Adapter == nil || e.CAS == nil || e.Model == "" || e.Timeout <= 0 {
		return nil, errors.New("harnesseval: incomplete editor")
	}
	ctx, cancel := context.WithTimeout(ctx, e.Timeout)
	defer cancel()
	type evidence struct {
		Run        improve.Run     `json:"run"`
		Artifact   json.RawMessage `json:"artifact"`
		Transcript json.RawMessage `json:"transcript"`
	}
	var records []evidence
	runs := []improve.Run{request.Reference}
	for _, c := range request.Candidates {
		runs = append(runs, c.Run)
	}
	for _, r := range runs {
		var item evidence
		item.Run = r
		for _, target := range []struct {
			d   record.Digest
			out *json.RawMessage
		}{{r.Artifact, &item.Artifact}, {r.Transcript, &item.Transcript}} {
			if !improve.ValidDigest(target.d) {
				continue
			}
			data, err := e.CAS.Get(target.d)
			if err != nil {
				return nil, err
			}
			if target.d == r.Transcript {
				*target.out, err = RenderTranscript(data)
			} else {
				*target.out, err = RenderArtifact(data)
			}
			if err != nil {
				return nil, err
			}
		}
		records = append(records, item)
	}
	payload, err := json.Marshal(struct {
		Request  improve.EditRequest `json:"request"`
		Evidence []evidence          `json:"evidence"`
	}{request, records})
	if err != nil {
		return nil, err
	}
	stream, err := e.Adapter.Stream(ctx, hx.GenerateOptions{Provider: e.Adapter.Route(), Model: e.Model, System: improve.Prompt(request), Messages: []hx.Message{{Role: hx.RoleUser, Content: []hx.ContentBlock{{Kind: hx.BlockText, Text: string(payload)}}}}, MaxTokens: 4096})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, improve.ErrBudget) {
			return nil, err
		}
		return nil, errors.Join(improve.ErrProvider, err)
	}
	var text strings.Builder
	finished := false
	providerFailed := false
	for chunk := range stream {
		if chunk.Kind == hx.ChunkTextDelta {
			if text.Len()+len(chunk.Delta) > 65536 {
				return nil, errors.New("harnesseval: editor output exceeds limit")
			}
			text.WriteString(chunk.Delta)
		}
		if chunk.Kind == hx.ChunkFinish {
			finished = chunk.Finish == hx.FinishStop
			providerFailed = chunk.Finish == hx.FinishError
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if !finished {
		if providerFailed {
			return nil, improve.ErrProvider
		}
		return nil, errors.New("harnesseval: editor did not finish")
	}
	return []byte(text.String()), nil
}
