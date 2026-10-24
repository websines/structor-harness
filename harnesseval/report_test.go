package harnesseval

import (
	"fmt"
	"testing"

	"github.com/websines/structor-harness/harness/improve"
	"github.com/websines/structor-harness/harness/llm"
)

func TestReportExcludesUnscoredComparisons(t *testing.T) {
	r := Report{Complete: true, Trials: []Trial{{TaskID: "one", Arm: "a", Evaluation: &Evaluation{Scored: true, Passed: false, Integrity: true}}, {TaskID: "one", Arm: "b", Evaluation: &Evaluation{Scored: false}}, {TaskID: "one", Arm: "c", Evaluation: &Evaluation{Scored: true, Passed: true, Integrity: true}}}}
	r.Summarize()
	if r.Complete || r.ExpansionEligible || r.NetWinsOverB != 0 || r.AgainstB.Pairs != 0 {
		t.Fatalf("missing outcome became win: %+v", r)
	}
}

func TestGradeTrialCountsSolverFailuresButNotInfrastructure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		trial  Trial
		scored bool
	}{{"timeout", Trial{Final: improve.Run{Error: "deadline exceeded", Fault: "timeout"}}, true}, {"missing artifact", Trial{}, true}, {"infrastructure", Trial{Infrastructure: true}, false}} {
		t.Run(tc.name, func(t *testing.T) {
			e, err := gradeTrial(t.Context(), Fixture{}, nil, tc.trial)
			if err != nil || e.Scored != tc.scored || e.Passed {
				t.Fatalf("bad failure classification: %+v %v", e, err)
			}
		})
	}
}
func TestReportClustersRepeatsAndBlocksIntegrityViolations(t *testing.T) {
	r := Report{Complete: true, Manifest: Manifest{Config: Config{Split: "evaluation"}}}
	for task := 0; task < 16; task++ {
		for repeat := 0; repeat < 2; repeat++ {
			for _, arm := range []string{"a", "b", "c"} {
				passed := task < 8 || arm == "c"
				r.Trials = append(r.Trials, Trial{ID: fmt.Sprintf("%d%d%s", task, repeat, arm), TaskID: fmt.Sprint(task), Family: fmt.Sprint(task % 4), Repeat: repeat, Arm: arm, Spending: improve.Spending{USD: 1, CostEstimate: &llm.CostEstimate{TotalUSD: 1}}, Evaluation: &Evaluation{Scored: true, Passed: passed, Integrity: true}})
			}
		}
	}
	r.Summarize()
	if !r.ExpansionEligible || r.AgainstB.Tasks != 16 || r.AgainstB.Pairs != 32 || r.AgainstB.Delta != 0.5 || r.AgainstB.Lower < 0 || r.AgainstB.Upper > 1 {
		t.Fatalf("bad paired report: %+v", r)
	}
	// A safe final fallback cannot erase a rejected probe's protected-input edit.
	r.Trials[len(r.Trials)-1].Runs = []improve.Run{{IntegrityViolation: true}}
	r.Summarize()
	if r.ExpansionEligible || r.IntegrityViolations != 1 {
		t.Fatal("fallback concealed a probe integrity violation")
	}
	r.Trials[len(r.Trials)-1].Runs = nil
	r.Trials[len(r.Trials)-1].Evaluation.Integrity = false
	r.Summarize()
	if r.ExpansionEligible || r.IntegrityViolations != 1 {
		t.Fatal("integrity violation admitted")
	}
}
