# structor-harness

The Forge native agent harness, extracted as a standalone, importable Go module.
Standard library only — no third-party dependencies.

```go
import (
	"github.com/websines/structor-harness/harness/native" // drive one agent execution
	"github.com/websines/structor-harness/harness/spine"  // compose the harness registry
)
```

## Layout

| Path | What it is |
|------|------------|
| `harness/` | The harness: `hx` (shared contracts), `spine` (composition), `native` (run driver), `llm`, `loop`, `agent`, `session`, `tools`, `fs`, `shell`, `subprocess`, `sandbox`, `mcp`, `spawn`, `compaction`, `context`, `memory`, `skill*`, `improve`, `selfimprove`, `evidence`, `extagent`, … |
| `harnesseval/` | Black-box evaluation and improvement-pilot runner |
| `cmd/forge-harness-pilot/` | Pilot CLI (`config.example.json` reads the API key from `FORGE_PILOT_API_KEY`) |
| `runtime/` | Component registry (everything resolved by service key) |
| `flg/`, `record/`, `resolver/`, `reason/`, `reasongate/`, `obligation/`, `query/`, `learning/`, `skillopt/`, `sandbox/floor/` | Support packages the harness depends on |
| `conformance/` | Resolver conformance corpus used by `resolver` and `reasongate` tests |
| `docs/` | Harness design doc and improvement-pilot docs |

## Develop

```sh
go build ./...
go test ./...
go generate ./resolver/   # regenerates ruletable_gen.go from conformance/canonical/
```

## Provenance

Snapshot of `engine/internal/harness/**` and its full transitive closure from
`websines/structor-main` at `bbc29576` plus that checkout's uncommitted working
tree (2026-10-24). Changes made during extraction:

- module path `github.com/structor/forge/engine` → `github.com/websines/structor-harness`
- `internal/` prefix dropped so other modules can import the packages
  (`internal/sandbox/floor` → `sandbox/floor`, `internal/harness/x` → `harness/x`)
- conformance-corpus test paths and the `canongen` output path adjusted for the new layout

Code comments still refer to the original `internal/...` paths.
