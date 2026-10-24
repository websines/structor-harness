package shell

import (
	"strings"
	"testing"
)

func TestWorkspaceEnvironmentFromOmitsHostSecrets(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "host-secret-canary")
	env, err := WorkspaceEnvironmentFrom(t.TempDir(), []string{"PATH=/usr/bin:/bin", "HOME=/isolated"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "host-secret-canary") || strings.Contains(joined, "OPENAI_API_KEY") {
		t.Fatal("experimental shell inherited provider credentials")
	}
	if !strings.Contains(joined, "GOCACHE=") || !strings.Contains(joined, "HOME=/isolated") {
		t.Fatalf("missing bounded environment: %s", joined)
	}
}
