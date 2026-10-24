package query_test

import (
	"github.com/websines/structor-harness/flg"
	"github.com/websines/structor-harness/query"
	"testing"
)

func TestAuditResearchParentPreservesObligationMetadata(t *testing.T) {
	parent := flg.Obligation{ID: "Q-1", LemmaID: "research.question", Title: "What authentication is required?", Anchor: flg.Anchor{File: "auth.go"}}
	child := flg.Obligation{ID: "F-1", LemmaID: "Q-1", Title: "Every request must authenticate"}
	for _, obligations := range [][]flg.Obligation{{parent, child}, {child, parent}} {
		index := query.FromProjection(obligations, nil, nil, "", nil)
		node, ok := index.Node(query.NodeID(parent.ID))
		if !ok || node.Kind != query.KindObligation || node.Text != parent.Title || node.Path != parent.Anchor.File {
			t.Fatalf("finding overwrote parent: %+v", node)
		}
	}
}

func TestAuditDeclaredRetrievalExcludesSupersededEvidence(t *testing.T) {
	o := flg.Obligation{ID: "O", Title: "authorization"}
	evs := []flg.Evidence{
		{ID: "old", ObligationID: "O", Summary: "legacytoken unique-old-support"},
		{ID: "new", ObligationID: "O", Summary: "current support"},
	}
	sups := []flg.SupersessionEvidence{{ID: "sup", Subject: "old", Kind: flg.KindSupersededBy, ReplacedBy: []string{"new"}}}
	ix := query.FromProjection([]flg.Obligation{o}, evs, sups, "", nil)
	direct := query.Retrieve(ix, sups, "legacytoken", 10)
	for _, item := range direct.Governing {
		if item.ID == "old" {
			t.Fatal("setup direct retrieval retained superseded item")
		}
	}
	pinned := query.Run(ix, query.QuerySet{Name: "proof", Version: "v1", Steps: []query.Step{{Verb: query.VerbRetrieve, Query: "legacytoken", Limit: 10}}})
	for _, edge := range pinned.Gate().ProofEdges {
		if edge.To == "old" {
			t.Fatalf("declared retrieval restored superseded fact as governing proof: %+v", edge)
		}
	}
}
