package correct

import (
	"strings"
	"testing"

	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/harness/prompt"
	"github.com/websines/structor-harness/harness/tools"
)

// advisoryText extracts the advisory the RepeatGuard appended to a result value,
// or "" when none was appended.
func advisoryText(value string) string {
	i := strings.Index(value, advisoryMarker)
	if i < 0 {
		return ""
	}
	return strings.TrimSpace(value[i+len(advisoryMarker):])
}

func okNext(value string) hx.ToolExecFn {
	return func(in hx.ToolExecutionInput) hx.ToolResult {
		return hx.ToolResult{Value: []byte(value)}
	}
}

// 1. RepeatGuard: 3 identical calls → the 3rd advises; a different-args call
// resets the count (no advisory).
func TestRepeatGuardAppendsAdvisoryAndResets(t *testing.T) {
	g := NewRepeatGuard()
	base := `{"ok":true}`
	exec := g.Middleware()(okNext(base))
	in := hx.ToolExecutionInput{Name: "grep", Args: []byte(`{"q":"x"}`), Initiator: "agent-1"}

	if r := exec(in); advisoryText(string(r.Value)) != "" {
		t.Fatalf("call 1 must not advise: %s", r.Value)
	}
	if r := exec(in); advisoryText(string(r.Value)) != "" {
		t.Fatalf("call 2 must not advise: %s", r.Value)
	}
	r3 := exec(in)
	if advisoryText(string(r3.Value)) == "" {
		t.Fatalf("call 3 must advise: %s", r3.Value)
	}
	if !strings.Contains(string(r3.Value), base) {
		t.Fatalf("advisory must be appended, not replace the result: %s", r3.Value)
	}
	if len(r3.AdditionalContext) == 0 {
		t.Fatal("advisory should also ride AdditionalContext (dsh's injected-context delivery)")
	}

	// A different-args call resets the consecutive counter → no advisory.
	diff := hx.ToolExecutionInput{Name: "grep", Args: []byte(`{"q":"y"}`), Initiator: "agent-1"}
	if r := exec(diff); advisoryText(string(r.Value)) != "" {
		t.Fatalf("different args must reset the count (no advisory): %s", r.Value)
	}

	// Counting is per-initiator: agent-1's loop must not trip agent-2.
	if r := exec(hx.ToolExecutionInput{Name: "grep", Args: []byte(`{"q":"x"}`), Initiator: "agent-2"}); advisoryText(string(r.Value)) != "" {
		t.Fatalf("a fresh initiator must start at count 1 (no advisory): %s", r.Value)
	}
}

// 2. Thresholds escalate at 3/5/8, and past 8 the guard still advises (not silent).
func TestRepeatGuardThresholdsEscalate(t *testing.T) {
	g := NewRepeatGuard()
	exec := g.Middleware()(okNext("{}"))
	in := hx.ToolExecutionInput{Name: "read", Args: []byte(`{"p":"a"}`), Initiator: "a"}

	var adv [11]string
	for n := 1; n <= 10; n++ {
		adv[n] = advisoryText(string(exec(in).Value))
	}

	// Off-threshold counts stay silent.
	for _, n := range []int{1, 2, 4, 6, 7} {
		if adv[n] != "" {
			t.Fatalf("count %d must not advise: %q", n, adv[n])
		}
	}
	// Thresholds fire.
	for _, n := range []int{3, 5, 8} {
		if adv[n] == "" {
			t.Fatalf("threshold %d must advise", n)
		}
	}
	// The advisory text escalates across the tiers.
	if adv[3] == adv[5] || adv[5] == adv[8] {
		t.Fatalf("advisory must change/intensify across thresholds:\n3=%q\n5=%q\n8=%q", adv[3], adv[5], adv[8])
	}
	if !strings.Contains(adv[3], "notice") {
		t.Fatalf("first threshold should be a notice: %q", adv[3])
	}
	if !strings.Contains(adv[5], "warning") {
		t.Fatalf("middle threshold should be a warning: %q", adv[5])
	}
	if !strings.Contains(adv[8], "ALERT") {
		t.Fatalf("top threshold should be an ALERT: %q", adv[8])
	}
	// IMPROVEMENT over dsh: past the highest threshold it does NOT go silent.
	if adv[9] == "" || adv[10] == "" {
		t.Fatalf("past the top threshold must still advise (not silent): 9=%q 10=%q", adv[9], adv[10])
	}
	if !strings.Contains(adv[9], "ALERT") {
		t.Fatalf("past-top advisory should stay at the top tier: %q", adv[9])
	}
}

// 3. FailureMemory records/surfaces failures; its middleware records only on error.
func TestFailureMemory(t *testing.T) {
	m := NewFailureMemory()
	m.Record(Failure{Action: "bash", Reason: "exit 1: permission denied", Signature: "sig-1"})
	m.Record(Failure{Action: "write_file", Reason: "path is read-only", Signature: "sig-2"})

	if got := len(m.Recent(10)); got != 2 {
		t.Fatalf("Recent(10) should return both failures, got %d", got)
	}
	adv := m.Advisory(10)
	for _, want := range []string{"bash", "permission denied", "write_file", "read-only"} {
		if !strings.Contains(adv, want) {
			t.Fatalf("advisory missing %q:\n%s", want, adv)
		}
	}

	// Middleware records on IsError, not on success.
	m2 := NewFailureMemory()
	mw := m2.Middleware()
	failExec := mw(func(in hx.ToolExecutionInput) hx.ToolResult {
		return hx.ToolResult{IsError: true, Value: []byte(`{"error":"boom"}`)}
	})
	failExec(hx.ToolExecutionInput{Name: "grep", Args: []byte(`{"q":1}`)})
	if got := len(m2.Recent(10)); got != 1 {
		t.Fatalf("middleware should record 1 failure on IsError, got %d", got)
	}
	okExec := mw(okNext(`{"ok":true}`))
	okExec(hx.ToolExecutionInput{Name: "grep", Args: []byte(`{"q":1}`)})
	if got := len(m2.Recent(10)); got != 1 {
		t.Fatalf("middleware must NOT record on success (still 1 expected), got %d", got)
	}
	f := m2.Recent(1)[0]
	if f.Action != "grep" {
		t.Fatalf("recorded Action = %q, want grep", f.Action)
	}
	if !strings.Contains(f.Reason, "boom") {
		t.Fatalf("recorded Reason should excerpt the error value: %q", f.Reason)
	}
	if f.Signature == "" {
		t.Fatal("recorded Signature should be set (hash of tool+args)")
	}
}

// 4. RecoveryMessage is idempotency-aware.
func TestRecoveryMessage(t *testing.T) {
	idem := RecoveryMessage("read_file", true, "temporary network blip")
	if !strings.Contains(strings.ToLower(idem), "safe to retry") {
		t.Fatalf("idempotent recovery should say it is safe to retry: %s", idem)
	}

	notIdem := RecoveryMessage("write_file", false, "disk error")
	low := strings.ToLower(notIdem)
	if !strings.Contains(low, "verify external state") {
		t.Fatalf("non-idempotent recovery should tell the model to verify state: %s", notIdem)
	}
	if !strings.Contains(low, "do not retry blindly") {
		t.Fatalf("non-idempotent recovery should warn against blind retry: %s", notIdem)
	}
}

// Integration: a failing loop driven through the real ToolRuntime + SystemPrompt
// seams the Component wires — the guard nudges, the memory records, and the past
// failure folds into the assembled prompt.
func TestFailingLoopThroughRealSeams(t *testing.T) {
	rt := tools.New(tools.Config{})
	guard := NewRepeatGuard()
	mem := NewFailureMemory()
	rt.Use(guard.Middleware()) // outermost
	rt.Use(mem.Middleware())   // inner

	dispose, err := rt.Register(hx.ToolDefinition{
		Schema: hx.ToolSchema{Name: "flaky"},
		Execute: func(rc hx.ToolRunContext, args []byte) (hx.ToolResult, error) {
			return hx.ToolResult{IsError: true, Value: []byte(`{"error":"still broken"}`)}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer dispose()

	in := hx.ToolExecutionInput{Name: "flaky", Args: []byte(`{"x":1}`), Initiator: "agent"}
	var last hx.ToolResult
	for i := 0; i < 3; i++ {
		last = rt.Execute(in)
	}
	if advisoryText(string(last.Value)) == "" {
		t.Fatalf("3rd identical failing call should carry the repeat advisory: %s", last.Value)
	}
	if got := len(mem.Recent(10)); got != 3 {
		t.Fatalf("failure memory should have recorded 3 failures, got %d", got)
	}

	sp := prompt.New()
	if _, err := sp.Context(promptOrder, func() (string, error) { return mem.Advisory(defaultRecentN), nil }); err != nil {
		t.Fatal(err)
	}
	out, err := sp.Assemble(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "flaky") || !strings.Contains(out, "still broken") {
		t.Fatalf("assembled prompt should surface the past failure:\n%s", out)
	}
}

// Component declares the seams it wires and carries an Apply.
func TestComponentDeclaration(t *testing.T) {
	c := Component()
	if c == nil || c.Name != "harness/correct" {
		t.Fatalf("unexpected component: %+v", c)
	}
	if c.Apply == nil {
		t.Fatal("Component must carry an Apply")
	}
	inj := map[string]bool{}
	for _, k := range c.Inject {
		inj[k] = true
	}
	for _, k := range []string{hx.KeyTools, hx.KeyPrompt} {
		if !inj[k] {
			t.Fatalf("Component must inject %q", k)
		}
	}
}
