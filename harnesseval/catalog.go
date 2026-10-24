package harnesseval

import (
	_ "embed"
	"errors"

	"github.com/websines/structor-harness/harness/improve"
)

//go:embed fixtures/catalog.json
var catalog []byte

// Catalog returns an owned copy. Private fixture bytes are never included in
// the Task passed to an executor or editor.
func Catalog() ([]Fixture, error) {
	var fixtures []Fixture
	if err := improve.Decode(catalog, &fixtures); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, f := range fixtures {
		if seen[f.Task.ID] {
			return nil, errors.New("harnesseval: duplicate task")
		}
		seen[f.Task.ID] = true
	}
	return fixtures, nil
}
