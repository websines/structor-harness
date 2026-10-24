package prompt

import (
	"errors"
	"strings"
	"testing"
)

func TestCachePrefixStopsBeforeDynamicOrInterpolatedSections(t *testing.T) {
	for _, tc := range []struct {
		name              string
		dynamic, variable bool
	}{
		{name: "dynamic", dynamic: true},
		{name: "interpolated", variable: true},
		{name: "all static"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := New()
			p.Section(0, "instructions")
			p.Section(500, "skills")
			invocations := 0
			if tc.dynamic {
				p.Context(850, func() (string, error) { invocations++; return "", nil })
			}
			if tc.variable {
				p.Variable("workspace", func() string { invocations++; return "A" })
				p.Section(850, "{{workspace}}")
			}
			p.Section(1100, "workspace")
			full, prefix, err := p.AssembleWithCachePrefix(nil)
			if err != nil || !strings.HasPrefix(full, prefix) {
				t.Fatalf("invalid prefix: %q / %q %v", full, prefix, err)
			}
			if tc.dynamic || tc.variable {
				if prefix != "instructions\n\nskills" || invocations != 1 {
					t.Fatal("volatile content included in prefix or providers rendered twice")
				}
			} else if prefix != full {
				t.Fatal("static prefix unnecessarily shortened")
			}
		})
	}
}

func TestSectionsRenderInOrder(t *testing.T) {
	p := New()
	if _, err := p.Section(30, "gamma"); err != nil {
		t.Fatalf("Section: %v", err)
	}
	if _, err := p.Section(10, "alpha"); err != nil {
		t.Fatalf("Section: %v", err)
	}
	if _, err := p.Section(20, "beta"); err != nil {
		t.Fatalf("Section: %v", err)
	}

	out, err := p.Assemble(nil)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	ia, ib, ig := strings.Index(out, "alpha"), strings.Index(out, "beta"), strings.Index(out, "gamma")
	if !(ia >= 0 && ia < ib && ib < ig) {
		t.Fatalf("sections out of ascending order: %q (a=%d b=%d g=%d)", out, ia, ib, ig)
	}
}

func TestEqualOrderRendersInRegistrationOrder(t *testing.T) {
	p := New()
	p.Section(5, "first")
	p.Section(5, "second")
	out, err := p.Assemble(nil)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if strings.Index(out, "first") > strings.Index(out, "second") {
		t.Fatalf("equal-order tie not registration-ordered: %q", out)
	}
}

func TestVariableInterpolatesFromProvider(t *testing.T) {
	p := New()
	if _, err := p.Variable("name", func() string { return "world" }); err != nil {
		t.Fatalf("Variable: %v", err)
	}
	p.Section(0, "hello {{name}}")

	out, err := p.Assemble(nil)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if out != "hello world" {
		t.Fatalf("out = %q, want %q", out, "hello world")
	}
}

func TestRequestVarsOverrideProviders(t *testing.T) {
	p := New()
	p.Variable("who", func() string { return "provider" })
	p.Section(0, "hi {{who}}")

	out, err := p.Assemble(map[string]string{"who": "request"})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if out != "hi request" {
		t.Fatalf("out = %q, want request-var precedence", out)
	}
}

func TestUnknownVariableIsLoudError(t *testing.T) {
	p := New()
	p.Section(0, "value: {{missing}}")
	if _, err := p.Assemble(nil); err == nil {
		t.Fatal("expected a loud error for unresolved {{missing}}")
	}
}

func TestDisposerRemovesSection(t *testing.T) {
	p := New()
	p.Section(0, "keep")
	dispose, err := p.Section(1, "drop-me")
	if err != nil {
		t.Fatalf("Section: %v", err)
	}
	dispose()

	out, err := p.Assemble(nil)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if strings.Contains(out, "drop-me") {
		t.Fatalf("disposed section still present: %q", out)
	}
	if !strings.Contains(out, "keep") {
		t.Fatalf("kept section missing: %q", out)
	}
	dispose() // idempotent second dispose must not panic
}

func TestDisposerRemovesVariable(t *testing.T) {
	p := New()
	dispose, err := p.Variable("v", func() string { return "x" })
	if err != nil {
		t.Fatalf("Variable: %v", err)
	}
	p.Section(0, "{{v}}")

	if out, err := p.Assemble(nil); err != nil || out != "x" {
		t.Fatalf("before dispose: out=%q err=%v", out, err)
	}
	dispose()
	if _, err := p.Assemble(nil); err == nil {
		t.Fatal("expected unresolved error after the variable was disposed")
	}
}

func TestDuplicateVariableIsError(t *testing.T) {
	p := New()
	if _, err := p.Variable("dup", func() string { return "1" }); err != nil {
		t.Fatalf("Variable: %v", err)
	}
	if _, err := p.Variable("dup", func() string { return "2" }); err == nil {
		t.Fatal("expected an error registering a duplicate variable name")
	}
}

// A Context provider is DYNAMIC: unlike a static Section (fixed at registration) it
// is invoked on every Assemble, so its block reflects the underlying data AS OF
// that assemble. Mutating the data the provider closes over and re-assembling must
// change the output — the proof that Context is per-assemble, not static.
func TestContextProviderIsDynamicPerAssemble(t *testing.T) {
	p := New()
	state := "A"
	if _, err := p.Context(10, func() (string, error) { return "data=" + state, nil }); err != nil {
		t.Fatalf("Context: %v", err)
	}
	if _, err := p.Section(0, "head"); err != nil {
		t.Fatalf("Section: %v", err)
	}

	out1, err := p.Assemble(nil)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if !strings.Contains(out1, "data=A") {
		t.Fatalf("dynamic context block absent from assemble: %q", out1)
	}
	// The context is ordered (order 10) AFTER the static section (order 0).
	if strings.Index(out1, "head") > strings.Index(out1, "data=A") {
		t.Fatalf("context not ordered relative to sections: %q", out1)
	}

	// Change the underlying data; a static section would be frozen, a provider recomputes.
	state = "B"
	out2, err := p.Assemble(nil)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if strings.Contains(out2, "data=A") || !strings.Contains(out2, "data=B") {
		t.Fatalf("context did not recompute per-assemble (looks static): %q", out2)
	}
}

// A Context provider's disposer removes exactly its block, leaving the rest intact.
func TestContextProviderDisposerRemovesBlock(t *testing.T) {
	p := New()
	p.Section(0, "keep")
	dispose, err := p.Context(5, func() (string, error) { return "drop-me", nil })
	if err != nil {
		t.Fatalf("Context: %v", err)
	}
	if out, _ := p.Assemble(nil); !strings.Contains(out, "drop-me") {
		t.Fatalf("context block missing before dispose: %q", out)
	}
	dispose()
	out, err := p.Assemble(nil)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if strings.Contains(out, "drop-me") {
		t.Fatalf("disposed context still present: %q", out)
	}
	if !strings.Contains(out, "keep") {
		t.Fatalf("kept section lost: %q", out)
	}
	dispose() // idempotent
}

// A context provider that fails fails Assemble LOUDLY — a context that cannot be
// produced is never silently swallowed.
func TestContextProviderErrorIsLoud(t *testing.T) {
	p := New()
	if _, err := p.Context(0, func() (string, error) { return "", errors.New("boom") }); err != nil {
		t.Fatalf("Context: %v", err)
	}
	if _, err := p.Assemble(nil); err == nil {
		t.Fatal("expected a loud error when a context provider fails")
	}
}

// A nil context provider is rejected at registration.
func TestContextNilProviderRejected(t *testing.T) {
	p := New()
	if _, err := p.Context(0, nil); err == nil {
		t.Fatal("expected an error registering a nil context provider")
	}
}

// Sections and context providers share ONE ordering space, interleaving by order.
func TestSectionAndContextInterleaveByOrder(t *testing.T) {
	p := New()
	p.Section(30, "s30")
	p.Context(20, func() (string, error) { return "c20", nil })
	p.Section(10, "s10")
	out, err := p.Assemble(nil)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	i10, i20, i30 := strings.Index(out, "s10"), strings.Index(out, "c20"), strings.Index(out, "s30")
	if !(i10 >= 0 && i10 < i20 && i20 < i30) {
		t.Fatalf("section/context not interleaved by order: %q (10=%d 20=%d 30=%d)", out, i10, i20, i30)
	}
}
