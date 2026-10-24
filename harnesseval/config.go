package harnesseval

import (
	"errors"
	"net/url"

	"github.com/websines/structor-harness/harness/improve"
	"github.com/websines/structor-harness/harness/native"
)

const PilotInstructions = native.Instructions

type Config struct {
	Schema           int            `json:"schema"`
	Project          string         `json:"project"`
	Model            string         `json:"model"`
	BaseURL          string         `json:"baseURL"`
	APIKeyEnv        string         `json:"apiKeyEnv"`
	Rates            improve.Rates  `json:"rates"`
	Split            string         `json:"split"`
	Repeats          int            `json:"repeats"`
	Trial            improve.Limits `json:"trial"`
	Total            improve.Limits `json:"total"`
	TimeoutSeconds   int            `json:"timeoutSeconds"`
	TrialSeconds     int            `json:"trialSeconds"`
	ExecutionSeconds int            `json:"executionSeconds"`
	MaxSteps         int            `json:"maxSteps"`
	MaxOutput        int            `json:"maxOutput"`
	SeedExperiment   string         `json:"seedExperiment,omitempty"`
	PricingSource    string         `json:"pricingSource,omitempty"`
}

func (c Config) Validate() error {
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Hostname() == "" || (u.Scheme != "https" && (u.Scheme != "http" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1"))) {
		return errors.New("harnesseval: HTTPS or loopback provider required")
	}
	if c.Schema != 1 || c.Project == "" || c.Model == "" || c.APIKeyEnv == "" || (c.Split != "development" && c.Split != "evaluation") || c.Repeats < 1 || c.Repeats > 2 || c.TimeoutSeconds < 1 || c.TimeoutSeconds > 86400 || c.TrialSeconds < 1 || c.TrialSeconds > 1800 || c.ExecutionSeconds < 1 || c.ExecutionSeconds > 300 || c.MaxSteps < 1 || c.MaxSteps > 24 || c.MaxOutput < 1 || c.MaxOutput > 8192 {
		return errors.New("harnesseval: invalid pilot configuration")
	}
	if c.Rates.Input <= 0 || c.Rates.Output <= 0 {
		return errors.New("harnesseval: explicit positive operator rates required")
	}
	if _, err := improve.NewBudget(c.Trial, c.Rates); err != nil {
		return err
	}
	_, err = improve.NewBudget(c.Total, c.Rates)
	return err
}
