package query

import "github.com/websines/structor-harness/flg"

func cloneSupersessions(sups []flg.SupersessionEvidence) []flg.SupersessionEvidence {
	snapshot := append([]flg.SupersessionEvidence(nil), sups...)
	for i := range snapshot {
		snapshot[i].ReplacedBy = append([]string(nil), snapshot[i].ReplacedBy...)
	}
	return snapshot
}

// Supersessions returns the same pinned state used to build the graph.
func (ix *InMemoryIndex) Supersessions() []flg.SupersessionEvidence {
	return cloneSupersessions(ix.supersessions)
}
