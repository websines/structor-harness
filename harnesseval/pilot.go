package harnesseval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/harness/improve"
	"github.com/websines/structor-harness/harness/llm"
	"github.com/websines/structor-harness/record"
)

// RunPilot keeps paid dispatch, prospective lessons and private evaluation in
// separate phases. A nil adapter selects the configured live provider.
func RunPilot(parent context.Context, c Config, state string, resume bool, adapter hx.LlmAdapter) (Report, error) {
	report := Report{}
	if err := c.Validate(); err != nil {
		return report, err
	}
	if !filepath.IsAbs(state) {
		return report, errors.New("harnesseval: absolute private experiment root required")
	}
	if err := os.MkdirAll(state, 0700); err != nil {
		return report, err
	}
	info, err := os.Lstat(state)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return report, errors.New("harnesseval: experiment directory must have mode 0700")
	}
	unlock, err := lockExperiment(state)
	if err != nil {
		return report, err
	}
	defer unlock()
	cas, err := record.NewLocalCAS(filepath.Join(state, "cas"))
	if err != nil {
		return report, err
	}
	journal, err := improve.NewJournal(filepath.Join(state, "journal"), cas)
	if err != nil {
		return report, err
	}
	var validation Validation
	executable, err := executableProvenance()
	if err != nil {
		return report, err
	}
	if _, err = journal.Read("validation", &validation); err != nil || validation.Schema != 1 || validation.Dataset != record.ComputeDigest(catalog) || validation.Fixtures != 24 || validation.Checks != 96 || validation.Checker != "blackbox-v1" || validation.Executable != executable.Digest {
		return report, errors.New("harnesseval: matching complete fixture validation for this executable is required before model calls")
	}
	baseDigest, err := improve.Digest(struct {
		Instructions, Project, Model, Endpoint string
		Steps, Output, Seconds                 int
		Executable                             record.Digest
	}{PilotInstructions, c.Project, c.Model, c.BaseURL, c.MaxSteps, c.MaxOutput, c.ExecutionSeconds, executable.Digest})
	if err != nil {
		return report, err
	}
	report.Manifest = Manifest{Schema: 1, Config: c, Dataset: record.ComputeDigest(catalog), Base: improve.Profile{Schema: 1, Base: baseDigest}, Upstream: improve.Upstream, Executable: executable}
	var previous Manifest
	if _, readErr := journal.Read("manifest", &previous); readErr == nil {
		if !resume {
			return report, errors.New("harnesseval: experiment exists; use resume")
		}
		want, _ := improve.Digest(report.Manifest)
		got, _ := improve.Digest(previous)
		if want != got {
			return report, errors.New("harnesseval: experiment dataset/configuration changed")
		}
		var done Report
		if _, err := journal.Read("report", &done); err == nil {
			return done, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return report, err
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return report, readErr
	} else {
		if resume {
			return report, errors.New("harnesseval: no experiment to resume")
		}
		if _, err = journal.Record(parent, "manifest", report.Manifest); err != nil {
			return report, err
		}
	}
	if adapter == nil {
		key := os.Getenv(c.APIKeyEnv)
		if key == "" {
			return report, errors.New("harnesseval: configured provider key is unavailable")
		}
		adapter = &llm.OpenAIAdapter{BaseURL: c.BaseURL, APIKey: key, DefaultModel: c.Model, HTTP: &http.Client{Timeout: 90 * time.Second}, NativeCaching: true, CacheScope: llm.CacheScopeKey(state, "pilot:operator")}
	}
	global, err := improve.NewBudget(c.Total, c.Rates)
	if err != nil {
		return report, err
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(c.TimeoutSeconds)*time.Second)
	defer cancel()
	activeStarted := time.Now()
	var restoredElapsed time.Duration
	if err := PrepareToolchain(ctx); err != nil {
		return report, err
	}
	fixtures, err := Catalog()
	if err != nil {
		return report, err
	}
	var selected []Fixture
	for _, f := range fixtures {
		if f.Split == c.Split {
			selected = append(selected, f)
		}
	}
	lessons := map[string][]improve.Lesson{}
	if c.SeedExperiment != "" {
		seed, err := loadSeed(c.SeedExperiment, report.Manifest)
		if err != nil {
			return report, err
		}
		for repeat := 0; repeat < c.Repeats; repeat++ {
			for _, lesson := range seed {
				key := fmt.Sprintf("%d:%s", repeat, lesson.Scope)
				lessons[key] = append(lessons[key], lesson)
			}
		}
	}
	stopped := false
	for repeat := 0; repeat < c.Repeats && !stopped; repeat++ {
		for index, f := range selected {
			if stopped {
				break
			}
			f.Task.Source, err = f.Source.Store(cas)
			if err != nil {
				return report, err
			}
			arms := []string{"a", "b", "c"}
			if c.Split == "development" {
				arms = []string{"c"}
			} else {
				offset := index % 3
				arms = append(append([]string{}, arms[offset:]...), arms[:offset]...)
			}
			for _, arm := range arms {
				id := fmt.Sprintf("t%02dr%d%s", index, repeat, arm)
				trial := Trial{ID: id, TaskID: f.Task.ID, Family: f.Task.Family, Repeat: repeat, Arm: arm}
				key := fmt.Sprintf("%d:%s", repeat, f.Task.Family)
				var restored Trial
				if _, readErr := journal.Read(id+"-trial", &restored); readErr == nil {
					if err := global.Restore(restored.Spending); err != nil {
						return report, err
					}
					restoredElapsed += time.Duration(restored.ElapsedMS) * time.Millisecond
					report.Trials = append(report.Trials, restored)
					var lesson improve.Lesson
					if _, err := journal.Read(id+"-lesson", &lesson); err == nil {
						lessons[key] = append(lessons[key], lesson)
					}
					continue
				} else if !errors.Is(readErr, os.ErrNotExist) {
					return report, readErr
				}
				var pending json.RawMessage
				if _, readErr := journal.Read(id+"-trial-intent", &pending); readErr == nil {
					trial.Error = "uncertain paid attempt; reserved maximum retained"
					trial.Infrastructure = true
					trial.Spending = improve.Spending{Input: c.Trial.Input, Output: c.Trial.Output, USD: c.Trial.USD, Calls: c.Trial.Calls, Unknown: 1}
					report.Trials = append(report.Trials, trial)
					report.StopReason = "uncertain original trial " + id + "; no paid execution replayed"
					stopped = true
					break
				} else if !errors.Is(readErr, os.ErrNotExist) {
					return report, readErr
				}
				remaining := time.Duration(c.TimeoutSeconds)*time.Second - restoredElapsed - time.Since(activeStarted)
				if ctx.Err() != nil || global.Exhausted() || remaining <= 0 {
					report.StopReason = "global budget/deadline exhausted"
					stopped = true
					break
				}
				if _, err = journal.Record(ctx, id+"-trial-intent", trial); err != nil {
					return report, err
				}
				prior := lessons[key]
				if len(prior) > 3 {
					prior = prior[len(prior)-3:]
				}
				admissionCtx, finishAdmission := context.WithTimeout(ctx, remaining)
				trial, lesson, trialErr := executeTrial(admissionCtx, c, state, cas, journal, adapter, global, f.Task, report.Manifest.Base, trial, prior)
				finishAdmission()
				if lesson != nil {
					lessons[key] = append(lessons[key], *lesson)
				}
				if trialErr != nil {
					trial.Error = trialErr.Error()
				}
				if trial.Infrastructure {
					report.StopReason = "execution infrastructure failure"
					stopped = true
				}
				if _, err = journal.Record(context.WithoutCancel(ctx), id+"-trial", trial); err != nil {
					return report, err
				}
				report.Trials = append(report.Trials, trial)
				if stopped {
					break
				}
				if global.Exhausted() || ctx.Err() != nil {
					report.StopReason = "global budget/deadline exhausted"
					stopped = true
					break
				}
			}
		}
	}
	// Every selection is sealed before any private grade is observed.
	if _, err = journal.Record(context.WithoutCancel(ctx), "selections", report.Trials); err != nil {
		return report, err
	}
	fixtureByID := map[string]Fixture{}
	for _, f := range selected {
		fixtureByID[f.Task.ID] = f
	}
	for i := range report.Trials {
		trial := &report.Trials[i]
		remaining := time.Duration(c.TimeoutSeconds)*time.Second - restoredElapsed - time.Since(activeStarted)
		if remaining <= 0 {
			report.StopReason = "active runtime budget exhausted before grading completed"
			stopped = true
			break
		}
		evalCtx, stop := context.WithTimeout(parent, min(90*time.Second, remaining))
		e, gradeErr := gradeTrial(evalCtx, fixtureByID[trial.TaskID], cas, *trial)
		stop()
		trial.Evaluation = &e
		if gradeErr != nil {
			trial.Evaluation.Reason = gradeErr.Error()
		}
	}
	expected := len(selected) * c.Repeats
	if c.Split == "evaluation" {
		expected *= 3
	}
	report.Complete = !stopped && len(report.Trials) == expected
	report.Summarize()
	if _, err = journal.Record(context.WithoutCancel(ctx), "report", report); err != nil {
		return report, err
	}
	return report, nil
}

func executeTrial(ctx context.Context, c Config, state string, cas record.CAS, journal *improve.Journal, adapter hx.LlmAdapter, global *improve.Budget, task improve.Task, base improve.Profile, trial Trial, lessons []improve.Lesson) (Trial, *improve.Lesson, error) {
	trialBudget, err := improve.NewBudget(c.Trial, c.Rates)
	if err != nil {
		return trial, nil, err
	}
	guard := &improve.Guard{Adapter: adapter, Budgets: []*improve.Budget{global, trialBudget}, MaxOutput: c.MaxOutput}
	runner := &Runner{CAS: cas, Adapter: guard, Model: c.Model, StateDir: state, Principal: "pilot:operator", Timeout: time.Duration(c.ExecutionSeconds) * time.Second, MaxSteps: c.MaxSteps, BaseInstructions: PilotInstructions}
	started := time.Now()
	trialCtx, stopTrial := context.WithTimeout(ctx, time.Duration(c.TrialSeconds)*time.Second)
	defer stopTrial()
	var lesson *improve.Lesson
	if trial.Arm == "c" {
		loop := improve.Loop{Runner: runner, Editor: &ModelEditor{Adapter: guard, Model: c.Model, CAS: cas, Timeout: 90 * time.Second}, Recorder: journal}
		out, runErr := loop.Run(trialCtx, task, base, trial.ID, lessons)
		trial.Runs, trial.Final, trial.Fallback = out.Runs, out.Final, out.Fallback
		trial.FallbackReason = out.Reason
		lesson = out.Lesson
		err = runErr
	} else {
		attempts := 1
		if trial.Arm == "b" {
			attempts = 5
		}
		for attempt := 0; attempt < attempts; attempt++ {
			runID := fmt.Sprintf("%s-at%d", trial.ID, attempt)
			if _, err = journal.Record(trialCtx, runID+"-intent", task); err != nil {
				break
			}
			run, runErr := runner.Execute(trialCtx, task, base, runID)
			trial.Runs = append(trial.Runs, run)
			if _, err = journal.Record(context.WithoutCancel(ctx), runID+"-result", run); err != nil {
				break
			}
			if runErr != nil {
				err = runErr
				break
			}
			if run.Fault == "provider" {
				trial.Infrastructure = true
				break
			}
			if attempt == 0 || betterRun(run, trial.Final) {
				trial.Final = run
			}
		}
	}
	stopTrial()
	guard.Wait()
	for _, run := range trial.Runs {
		if run.Fault == "provider" {
			trial.Infrastructure = true
		}
	}
	if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) && !errors.Is(err, improve.ErrBudget) {
		trial.Infrastructure = true
	}
	trial.ElapsedMS = time.Since(started).Milliseconds()
	trial.Spending = trialBudget.Snapshot()
	return trial, lesson, err
}

func betterRun(candidate, current improve.Run) bool {
	if (candidate.Error == "") != (current.Error == "") {
		return candidate.Error == ""
	}
	return candidate.PublicPassed > current.PublicPassed || candidate.PublicPassed == current.PublicPassed && candidate.BuildOK && !current.BuildOK
}

func gradeTrial(ctx context.Context, f Fixture, cas record.CAS, trial Trial) (Evaluation, error) {
	if trial.Infrastructure {
		return Evaluation{Artifact: trial.Final.Artifact, Reason: "execution infrastructure unavailable"}, nil
	}
	if trial.Error != "" || trial.Final.Error != "" || !improve.ValidDigest(trial.Final.Artifact) {
		return Evaluation{Artifact: trial.Final.Artifact, Scored: true, Passed: false, Integrity: !trial.Final.IntegrityViolation, Reason: "execution failed, exhausted budget or did not deliver an artifact"}, nil
	}
	return f.Evaluate(ctx, cas, trial.Final.Artifact)
}
