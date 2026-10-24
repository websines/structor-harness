package llm

import (
	"context"
	"sync"

	"github.com/websines/structor-harness/harness/hx"
)

// FakeAdapter replays a fixed script over the channel — no network, fully
// deterministic. It is what sibling spine packages and the integration test drive
// to exercise the loop without a live model. The Nth Stream call replays
// Script[N]; once the script is exhausted it emits a lone terminal
// ChunkFinish{FinishStop} so a consumer always sees a clean end.
//
// Each scripted slice should carry its own terminal ChunkFinish — the adapter
// replays chunks verbatim and does not append one.
type FakeAdapter struct {
	Script [][]hx.StreamChunk

	mu   sync.Mutex
	next int
}

var _ hx.LlmAdapter = (*FakeAdapter)(nil)

// Route is the provider id this adapter answers to.
func (f *FakeAdapter) Route() string { return "fake" }

// Stream returns a channel replaying the next scripted slice. It respects ctx:
// cancellation stops the replay and closes the channel.
func (f *FakeAdapter) Stream(ctx context.Context, opts hx.GenerateOptions) (<-chan hx.StreamChunk, error) {
	f.mu.Lock()
	var chunks []hx.StreamChunk
	if f.next < len(f.Script) {
		chunks = f.Script[f.next]
		f.next++
	} else {
		chunks = []hx.StreamChunk{{Kind: hx.ChunkFinish, Finish: hx.FinishStop}}
	}
	f.mu.Unlock()

	out := make(chan hx.StreamChunk)
	go func() {
		defer close(out)
		for _, c := range chunks {
			select {
			case <-ctx.Done():
				return
			case out <- c:
			}
		}
	}()
	return out, nil
}
