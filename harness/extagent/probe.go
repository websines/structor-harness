package extagent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/harness/subprocess"
)

// ProbeAuthentication calls the installed CLI's status command. It does not run
// a coding task and deliberately discards account identifiers from its output.
func ProbeAuthentication(ctx context.Context, name string) (bool, error) {
	sp, ok := lookup(name)
	if !ok {
		return false, errors.New("unsupported CLI")
	}
	var args []string
	switch sp.Name {
	case "claude-code":
		args = []string{"auth", "status", "--json"}
	case "codex":
		args = []string{"login", "status"}
	default:
		return false, errors.New("this CLI has no configured authentication probe")
	}
	result := subprocess.New(nil).Spawn(hx.SpawnRequest{Ctx: ctx, Argv: append([]string{sp.Bin}, args...), TimeoutMs: 15000, Confinement: hx.ConfineNone})
	if result.Err != "" || result.TimedOut || ctx.Err() != nil {
		return false, errors.New("CLI authentication probe could not complete")
	}
	if result.ExitCode != 0 {
		return false, nil
	}
	if sp.Name == "claude-code" {
		var status struct {
			LoggedIn bool `json:"loggedIn"`
		}
		if err := json.Unmarshal(result.Stdout, &status); err != nil {
			return false, errors.New("unrecognized CLI authentication response")
		}
		return status.LoggedIn, nil
	}
	return strings.Contains(string(result.Stdout)+string(result.Stderr), "Logged in"), nil
}
