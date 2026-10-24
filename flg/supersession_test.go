package flg

import "testing"

func sup(subject string, replacedBy ...string) SupersessionEvidence {
	return SupersessionEvidence{
		Subject:    subject,
		Kind:       KindSupersededBy,
		ReplacedBy: replacedBy,
		Evidence:   []string{"cite:" + subject},
	}
}

// A linear chain A -> B -> C resolves: A and B are superseded with active head C.
func TestSupersessionLinearChain(t *testing.T) {
	r := ResolveSupersession(
		[]string{"A", "B", "C"},
		[]SupersessionEvidence{sup("A", "B"), sup("B", "C")},
	)
	if got := r.StateOf("A"); got != NodeSuperseded {
		t.Fatalf("A: got %q want superseded", got)
	}
	if got := r.StateOf("B"); got != NodeSuperseded {
		t.Fatalf("B: got %q want superseded", got)
	}
	if got := r.StateOf("C"); got != NodeActive {
		t.Fatalf("C: got %q want active", got)
	}
	headsA := r.ByID()["A"].ReplacedBy
	if len(headsA) != 1 || headsA[0] != "C" {
		t.Fatalf("A heads: got %v want [C]", headsA)
	}
	if active := r.Active(); len(active) != 1 || active[0] != "C" {
		t.Fatalf("active set: got %v want [C]", active)
	}
}

// A self-cycle A -> A never resolves to a head — surfaced as ambiguous/repair.
func TestSupersessionSelfCycle(t *testing.T) {
	r := ResolveSupersession(
		[]string{"A"},
		[]SupersessionEvidence{sup("A", "A")},
	)
	if got := r.StateOf("A"); got != NodeAmbiguous {
		t.Fatalf("A: got %q want ambiguous", got)
	}
	if len(r.Ambiguities) == 0 {
		t.Fatalf("expected an ambiguity row for the self-cycle")
	}
	if a := r.Ambiguous(); len(a) != 1 || a[0] != "A" {
		t.Fatalf("ambiguous set: got %v want [A]", a)
	}
}

// A two-cycle A -> B -> A never resolves — both nodes are ambiguous.
func TestSupersessionTwoCycle(t *testing.T) {
	r := ResolveSupersession(
		[]string{"A", "B"},
		[]SupersessionEvidence{sup("A", "B"), sup("B", "A")},
	)
	if got := r.StateOf("A"); got != NodeAmbiguous {
		t.Fatalf("A: got %q want ambiguous", got)
	}
	if got := r.StateOf("B"); got != NodeAmbiguous {
		t.Fatalf("B: got %q want ambiguous", got)
	}
	if a := r.Ambiguous(); len(a) != 2 {
		t.Fatalf("ambiguous set: got %v want [A B]", a)
	}
	if len(r.Active()) != 0 {
		t.Fatalf("a cycle must leave nothing active, got %v", r.Active())
	}
}

// An exclude_marker keeps a fact for audit only, never governing.
func TestSupersessionAuditOnly(t *testing.T) {
	r := ResolveSupersession(
		[]string{"A"},
		[]SupersessionEvidence{{Subject: "A", Kind: KindExcludeMarker, Evidence: []string{"note"}}},
	)
	if got := r.StateOf("A"); got != NodeAuditOnly {
		t.Fatalf("A: got %q want audit_only", got)
	}
	if len(r.Active()) != 0 {
		t.Fatalf("audit_only must not govern, active=%v", r.Active())
	}
}
