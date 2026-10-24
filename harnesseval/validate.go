package harnesseval

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/websines/structor-harness/harness/improve"
	"github.com/websines/structor-harness/record"
)

type Validation struct {
	Schema     int           `json:"schema"`
	Dataset    record.Digest `json:"dataset"`
	Fixtures   int           `json:"fixtures"`
	Checks     int           `json:"checks"`
	Checker    string        `json:"checker"`
	Executable record.Digest `json:"executable"`
}

// ValidateCatalog creates the prerequisite receipt only after every original,
// gold and negative mutation has executed with the expected behavior.
func ValidateCatalog(ctx context.Context, state string, progress func(string)) (Validation, error) {
	executable, err := executableProvenance()
	if err != nil {
		return Validation{}, err
	}
	if err := PrepareToolchain(ctx); err != nil {
		return Validation{}, err
	}
	v := Validation{Schema: 1, Dataset: record.ComputeDigest(catalog), Checker: "blackbox-v1", Executable: executable.Digest}
	if !filepath.IsAbs(state) {
		return v, errors.New("harnesseval: absolute validation root required")
	}
	if err := os.MkdirAll(state, 0700); err != nil {
		return v, err
	}
	unlock, err := lockExperiment(state)
	if err != nil {
		return v, err
	}
	defer unlock()
	cas, err := record.NewLocalCAS(filepath.Join(state, "cas"))
	if err != nil {
		return v, err
	}
	journal, err := improve.NewJournal(filepath.Join(state, "journal"), cas)
	if err != nil {
		return v, err
	}
	fixtures, err := Catalog()
	if err != nil {
		return v, err
	}
	for _, f := range fixtures {
		if err = f.Validate(ctx, cas); err != nil {
			return v, err
		}
		v.Fixtures++
		v.Checks += 2 + len(f.Mutations)
		if progress != nil {
			progress(f.Task.ID)
		}
	}
	_, err = journal.Record(ctx, "validation", v)
	return v, err
}
