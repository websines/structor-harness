// White-box tests (package skillstore): drive skills through the REAL append-only
// record — record.NewLocalCAS + flg.NewStore + learning.NewStore — and inspect the
// registry buildRegistry produces, so every assertion is about activation gated by
// facts that survived a round-trip through the canonical record (Law 1).
package skillstore

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/websines/structor-harness/flg"
	"github.com/websines/structor-harness/harness/skill"
	"github.com/websines/structor-harness/learning"
	"github.com/websines/structor-harness/record"
)

// newStore wires a learning store over a fresh local CAS and its flg record.
func newStore(t *testing.T) *learning.Store {
	t.Helper()
	cas, err := record.NewLocalCAS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return learning.NewStore(flg.NewStore(cas))
}

// catalogNames returns the (sorted) names in a registry's catalog.
func catalogNames(reg *skill.Registry) []string {
	cat := reg.Catalog()
	names := make([]string, len(cat))
	for i, s := range cat {
		names[i] = s.Name
	}
	return names
}

func hasName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// TestOnlyPromotedActivate: the built registry admits ONLY promoted skills — a
// candidate never appears, and a retired one drops out. This is the activation
// gate: presence in a directory is irrelevant; only an admission fact activates.
func TestOnlyPromotedActivate(t *testing.T) {
	ls := newStore(t)

	// Promoted → should activate.
	if _, err := ls.AddCandidate(learning.Skill{ID: SkillID("alpha"), Title: "alpha skill", Body: "do alpha"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ls.Promote(SkillID("alpha"), learning.AcceptHuman("principal:lead", "reviewed", true)); err != nil {
		t.Fatal(err)
	}
	// Candidate → must NOT activate (no promotion fact).
	if _, err := ls.AddCandidate(learning.Skill{ID: SkillID("beta"), Title: "beta skill", Body: "do beta"}); err != nil {
		t.Fatal(err)
	}
	// Promoted then retired → must drop out (supersession beats promotion).
	if _, err := ls.AddCandidate(learning.Skill{ID: SkillID("gamma"), Title: "gamma skill", Body: "do gamma"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ls.Promote(SkillID("gamma"), learning.AcceptHuman("principal:lead", "reviewed", true)); err != nil {
		t.Fatal(err)
	}
	if _, err := ls.Retire(SkillID("gamma"), "superseded"); err != nil {
		t.Fatal(err)
	}

	names := catalogNames(buildRegistry(ls, ""))
	if len(names) != 1 || names[0] != "alpha" {
		t.Fatalf("only the promoted skill must activate, got catalog %v", names)
	}
	if hasName(names, "beta") {
		t.Fatal("a candidate must not activate")
	}
	if hasName(names, "gamma") {
		t.Fatal("a retired skill must drop out of the catalog")
	}
}

// TestScopeFiltersActivation: activation is filtered to the agent's scope — a
// promoted skill outside the scope query is not activated for that agent, even
// though it is admitted globally.
func TestScopeFiltersActivation(t *testing.T) {
	ls := newStore(t)
	add := func(name, title, body string) {
		if _, err := ls.AddCandidate(learning.Skill{ID: SkillID(name), Title: title, Body: body}); err != nil {
			t.Fatal(err)
		}
		if _, err := ls.Promote(SkillID(name), learning.AcceptHuman("principal:lead", "reviewed", true)); err != nil {
			t.Fatal(err)
		}
	}
	add("research-notes", "distill research findings", "how to take research notes")
	add("deploy-canary", "roll out a canary deploy", "how to deploy a canary")

	names := catalogNames(buildRegistry(ls, "research"))
	if len(names) != 1 || names[0] != "research-notes" {
		t.Fatalf("scope should admit only in-scope promoted skills, got %v", names)
	}
}

// TestResidentBodyRoundTrip: a promoted skill's body survives the whole path —
// AddCandidate (body → CAS) → Recall (projected back) → AddResident → Load — so
// the `skill` tool returns the admitted body with no file on disk.
func TestResidentBodyRoundTrip(t *testing.T) {
	ls := newStore(t)
	const body = "step 1: read.\nstep 2: write.\nstep 3: verify."
	if _, err := ls.AddCandidate(learning.Skill{ID: SkillID("procedure"), Title: "a procedure", Body: body}); err != nil {
		t.Fatal(err)
	}
	if _, err := ls.Promote(SkillID("procedure"), learning.AcceptHuman("principal:lead", "reviewed", true)); err != nil {
		t.Fatal(err)
	}

	reg := buildRegistry(ls, "")
	loaded, ok := reg.Load("procedure")
	if !ok {
		t.Fatal("admitted skill must be loadable from the registry")
	}
	if loaded.Source != "" {
		t.Fatalf("a record-backed skill has no file source, got %q", loaded.Source)
	}
	if loaded.Body != body {
		t.Fatalf("resident body did not round-trip: got %q", loaded.Body)
	}
}

// TestImportTrustedIsAdmitted: Import(trust=true) records the candidate AND a
// promotion fact, so the imported skill then activates in the built registry.
func TestImportTrustedIsAdmitted(t *testing.T) {
	ls := newStore(t)
	dir := writeSkillFile(t, "clean-skill", "a cleanly imported skill", "imported body")

	imported, admitted, err := Import(ls, []skill.Root{{Dir: dir, Rank: skill.RankProject}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if imported != 1 || admitted != 1 {
		t.Fatalf("trusted import should record 1 candidate and admit 1, got imported=%d admitted=%d", imported, admitted)
	}

	names := catalogNames(buildRegistry(ls, ""))
	if !hasName(names, "clean-skill") {
		t.Fatalf("a trusted import must activate, got catalog %v", names)
	}
	// Body should have round-tripped file → CAS → resident.
	loaded, ok := buildRegistry(ls, "").Load("clean-skill")
	if !ok || loaded.Body != "imported body" {
		t.Fatalf("imported body did not round-trip through the record: ok=%v body=%q", ok, loaded.Body)
	}
}

// TestImportUntrustedIsCandidateOnly: Import(trust=false) records a candidate but
// does NOT admit it — it is recallable as a candidate yet absent from the built
// (promoted-only) catalog.
func TestImportUntrustedIsCandidateOnly(t *testing.T) {
	ls := newStore(t)
	dir := writeSkillFile(t, "untrusted-skill", "an untrusted import", "untrusted body")

	imported, admitted, err := Import(ls, []skill.Root{{Dir: dir, Rank: skill.RankProject}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if imported != 1 || admitted != 0 {
		t.Fatalf("untrusted import should record 1 candidate and admit 0, got imported=%d admitted=%d", imported, admitted)
	}

	// Recalled as a candidate (it is in the record) ...
	got := ls.Recall("untrusted")
	if len(got) != 1 || got[0].State != learning.SkillCandidate {
		t.Fatalf("untrusted import should be a recallable candidate, got %+v", got)
	}
	// ... but NOT activated.
	if hasName(catalogNames(buildRegistry(ls, "")), "untrusted-skill") {
		t.Fatal("an untrusted import must not activate")
	}
}

// TestIDHelpers pins the shared id↔name↔version convention.
func TestIDHelpers(t *testing.T) {
	if got := SkillName(SkillID("foo")); got != "foo" {
		t.Fatalf(`SkillName(SkillID("foo")) = %q, want "foo"`, got)
	}
	if got := SkillName(VersionID("foo", "x")); got != "foo" {
		t.Fatalf(`SkillName(VersionID("foo","x")) = %q, want "foo"`, got)
	}
	// A versioned id is content-addressed: distinct bodies → distinct ids,
	// the same body → the same id.
	if VersionID("foo", "a") == VersionID("foo", "b") {
		t.Fatal("distinct bodies must produce distinct version ids")
	}
	if VersionID("foo", "a") != VersionID("foo", "a") {
		t.Fatal("the same body must produce the same version id")
	}
	if SkillID("foo") == VersionID("foo", "a") {
		t.Fatal("a versioned id must differ from the base id")
	}
}

// writeSkillFile writes a flat <dir>/<name>.md skill with the given description
// and body, and returns dir (a fresh temp root discovery can scan).
func writeSkillFile(t *testing.T, name, description, body string) string {
	t.Helper()
	dir := t.TempDir()
	content := "---\nname: " + name + "\ndescription: " + description + "\n---\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}
