package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/websines/structor-harness/harness/hx"
)

// writeFlat writes a flat <name>.md skill file into dir and returns its path.
func writeFlat(t *testing.T, dir, name, content string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, name+".md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// writeBundle writes a directory-bundle <name>/SKILL.md skill and returns its path.
func writeBundle(t *testing.T, dir, name, content string) string {
	t.Helper()
	bundle := filepath.Join(dir, name)
	if err := os.MkdirAll(bundle, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", bundle, err)
	}
	path := filepath.Join(bundle, "SKILL.md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// discoverInto discovers roots and Adds every result into a fresh Registry.
func discoverInto(t *testing.T, roots []Root) *Registry {
	t.Helper()
	skills, err := DiscoverFilesystem(roots)
	if err != nil {
		t.Fatalf("DiscoverFilesystem: %v", err)
	}
	reg := NewRegistry()
	for _, s := range skills {
		reg.Add(s)
	}
	return reg
}

// Test 1: discovery across a project root (rank 100) and a user root (rank 400).
// The same-named `deploy` skill in both roots resolves to the project one (nearest
// rank), the catalog lists it once, and a directory-bundle skill is discovered too.
func TestDiscoveryRanking(t *testing.T) {
	projectRoot := t.TempDir()
	userRoot := t.TempDir()

	writeFlat(t, projectRoot, "deploy", "---\nname: deploy\ndescription: Project deploy.\n---\nProject body.")
	writeFlat(t, userRoot, "deploy", "---\nname: deploy\ndescription: User deploy.\n---\nUser body.")
	// A directory-bundle skill in the project root, to exercise the SKILL.md path.
	writeBundle(t, projectRoot, "build", "---\nname: build\ndescription: Build the app.\n---\nBuild body.")

	roots := []Root{{Dir: projectRoot, Rank: RankProject}, {Dir: userRoot, Rank: RankUser}}
	reg := discoverInto(t, roots)

	cat := reg.Catalog()
	if len(cat) != 2 {
		t.Fatalf("catalog size = %d, want 2 (build, deploy once); got %+v", len(cat), cat)
	}
	// Sorted by name: build, deploy.
	if cat[0].Name != "build" || cat[1].Name != "deploy" {
		t.Fatalf("catalog names = %q,%q, want build,deploy", cat[0].Name, cat[1].Name)
	}

	// The project deploy won on rank.
	if cat[1].Description != "Project deploy." {
		t.Errorf("deploy description = %q, want %q (project should win)", cat[1].Description, "Project deploy.")
	}
	loaded, ok := reg.Load("deploy")
	if !ok {
		t.Fatal("Load(deploy) not found")
	}
	if !strings.HasPrefix(loaded.Source, projectRoot) {
		t.Errorf("deploy Source = %q, want under project root %q", loaded.Source, projectRoot)
	}
	if loaded.Rank != RankProject {
		t.Errorf("deploy Rank = %d, want %d", loaded.Rank, RankProject)
	}
}

// Test 2: a disable-model-invocation skill is excluded from Catalog(), yet Load
// still returns it and ExtractUserInvocation finds it via /name (its only entry).
func TestDisableModelInvocation(t *testing.T) {
	root := t.TempDir()
	writeFlat(t, root, "secret",
		"---\nname: secret\ndescription: A user-only skill.\ndisable-model-invocation: true\n---\nSecret instructions.")
	writeFlat(t, root, "greet",
		"---\nname: greet\ndescription: Greet the user.\n---\nSay hello.")

	reg := discoverInto(t, []Root{{Dir: root, Rank: RankProject}})

	for _, s := range reg.Catalog() {
		if s.Name == "secret" {
			t.Fatal("secret must be excluded from the model catalog")
		}
	}

	s, ok := reg.Load("secret")
	if !ok {
		t.Fatal("Load(secret) not found")
	}
	if s.ModelInvocable {
		t.Error("secret.ModelInvocable = true, want false")
	}
	if !strings.Contains(s.Body, "Secret instructions.") {
		t.Errorf("secret body = %q, want it to contain the instructions", s.Body)
	}

	got, ok := ExtractUserInvocation("please /secret now", reg)
	if !ok {
		t.Fatal("ExtractUserInvocation(/secret) not found")
	}
	if got.Name != "secret" {
		t.Errorf("ExtractUserInvocation name = %q, want secret", got.Name)
	}
}

// Test 3: Load re-reads the body (full Body returned) while the Catalog carries only
// name+description (never the body).
func TestLoadRereadsBodyCatalogHasNone(t *testing.T) {
	root := t.TempDir()
	path := writeFlat(t, root, "notes", "---\nname: notes\ndescription: Take notes.\n---\nOriginal body.")

	reg := discoverInto(t, []Root{{Dir: root, Rank: RankProject}})

	cat := reg.Catalog()
	if len(cat) != 1 {
		t.Fatalf("catalog size = %d, want 1", len(cat))
	}
	if cat[0].Body != "" {
		t.Errorf("catalog entry carried a body: %q, want empty", cat[0].Body)
	}
	if cat[0].Description != "Take notes." {
		t.Errorf("catalog description = %q, want %q", cat[0].Description, "Take notes.")
	}

	first, ok := reg.Load("notes")
	if !ok || !strings.Contains(first.Body, "Original body.") {
		t.Fatalf("Load(notes) body = %q, want it to contain Original body.", first.Body)
	}

	// Edit the file on disk; the next Load must reflect it (body reparsed each load).
	if err := os.WriteFile(path, []byte("---\nname: notes\ndescription: Take notes.\n---\nEdited body."), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	second, ok := reg.Load("notes")
	if !ok || !strings.Contains(second.Body, "Edited body.") {
		t.Fatalf("second Load(notes) body = %q, want it to contain Edited body.", second.Body)
	}
}

// Test 4: the skill tool returns the body for a known name and IsError for an
// unknown one.
func TestSkillTool(t *testing.T) {
	root := t.TempDir()
	writeFlat(t, root, "greet", "---\nname: greet\ndescription: Greet the user.\n---\nSay hello to the user.")

	reg := discoverInto(t, []Root{{Dir: root, Rank: RankProject}})
	tool := SkillTool(reg)
	if tool.Schema.Name != "skill" {
		t.Fatalf("tool name = %q, want skill", tool.Schema.Name)
	}

	res, err := tool.Execute(hx.ToolRunContext{}, []byte(`{"name":"greet"}`))
	if err != nil {
		t.Fatalf("Execute returned a Go error: %v (must never happen)", err)
	}
	if res.IsError {
		t.Fatalf("known skill returned IsError; value=%s", res.Value)
	}
	if len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "Say hello to the user.") {
		t.Errorf("tool result did not carry the body; content=%+v", res.Content)
	}

	bad, err := tool.Execute(hx.ToolRunContext{}, []byte(`{"name":"nope"}`))
	if err != nil {
		t.Fatalf("Execute(unknown) returned a Go error: %v", err)
	}
	if !bad.IsError {
		t.Error("unknown skill did not return IsError")
	}
}

// Test 5: ExtractUserInvocation resolves a leading /token, ignores messages with no
// slash, and rejects an unknown skill name.
func TestExtractUserInvocation(t *testing.T) {
	root := t.TempDir()
	writeFlat(t, root, "deploy", "---\nname: deploy\ndescription: Deploy it.\n---\nDeploy steps.")
	reg := discoverInto(t, []Root{{Dir: root, Rank: RankProject}})

	if got, ok := ExtractUserInvocation("/deploy do the thing", reg); !ok || got.Name != "deploy" {
		t.Errorf("ExtractUserInvocation(/deploy ...) = (%q,%v), want (deploy,true)", got.Name, ok)
	}
	if _, ok := ExtractUserInvocation("no slash here", reg); ok {
		t.Error("ExtractUserInvocation(no slash) = true, want false")
	}
	if _, ok := ExtractUserInvocation("/unknown-skill", reg); ok {
		t.Error("ExtractUserInvocation(/unknown-skill) = true, want false")
	}
}
