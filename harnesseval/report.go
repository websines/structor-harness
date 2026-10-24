package harnesseval

import (
	"fmt"
	"math/rand/v2"
	"sort"

	"github.com/websines/structor-harness/harness/improve"
	"github.com/websines/structor-harness/record"
)

type Trial struct {
	ID             string           `json:"id"`
	TaskID         string           `json:"taskId"`
	Family         string           `json:"family"`
	Repeat         int              `json:"repeat"`
	Arm            string           `json:"arm"`
	Runs           []improve.Run    `json:"runs"`
	Final          improve.Run      `json:"final"`
	Fallback       bool             `json:"fallback"`
	FallbackReason string           `json:"fallbackReason,omitempty"`
	Infrastructure bool             `json:"infrastructure"`
	Spending       improve.Spending `json:"spending"`
	ElapsedMS      int64            `json:"elapsedMS"`
	Error          string           `json:"error,omitempty"`
	Evaluation     *Evaluation      `json:"evaluation,omitempty"`
}
type Manifest struct {
	Schema     int             `json:"schema"`
	Config     Config          `json:"config"`
	Dataset    record.Digest   `json:"dataset"`
	Base       improve.Profile `json:"base"`
	Upstream   string          `json:"upstream"`
	Executable Executable      `json:"executable"`
}
type Report struct {
	Manifest            Manifest              `json:"manifest"`
	Trials              []Trial               `json:"trials"`
	Complete            bool                  `json:"complete"`
	StopReason          string                `json:"stopReason,omitempty"`
	Arms                map[string]ArmSummary `json:"arms"`
	NetWinsOverA        int                   `json:"netWinsOverA"`
	NetWinsOverB        int                   `json:"netWinsOverB"`
	LossesToA           int                   `json:"lossesToA"`
	AgainstA            Difference            `json:"againstA"`
	AgainstB            Difference            `json:"againstB"`
	IntegrityViolations int                   `json:"integrityViolations"`
	ExpansionEligible   bool                  `json:"expansionEligible"`
}
type Difference struct {
	Tasks int     `json:"tasks"`
	Pairs int     `json:"pairs"`
	Delta float64 `json:"delta"`
	Lower float64 `json:"lower95"`
	Upper float64 `json:"upper95"`
}

type ArmSummary struct {
	Trials       int      `json:"trials"`
	Scored       int      `json:"scored"`
	Passed       int      `json:"passed"`
	USD          float64  `json:"usd"`
	Unknown      int      `json:"unknown"`
	Input        int64    `json:"input"`
	Output       int64    `json:"output"`
	ElapsedMS    int64    `json:"elapsedMS"`
	KnownCostUSD *float64 `json:"knownCostUSD,omitempty"`
}

func (r *Report) Summarize() {
	r.Arms = map[string]ArmSummary{}
	type outcome struct {
		present, scored, passed bool
		task                    string
	}
	pairs := map[string]map[string]outcome{}
	families := map[string]map[string]int{}
	r.NetWinsOverA, r.NetWinsOverB, r.LossesToA, r.IntegrityViolations = 0, 0, 0, 0
	for _, trial := range r.Trials {
		integrityViolation := trial.Final.IntegrityViolation
		for _, run := range trial.Runs {
			integrityViolation = integrityViolation || run.IntegrityViolation
		}
		s := r.Arms[trial.Arm]
		s.Trials++
		s.USD += trial.Spending.USD
		s.Unknown += trial.Spending.Unknown
		s.Input += trial.Spending.Input
		s.Output += trial.Spending.Output
		s.ElapsedMS += trial.ElapsedMS
		o := outcome{present: true, task: trial.TaskID}
		if trial.Evaluation != nil && trial.Evaluation.Scored {
			s.Scored++
			o.scored = true
			o.passed = trial.Evaluation.Passed
			if o.passed {
				s.Passed++
			}
			integrityViolation = integrityViolation || !trial.Evaluation.Integrity
		} else {
			r.Complete = false
		}
		if integrityViolation {
			r.IntegrityViolations++
		}
		r.Arms[trial.Arm] = s
		key := fmt.Sprintf("%s:%d", trial.TaskID, trial.Repeat)
		if pairs[key] == nil {
			pairs[key] = map[string]outcome{}
		}
		if pairs[key][trial.Arm].present {
			r.Complete = false
		}
		pairs[key][trial.Arm] = o
		if families[trial.Family] == nil {
			families[trial.Family] = map[string]int{}
		}
		if o.passed {
			families[trial.Family][trial.Arm]++
		}
	}
	// Budget USD is conservative; comparisons use the existing detailed cache
	// estimator only when every billed call supplies complete pricing details.
	for arm, s := range r.Arms {
		total := 0.0
		known := true
		for _, trial := range r.Trials {
			if trial.Arm != arm {
				continue
			}
			if trial.Spending.CostEstimate == nil {
				known = false
				break
			}
			total += trial.Spending.CostEstimate.TotalUSD
		}
		if known {
			s.KnownCostUSD = &total
		}
		r.Arms[arm] = s
	}
	clustersA, clustersB := map[string][]float64{}, map[string][]float64{}
	for _, p := range pairs {
		if !p["a"].scored || !p["b"].scored || !p["c"].scored {
			continue
		}
		a, b, c := 0, 0, 0
		if p["a"].passed {
			a = 1
		}
		if p["b"].passed {
			b = 1
		}
		if p["c"].passed {
			c = 1
		}
		r.NetWinsOverA += c - a
		r.NetWinsOverB += c - b
		if a == 1 && c == 0 {
			r.LossesToA++
		}
		task := p["c"].task
		clustersA[task] = append(clustersA[task], float64(c-a))
		clustersB[task] = append(clustersB[task], float64(c-b))
	}
	r.AgainstA = clusterDifference(clustersA)
	r.AgainstB = clusterDifference(clustersB)
	nonnegative := 0
	for _, f := range families {
		if f["c"] >= f["b"] {
			nonnegative++
		}
	}
	a, b, c := r.Arms["a"], r.Arms["b"], r.Arms["c"]
	r.ExpansionEligible = r.Complete && r.Manifest.Config.Split == "evaluation" && c.Trials == 32 && a.Trials == 32 && b.Trials == 32 && r.AgainstB.Pairs == 32 && r.AgainstB.Tasks == 16 && r.NetWinsOverB >= 4 && r.LossesToA <= 1 && nonnegative >= 3 && r.IntegrityViolations == 0 && b.Passed > 0 && c.Passed > 0 && a.Unknown == 0 && c.Unknown == 0 && b.Unknown == 0 && b.KnownCostUSD != nil && c.KnownCostUSD != nil && *c.KnownCostUSD/float64(c.Passed) <= 1.25*(*b.KnownCostUSD)/float64(b.Passed)
}

// Bootstrap whole tasks, keeping repeated observations in their task cluster.
// Intervals describe the complete paired subset; Complete exposes missing cases.
func clusterDifference(clusters map[string][]float64) Difference {
	d := Difference{Tasks: len(clusters)}
	if len(clusters) == 0 {
		return d
	}
	keys := make([]string, 0, len(clusters))
	for k := range clusters {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	means := make([]float64, 0, len(keys))
	for _, k := range keys {
		values := clusters[k]
		sum := 0.0
		for _, v := range values {
			sum += v
		}
		d.Pairs += len(values)
		d.Delta += sum
		means = append(means, sum/float64(len(values)))
	}
	d.Delta /= float64(d.Pairs)
	rng := rand.New(rand.NewPCG(19, 41))
	samples := make([]float64, 10000)
	for i := range samples {
		sum := 0.0
		for range means {
			sum += means[rng.IntN(len(means))]
		}
		samples[i] = sum / float64(len(means))
	}
	sort.Float64s(samples)
	d.Lower = samples[249]
	d.Upper = samples[9749]
	return d
}
