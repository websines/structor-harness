package loop

import "github.com/websines/structor-harness/harness/hx"

// fold is a minimal chunk→blocks assembler. The canonical BlockAssembler is
// concrete in the llm package, which the loop must not import; so the loop
// re-implements the tiny fold it needs here. It consumes the same StreamChunk
// vocabulary the model runtime emits and reconstructs the ordered ContentBlocks
// of one assistant message — incrementally, so the full response is never held as
// a single buffered string (memory efficiency, §5.1).
//
// The fold is deliberately forgiving: a delta for a block index that never got an
// explicit block-start still materializes a block of the delta's kind, because
// not every adapter brackets its blocks. Block order is first-appearance order of
// the block index.
type fold struct {
	blocks  []*hx.ContentBlock
	byIndex map[int]*hx.ContentBlock
}

func newFold() *fold {
	return &fold{byIndex: make(map[int]*hx.ContentBlock)}
}

// ensure returns the block at index, creating it with kind if it is new. A block
// created earlier keeps its identity (and accumulated content) regardless of the
// kind passed on a later delta.
func (f *fold) ensure(index int, kind hx.BlockKind) *hx.ContentBlock {
	if b := f.byIndex[index]; b != nil {
		return b
	}
	b := &hx.ContentBlock{Kind: kind}
	f.byIndex[index] = b
	f.blocks = append(f.blocks, b)
	return b
}

// consume folds one chunk into the growing block set. Usage and finish chunks
// carry no block content and are ignored here (the caller reads their fields).
func (f *fold) consume(c hx.StreamChunk) {
	switch c.Kind {
	case hx.ChunkBlockStart:
		f.ensure(c.BlockIndex, c.BlockOf)
	case hx.ChunkTextDelta:
		b := f.ensure(c.BlockIndex, hx.BlockText)
		b.Text += c.Delta
	case hx.ChunkReasoningDelta:
		b := f.ensure(c.BlockIndex, hx.BlockReasoning)
		b.Text += c.Delta
	case hx.ChunkToolCallDelta:
		b := f.ensure(c.BlockIndex, hx.BlockToolCall)
		if c.ToolCallID != "" {
			b.ToolCallID = c.ToolCallID
		}
		if c.ToolName != "" {
			b.ToolName = c.ToolName
		}
		if c.ArgsDelta != "" {
			b.ToolArgs = append(b.ToolArgs, []byte(c.ArgsDelta)...)
		}
	case hx.ChunkBlockEnd, hx.ChunkUsage, hx.ChunkFinish:
		// no content to fold
	}
}

// result returns the assembled blocks as values, in appearance order.
func (f *fold) result() []hx.ContentBlock {
	out := make([]hx.ContentBlock, len(f.blocks))
	for i, b := range f.blocks {
		out[i] = *b
	}
	return out
}
