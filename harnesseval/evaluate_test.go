package harnesseval

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/websines/structor-harness/record"
)

func TestConfinerProtectsPrivateFiles(t *testing.T) {
	dir := t.TempDir()
	c, err := NewConfiner(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err = c.Preflight(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogContract(t *testing.T) {
	fixtures, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	splits := map[string]int{}
	for _, f := range fixtures {
		counts[f.Task.Family]++
		splits[f.Split]++
		if err = f.Source.Validate(); err != nil {
			t.Fatal(f.Task.ID, err)
		}
	}
	if len(counts) != 4 || len(fixtures) != 24 || splits["development"] != 8 || splits["evaluation"] != 16 {
		t.Fatalf("counts: %v %v", counts, splits)
	}
}

// TestBehavioralFixtures runs real Go programs under confinement. All-fixture
// validation is explicit because it compiles isolated test binaries.
func TestBehavioralFixtures(t *testing.T) {
	fixtures, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("FORGE_VALIDATE_ALL_FIXTURES") == "" {
		fixtures = fixtures[:1]
	}
	cas, err := record.NewLocalCAS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fixtures {
		t.Run(f.Task.ID, func(t *testing.T) {
			if err := f.Validate(t.Context(), cas); err != nil {
				t.Fatal(err)
			}
		})
	}
}
