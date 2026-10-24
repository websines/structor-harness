package llm

import (
	"strings"

	"github.com/websines/structor-harness/harness/hx"
)

// BlockAssembler folds a stream of hx.StreamChunk into ordered content blocks
// plus the usage and finish reason the stream reported. It is incremental by
// construction: it holds only the ONE block currently open (a strings.Builder of
// text, or an accumulating tool-call), appending it to the completed list when a
// block boundary closes it. The whole response is never buffered as one string —
// memory stays bounded to the largest single block (design §5.1).
//
// The zero value is ready to use. Feed each streamed chunk to Push in order;
// read the result with Blocks / Usage / Finish once a terminal ChunkFinish (or a
// closing ChunkBlockEnd) has flushed the last open block.
type BlockAssembler struct {
	blocks []hx.ContentBlock

	// The single open block. haveCur guards curKind; text backs text/reasoning
	// blocks; toolID/toolName/toolArgs back a tool-call block.
	haveCur  bool
	curKind  hx.BlockKind
	text     strings.Builder
	toolID   string
	toolName string
	toolArgs strings.Builder

	usage  hx.Usage
	finish hx.FinishReason
	errMsg string
}

// Push folds one chunk into the assembly.
//
// Deltas open a block implicitly: text-delta accumulates a text block,
// reasoning-delta a reasoning block, tool-call-delta a tool-call block keyed by
// ToolCallID (a new id, or a switch away from a tool-call block, closes the open
// one and starts a fresh block). An explicit block-start opens a block and
// block-end closes it; usage records token accounting; a terminal finish flushes
// the open block and records the reason.
func (a *BlockAssembler) Push(c hx.StreamChunk) {
	switch c.Kind {
	case hx.ChunkBlockStart:
		a.flush()
		a.haveCur = true
		a.curKind = c.BlockOf

	case hx.ChunkTextDelta:
		a.ensure(hx.BlockText)
		a.text.WriteString(c.Delta)

	case hx.ChunkReasoningDelta:
		a.ensure(hx.BlockReasoning)
		a.text.WriteString(c.Delta)

	case hx.ChunkToolCallDelta:
		// A different tool-call id delimits a new tool-call block.
		if a.haveCur && a.curKind == hx.BlockToolCall && c.ToolCallID != "" && c.ToolCallID != a.toolID {
			a.flush()
		}
		a.ensure(hx.BlockToolCall)
		if c.ToolCallID != "" {
			a.toolID = c.ToolCallID
		}
		if c.ToolName != "" {
			a.toolName = c.ToolName
		}
		a.toolArgs.WriteString(c.ArgsDelta)

	case hx.ChunkBlockEnd:
		a.flush()

	case hx.ChunkUsage:
		if c.Usage != nil {
			a.usage = *c.Usage
		}

	case hx.ChunkFinish:
		a.flush()
		a.finish = c.Finish
		a.errMsg = c.Err
	}
}

// ensure guarantees an open block of kind k, flushing an open block of any other
// kind first so blocks never mix content.
func (a *BlockAssembler) ensure(k hx.BlockKind) {
	if a.haveCur && a.curKind == k {
		return
	}
	if a.haveCur {
		a.flush()
	}
	a.haveCur = true
	a.curKind = k
}

// flush appends the open block (if any) to the completed list and resets the
// open-block state. Empty blocks are dropped — a block-start with no delta, or a
// tool-call that never named anything, is not a content block.
func (a *BlockAssembler) flush() {
	if !a.haveCur {
		return
	}
	switch a.curKind {
	case hx.BlockText:
		if s := a.text.String(); s != "" {
			a.blocks = append(a.blocks, hx.ContentBlock{Kind: hx.BlockText, Text: s})
		}
	case hx.BlockReasoning:
		if s := a.text.String(); s != "" {
			a.blocks = append(a.blocks, hx.ContentBlock{Kind: hx.BlockReasoning, Text: s})
		}
	case hx.BlockToolCall:
		if a.toolID != "" || a.toolName != "" || a.toolArgs.Len() > 0 {
			blk := hx.ContentBlock{Kind: hx.BlockToolCall, ToolCallID: a.toolID, ToolName: a.toolName}
			if a.toolArgs.Len() > 0 {
				blk.ToolArgs = []byte(a.toolArgs.String())
			}
			a.blocks = append(a.blocks, blk)
		}
	}
	a.reset()
}

func (a *BlockAssembler) reset() {
	a.haveCur = false
	a.curKind = ""
	a.text.Reset()
	a.toolID = ""
	a.toolName = ""
	a.toolArgs.Reset()
}

// Blocks returns the content blocks assembled so far, in stream order.
func (a *BlockAssembler) Blocks() []hx.ContentBlock { return a.blocks }

// Usage returns the token accounting a usage chunk reported (zero if none).
func (a *BlockAssembler) Usage() hx.Usage { return a.usage }

// Finish returns the terminal finish reason (empty until a finish chunk).
func (a *BlockAssembler) Finish() hx.FinishReason { return a.finish }

// Err returns the message a FinishError chunk carried, if any.
func (a *BlockAssembler) Err() string { return a.errMsg }
