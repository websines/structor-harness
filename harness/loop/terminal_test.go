package loop

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/websines/structor-harness/harness/compaction"
	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/harness/session"
	"github.com/websines/structor-harness/record"
)

func TestCancellationDuringCompactionStopsNativeTurn(t *testing.T) {
	cas, err := record.NewLocalCAS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := session.NewStore(cas).Create("cancel-compaction")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	model := &fakeLLM{scripts: [][]hx.StreamChunk{toolCallScript(), stopScript()}}
	engine := &compaction.Engine{
		MaxMessages: 1,
		Summarize: func(ctx context.Context, _ []hx.Message) (string, error) {
			close(started)
			<-ctx.Done()
			return "", ctx.Err()
		},
	}
	agent := newAgent(hx.AgentConfig{}, mapServices{
		hx.KeyLLM: model, hx.KeyTools: &fakeTools{result: hx.ToolResult{Value: []byte(`"done"`)}},
		hx.KeyPrompt: fakePrompt{}, hx.KeyCompaction: engine,
	}, sess)
	t.Cleanup(func() {
		agent.Cancel("test cleanup")
		joined, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = agent.WhenIdle(joined)
		if joined.Err() != nil {
			t.Errorf("test cleanup could not join the native driver: %v", joined.Err())
		}
	})
	agent.Followup("call the tool and then compact")
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("native compaction did not start")
	}
	agent.Cancel("user stopped compaction")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var terminal *hx.TurnError
	if err := agent.WhenIdle(ctx); !errors.Is(err, context.Canceled) || !errors.As(err, &terminal) || terminal.Reason != "cancelled" {
		t.Fatalf("canceled native compaction lost its terminal cause: %v", err)
	}
	if n := model.callCount(); n != 1 {
		t.Fatalf("canceled native compaction dispatched %d model requests, want one", n)
	}
}

func TestTaskSurvivesRealCompaction(t *testing.T) {
	cas, err := record.NewLocalCAS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := session.NewStore(cas).Create("task-retention")
	if err != nil {
		t.Fatal(err)
	}
	task := "Implement the requested change.\nMANDATORY: retain the complete requirement.\n" + strings.Repeat("acceptance detail ", 100)
	model := &fakeLLM{scripts: [][]hx.StreamChunk{toolCallScript(), stopScript()}}
	agent := newAgent(hx.AgentConfig{}, mapServices{
		hx.KeyLLM: model, hx.KeyTools: &fakeTools{result: hx.ToolResult{Value: []byte(`"done"`)}},
		hx.KeyPrompt: fakePrompt{}, hx.KeyCompaction: &compaction.Engine{MaxTokens: 32, MaxMessages: 1},
	}, sess)
	t.Cleanup(func() {
		agent.Cancel("test cleanup")
		joined, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = agent.WhenIdle(joined)
		if joined.Err() != nil {
			t.Errorf("test cleanup could not join the native driver: %v", joined.Err())
		}
	})
	agent.Followup(task)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := agent.WhenIdle(ctx); err != nil {
		t.Fatal(err)
	}
	second, ok := model.callAt(1)
	if !ok || !strings.Contains(second.System, task) {
		t.Fatal("full task disappeared from the system prompt after compaction")
	}
	for _, msg := range second.Messages {
		for _, block := range msg.Content {
			if strings.Contains(block.Text, "MANDATORY") {
				t.Fatal("test did not actually compact the task out of message history")
			}
		}
	}
}

func TestStepLimitIsRecordedAndReported(t *testing.T) {
	sess := &fakeSession{}
	agent := newAgent(hx.AgentConfig{MaxSteps: 1}, mapServices{
		hx.KeyLLM: &loopingLLM{}, hx.KeyTools: &fakeTools{}, hx.KeyPrompt: fakePrompt{},
	}, sess)
	agent.Followup("continue until done")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := agent.WhenIdle(ctx); !errors.Is(err, hx.ErrStepLimit) {
		t.Fatalf("step exhaustion was reported as success: %v", err)
	}
	events := sess.Events()
	last := events[len(events)-1]
	var outcome hx.TurnOutcome
	if last.Kind != hx.EvTurnEnd || json.Unmarshal(last.Data, &outcome) != nil {
		t.Fatalf("missing terminal outcome: %+v", last)
	}
	if outcome.Reason != "step_limit" || outcome.Steps != 1 || outcome.Error == "" {
		t.Fatalf("wrong terminal outcome: %+v", outcome)
	}
}
