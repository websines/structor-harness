package spawn

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/harness/session"
	"github.com/websines/structor-harness/record"
)

// --- a FAKE AgentFactory standing in for the loop ---------------------------
//
// It creates real sessions in a real (shared) session store so the tool's
// find-collection walks the SAME record the child wrote to — exactly the "child
// shares the parent's record" property. The fake agent's Followup appends a canned
// finding fact; its WhenIdle settles immediately (or blocks on a gate, for the cap
// test).

type fakeFactory struct {
	store *session.Store

	// onFollowup runs on the child's session when the fake agent is driven; it stands
	// in for a real research turn by appending finding facts.
	onFollowup func(sess hx.Session)

	// reached (if set) is closed the first time ANY child enters WhenIdle (guarded by
	// reachedOnce so a later child is a no-op); block (if set) holds that child in
	// WhenIdle until the test closes it. Used by the cap test.
	reached     chan struct{}
	reachedOnce sync.Once
	block       chan struct{}

	mu      sync.Mutex
	lastCfg hx.AgentConfig
	created int
}

func (f *fakeFactory) Create(cfg hx.AgentConfig) (hx.AgentHandle, error) {
	f.mu.Lock()
	f.lastCfg = cfg
	f.created++
	f.mu.Unlock()

	sess, err := f.store.Create(cfg.SessionID)
	if err != nil {
		return nil, err
	}
	return &fakeHandle{agent: &fakeAgent{
		id:          "fake-" + cfg.SessionID,
		sess:        sess,
		onFollowup:  f.onFollowup,
		reached:     f.reached,
		reachedOnce: &f.reachedOnce,
		block:       f.block,
	}}, nil
}

type fakeHandle struct {
	agent    *fakeAgent
	disposed bool
}

func (h *fakeHandle) Agent() hx.Agent { return h.agent }
func (h *fakeHandle) Dispose()        { h.disposed = true }

type fakeAgent struct {
	id          string
	sess        hx.Session
	onFollowup  func(sess hx.Session)
	reached     chan struct{}
	reachedOnce *sync.Once
	block       chan struct{}
}

func (a *fakeAgent) ID() string                      { return a.id }
func (a *fakeAgent) Send(hx.DeliverKind, hx.Message) {}
func (a *fakeAgent) Cancel(string)                   {}

func (a *fakeAgent) Followup(string) {
	if a.onFollowup != nil {
		a.onFollowup(a.sess)
	}
}

func (a *fakeAgent) WhenIdle(ctx context.Context) error {
	if a.reached != nil && a.reachedOnce != nil {
		a.reachedOnce.Do(func() { close(a.reached) })
	}
	if a.block != nil {
		select {
		case <-a.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// appendFinding is the canned "child did research" effect: one cited assistant
// finding, appended as a surface fact — the shape DeriveMessages projects.
func appendFinding(text string) func(sess hx.Session) {
	return func(sess hx.Session) {
		data, _ := json.Marshal(hx.Message{
			Role:    hx.RoleAssistant,
			Content: []hx.ContentBlock{{Kind: hx.BlockText, Text: text}},
		})
		_, _ = sess.Append(hx.EvAssistantMessage, data, true)
	}
}

func newStore(t *testing.T) *session.Store {
	t.Helper()
	cas, err := record.NewLocalCAS(t.TempDir())
	if err != nil {
		t.Fatalf("new CAS: %v", err)
	}
	return session.NewStore(cas).StampActor(session.Config{PrincipalID: "principal:test", Commit: "test"})
}

// assistantFactID reads back the fact id of the finding the fake appended, so the
// test asserts the tool returned the REAL content address, not a placeholder.
func assistantFactID(t *testing.T, store *session.Store, nodeID string) string {
	t.Helper()
	sess, ok := store.Get(nodeID)
	if !ok {
		t.Fatalf("session %q not found in store", nodeID)
	}
	for _, ev := range sess.Events() {
		if ev.Kind == hx.EvAssistantMessage {
			return ev.FactID
		}
	}
	t.Fatalf("no assistant finding event in session %q", nodeID)
	return ""
}

// TestSpawn_CreatesChildAndCollectsFindings covers the happy path: the tool creates
// the child with SessionID=node_id and PrincipalID=parent, awaits it, and returns the
// collected finding ids + summary.
func TestSpawn_CreatesChildAndCollectsFindings(t *testing.T) {
	store := newStore(t)
	const parent = "principal:orchestrator"
	factory := &fakeFactory{store: store, onFollowup: appendFinding("FINDING: sky is blue [https://example.com]")}

	def := Tool(Deps{
		Factory:         factory,
		Sessions:        store,
		ParentPrincipal: parent,
		MaxChildren:     4,
		ChildMaxSteps:   7,
	})

	raw, _ := json.Marshal(args{Subquery: "why is the sky blue", NodeID: "n1"})
	res, err := def.Execute(hx.ToolRunContext{Ctx: context.Background(), CallID: "call-1", Initiator: "principal:should-be-ignored"}, raw)
	if err != nil {
		t.Fatalf("Execute returned a Go error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %s", res.Value)
	}

	// Child created exactly once, with the config the contract requires.
	if factory.created != 1 {
		t.Fatalf("expected 1 child created, got %d", factory.created)
	}
	if factory.lastCfg.SessionID != "n1" {
		t.Errorf("child SessionID = %q, want %q", factory.lastCfg.SessionID, "n1")
	}
	if factory.lastCfg.PrincipalID != parent {
		t.Errorf("child PrincipalID = %q, want %q (initiator stamped to parent)", factory.lastCfg.PrincipalID, parent)
	}
	if factory.lastCfg.MaxSteps != 7 {
		t.Errorf("child MaxSteps = %d, want 7 (the cap)", factory.lastCfg.MaxSteps)
	}

	var out result
	if err := json.Unmarshal(res.Value, &out); err != nil {
		t.Fatalf("result is not valid JSON: %v (%s)", err, res.Value)
	}
	if out.NodeID != "n1" {
		t.Errorf("result node_id = %q, want %q", out.NodeID, "n1")
	}
	want := assistantFactID(t, store, "n1")
	if len(out.FindingIDs) != 1 || out.FindingIDs[0] != want {
		t.Errorf("finding_ids = %v, want [%s] (the child's appended fact id)", out.FindingIDs, want)
	}
	if !strings.Contains(out.Summary, "sky is blue") {
		t.Errorf("summary = %q, want it to carry the child's final finding text", out.Summary)
	}
}

// TestSpawn_UsesInitiatorWhenNoParentPrincipal proves the fallback: with no explicit
// ParentPrincipal, the child is stamped with the calling tool's Initiator (the parent
// agent's verified principal).
func TestSpawn_UsesInitiatorWhenNoParentPrincipal(t *testing.T) {
	store := newStore(t)
	factory := &fakeFactory{store: store, onFollowup: appendFinding("FINDING: x [https://src]")}

	def := Tool(Deps{Factory: factory, Sessions: store}) // ParentPrincipal empty

	raw, _ := json.Marshal(args{Subquery: "q", NodeID: "node-A"})
	if _, err := def.Execute(hx.ToolRunContext{Ctx: context.Background(), Initiator: "principal:parent-init"}, raw); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if factory.lastCfg.PrincipalID != "principal:parent-init" {
		t.Errorf("child PrincipalID = %q, want the caller's initiator %q", factory.lastCfg.PrincipalID, "principal:parent-init")
	}
}

// TestSpawn_RejectsBadArgs covers the graceful-error paths for missing args.
func TestSpawn_RejectsBadArgs(t *testing.T) {
	store := newStore(t)
	factory := &fakeFactory{store: store, onFollowup: appendFinding("f")}
	def := Tool(Deps{Factory: factory, Sessions: store})

	cases := []string{
		`{"subquery":"","node_id":"n"}`,
		`{"subquery":"q","node_id":""}`,
		`{not json`,
	}
	for _, c := range cases {
		res, err := def.Execute(hx.ToolRunContext{Ctx: context.Background()}, []byte(c))
		if err != nil {
			t.Fatalf("Execute(%s) returned a Go error: %v", c, err)
		}
		if !res.IsError {
			t.Errorf("Execute(%s) should be a graceful error result", c)
		}
	}
	if factory.created != 0 {
		t.Errorf("no child should be created for bad args, got %d", factory.created)
	}
}

// TestSpawn_ConcurrencyCapRefuses covers the bounded fan-out path: with MaxChildren=1,
// while one child is in flight (blocked in WhenIdle) a second spawn is REFUSED with a
// graceful cap error rather than blocking or exceeding the cap. Releasing the first
// then lets it complete and return its finding.
func TestSpawn_ConcurrencyCapRefuses(t *testing.T) {
	store := newStore(t)
	block := make(chan struct{})
	reached := make(chan struct{})
	factory := &fakeFactory{
		store:      store,
		onFollowup: appendFinding("FINDING: held [https://src]"),
		reached:    reached,
		block:      block,
	}

	def := Tool(Deps{Factory: factory, Sessions: store, ParentPrincipal: "principal:p", MaxChildren: 1})

	// Goroutine A acquires the only slot and parks in WhenIdle.
	type outcome struct {
		res hx.ToolResult
		err error
	}
	firstDone := make(chan outcome, 1)
	go func() {
		raw, _ := json.Marshal(args{Subquery: "held query", NodeID: "n1"})
		res, err := def.Execute(hx.ToolRunContext{Ctx: context.Background(), CallID: "a"}, raw)
		firstDone <- outcome{res, err}
	}()

	// Wait until A is genuinely in flight (slot held) before probing the cap.
	<-reached

	// A second spawn must be refused immediately — the cap is full.
	raw2, _ := json.Marshal(args{Subquery: "second", NodeID: "n2"})
	res2, err := def.Execute(hx.ToolRunContext{Ctx: context.Background(), CallID: "b"}, raw2)
	if err != nil {
		t.Fatalf("second Execute returned a Go error: %v", err)
	}
	if !res2.IsError {
		t.Fatalf("second spawn should be refused while the cap is full; got %s", res2.Value)
	}
	if !strings.Contains(string(res2.Value), "cap reached") {
		t.Errorf("refusal should name the cap; got %s", res2.Value)
	}
	if factory.created != 1 {
		t.Errorf("the refused spawn must NOT create a child; created = %d", factory.created)
	}

	// Release the first child; it completes and returns its finding.
	close(block)
	first := <-firstDone
	if first.err != nil {
		t.Fatalf("first Execute: %v", first.err)
	}
	if first.res.IsError {
		t.Fatalf("first spawn should have succeeded; got %s", first.res.Value)
	}
	var out result
	if err := json.Unmarshal(first.res.Value, &out); err != nil {
		t.Fatalf("first result JSON: %v", err)
	}
	if len(out.FindingIDs) != 1 {
		t.Errorf("first spawn should return 1 finding, got %v", out.FindingIDs)
	}

	// And after the slot is released, a fresh spawn succeeds (the semaphore recovered).
	raw3, _ := json.Marshal(args{Subquery: "third", NodeID: "n3"})
	res3, err := def.Execute(hx.ToolRunContext{Ctx: context.Background(), CallID: "c"}, raw3)
	if err != nil || res3.IsError {
		t.Fatalf("post-release spawn should succeed; err=%v res=%s", err, res3.Value)
	}
}
