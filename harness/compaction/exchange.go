package compaction

import "github.com/websines/structor-harness/harness/hx"

// exchangeGroups keeps an assistant's calls and all corresponding results atomic.
// Multi-call messages connect their result messages into the same group.
func exchangeGroups(msgs []hx.Message) [][]int {
	parents := make([]int, len(msgs))
	for i := range parents {
		parents[i] = i
	}
	root := func(i int) int {
		for parents[i] != i {
			i = parents[i]
		}
		return i
	}
	calls := make(map[string]int)
	for i, m := range msgs {
		for _, b := range m.Content {
			if b.Kind == hx.BlockToolCall && b.ToolCallID != "" {
				calls[b.ToolCallID] = i
			}
		}
	}
	for i, m := range msgs {
		for _, b := range m.Content {
			if b.Kind != hx.BlockToolResult {
				continue
			}
			if call, ok := calls[b.ToolCallID]; ok {
				a, c := root(i), root(call)
				if a < c {
					parents[c] = a
				} else {
					parents[a] = c
				}
			}
		}
	}
	var groups [][]int
	indices := make(map[int]int)
	for i := range msgs {
		r := root(i)
		g, ok := indices[r]
		if !ok {
			g = len(groups)
			indices[r] = g
			groups = append(groups, nil)
		}
		groups[g] = append(groups[g], i)
	}
	return groups
}
