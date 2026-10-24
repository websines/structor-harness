package fs

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"github.com/websines/structor-harness/harness/hx"
)

// The three model-facing file tools. Each is a plain hx.ToolDefinition with a
// hand-written JSON Schema and a typed body that calls the hx.FileSystem — no
// dsh inference DSL. A body NEVER panics and NEVER returns a Go error: a bad
// argument or a filesystem failure becomes a ToolResult{IsError:true} carrying
// the message, so the result surface stays a total function of its input.

// maxReadBytes caps the content read_file returns to the model. The file is read
// in full by the capability; only the model-visible slice is bounded (memory
// efficiency), and truncation is flagged so the model knows more remains.
const maxReadBytes = 64 << 10 // 64 KiB

// readFileTool returns the read_file definition bound to fs.
func readFileTool(fs hx.FileSystem) hx.ToolDefinition {
	return hx.ToolDefinition{
		Schema: hx.ToolSchema{
			Name:        "read_file",
			Description: "Read a UTF-8 text file, relative to the workspace root. Content is capped; a truncated flag reports when more remains. Reading a file is a prerequisite for overwriting it.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "File path relative to the workspace root.",
					},
				},
				"required":             []any{"path"},
				"additionalProperties": false,
			},
		},
		Concurrent: true, // pure read — safe alongside other reads
		Execute: func(_ hx.ToolRunContext, args []byte) (hx.ToolResult, error) {
			var in struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return toolErr("read_file: invalid arguments: %v", err), nil
			}
			content, err := fs.ReadText(in.Path)
			if err != nil {
				return toolErr("%v", err), nil
			}
			shown, truncated := truncateUTF8(content, maxReadBytes)
			return okResult(map[string]any{
				"path":      in.Path,
				"content":   shown,
				"bytes":     len(content),
				"truncated": truncated,
			}), nil
		},
	}
}

// writeFileTool returns the write_file definition bound to fs.
func writeFileTool(fs hx.FileSystem) hx.ToolDefinition {
	return hx.ToolDefinition{
		Schema: hx.ToolSchema{
			Name:        "write_file",
			Description: "Write a UTF-8 text file atomically, relative to the workspace root. Creating a new file is allowed; overwriting an existing file requires having read it first.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "File path relative to the workspace root.",
					},
					"content": map[string]any{
						"type":        "string",
						"description": "Full new contents of the file.",
					},
				},
				"required":             []any{"path", "content"},
				"additionalProperties": false,
			},
		},
		Execute: func(_ hx.ToolRunContext, args []byte) (hx.ToolResult, error) {
			var in struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return toolErr("write_file: invalid arguments: %v", err), nil
			}
			if err := fs.WriteText(in.Path, in.Content); err != nil {
				return toolErr("%v", err), nil
			}
			return okResult(map[string]any{
				"path":  in.Path,
				"bytes": len(in.Content),
			}), nil
		},
	}
}

// editFileTool returns the edit_file definition bound to fs.
func editFileTool(fs hx.FileSystem) hx.ToolDefinition {
	return hx.ToolDefinition{
		Schema: hx.ToolSchema{
			Name:        "edit_file",
			Description: "Replace the UNIQUE literal occurrence of old_string with new_string in a file (never a diff). Errors if old_string is not found or occurs more than once.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "File path relative to the workspace root.",
					},
					"old_string": map[string]any{
						"type":        "string",
						"description": "Exact text to replace; must occur exactly once.",
					},
					"new_string": map[string]any{
						"type":        "string",
						"description": "Replacement text.",
					},
				},
				"required":             []any{"path", "old_string", "new_string"},
				"additionalProperties": false,
			},
		},
		Execute: func(_ hx.ToolRunContext, args []byte) (hx.ToolResult, error) {
			var in struct {
				Path      string `json:"path"`
				OldString string `json:"old_string"`
				NewString string `json:"new_string"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return toolErr("edit_file: invalid arguments: %v", err), nil
			}
			if err := fs.EditText(in.Path, in.OldString, in.NewString); err != nil {
				return toolErr("%v", err), nil
			}
			return okResult(map[string]any{
				"path":     in.Path,
				"replaced": 1,
			}), nil
		},
	}
}

// RegisterTools registers read_file, write_file and edit_file (all bound to fs)
// on tr and returns a single disposer that withdraws all three in LIFO order. If
// any registration fails, the ones already installed are rolled back before the
// error is returned, so the tool surface is all-or-nothing.
func RegisterTools(fs hx.FileSystem, tr hx.ToolRuntime) (dispose func(), err error) {
	defs := []hx.ToolDefinition{
		readFileTool(fs),
		writeFileTool(fs),
		editFileTool(fs),
	}
	disposers := make([]func(), 0, len(defs))
	revert := func() {
		for i := len(disposers) - 1; i >= 0; i-- {
			disposers[i]()
		}
	}
	for _, def := range defs {
		d, err := tr.Register(def)
		if err != nil {
			revert()
			return nil, fmt.Errorf("fs: register %q: %w", def.Schema.Name, err)
		}
		if d != nil {
			disposers = append(disposers, d)
		}
	}
	return revert, nil
}

// okResult marshals v into a successful (model-visible JSON) ToolResult.
func okResult(v map[string]any) hx.ToolResult {
	b, err := json.Marshal(v)
	if err != nil {
		return toolErr("fs: encode result: %v", err)
	}
	return hx.ToolResult{Value: b}
}

// toolErr builds a ToolResult{IsError:true} whose Value is a JSON object carrying
// the message, so a downstream reader always gets valid JSON — never a panic.
func toolErr(format string, a ...any) hx.ToolResult {
	msg := fmt.Sprintf(format, a...)
	b, _ := json.Marshal(struct {
		Error string `json:"error"`
	}{Error: msg})
	return hx.ToolResult{IsError: true, Value: b}
}

// truncateUTF8 returns s bounded to at most max bytes and whether it was cut. The
// cut backs off to a rune boundary so the returned string is never split mid-rune.
func truncateUTF8(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	t := s[:max]
	for len(t) > 0 && !utf8.ValidString(t) {
		t = t[:len(t)-1]
	}
	return t, true
}
