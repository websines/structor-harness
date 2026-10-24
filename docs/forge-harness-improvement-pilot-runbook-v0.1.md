# Native harness improvement pilot — runbook v0.1

24 October 2026 · Experimental developer interface. Normal desktop runs continue to use their ordinary single-execution behavior.

This implements the bounded method in the [integration plan](forge-harness-improvement-pilot-v0.1.md): a reference execution, two independently proposed/probed guidance profiles, a composed execution, then evidence-linked reflection. Profiles can change workflow/tool guidance and Markdown skills. The shared native driver, instructions, file tools, context/cache ordering, compaction and self-correction also serve the desktop. The pilot does not load generated executable plugins or change shared guidance automatically.

## Build and validate

From `engine/`, using the repository's Go toolchain:

```sh
go build -o /absolute/private/forge-harness-pilot ./cmd/forge-harness-pilot
/absolute/private/forge-harness-pilot validate -state /absolute/private/pilot-development
```

Use a new dedicated directory, with mode `0700`. Keep the binary outside OS runtime directories on Linux: the executable embeds the private fixture catalog. Validation makes no model calls. It checks all 24 original/gold/negative cases and saves a receipt for the exact fixture catalog, checker and executable digest. Rebuilding requires a fresh validation receipt. A missing or mismatched receipt prevents model execution. Negative cases include a hard-coded answer, a legacy-client regression and protected-test tampering.

On macOS, the pilot uses a stricter read-restricted Seatbelt profile than the ordinary desktop sandbox, verifies a successful command and a denied private-file canary, and denies reading the fixture-bearing executable. On Linux it requires the available confinement provider and verifies the same canaries. Unsupported or ineffective confinement refuses execution. Shell environments allow only the declared runtime/cache settings and omit provider credentials. This is a local pilot boundary, not a claim that every OS configuration has received a production security audit.

The `blackbox-v1` private checker first verifies the protected visible suite. It then compiles candidate code with input expressions only, removes the compilation workspace, and runs the resulting executable in a separate directory containing only that executable. Each query has a fixed ten-second limit, allowing for native process startup; the whole grading operation is separately bounded. Expected answers remain in the trusted host process, which compares returned values. Private test sources, expected-answer tables and grader diagnostics never enter the candidate runtime. The validation receipt binds this checker version and executable; older builds' receipts cannot admit a run.

Preparation builds a process-owned cache from named Go standard-library packages only. Each workspace receives an independent copy (copy-on-write where supported), with no shared writable cache. Candidate source and private tests never enter the template. Runtime home data, including Go's local telemetry, stays under the excluded `.forge` directory. This reduces repeated compiler work while keeping code and grader data isolated.

## Configure and run calibration

Copy `engine/cmd/forge-harness-pilot/config.example.json` into a private configuration file. Supply the provider key through the named process environment variable. Keep key values out of the JSON, repository and report. The model and endpoint must match the intended native provider.

The example uses GPT-6 Luna and the [published Standard short-context rates](https://developers.openai.com/api/docs/models/gpt-6-luna), checked on 24 October 2026. Set rates for the account's actual tier/context. `rates.input` and `rates.output` are conservative admission bounds; `rates.actual` supplies the existing cache-aware estimator. If the provider omits billing details, detailed cost stays unknown. The example reserves higher rates for admission and uses a US$5 whole-experiment estimate cap, below the plan's US$250 ceiling. Dollar caps are estimates, backed by token/request/output limits; they are not provider invoice controls.

```sh
/absolute/private/forge-harness-pilot run \
  -state /absolute/private/pilot-development \
  -config /absolute/private/development.json
```

Development runs the eight calibration tasks with arm C only. Every native execution receives the original source in a new workspace and a fresh registry. Candidate code is not merged into the final run. The editor sees bounded public trajectories and outputs, with explicit truncation markers and original CAS references; it never receives private tests or scores. Private grading begins after the whole stream's deliverables are sealed.

## Freeze and run evaluation

After calibration, preserve the executable and configuration. Correct implementation or fixture defects before opening held-out evaluation, then create a new calibration experiment if those changes affect its recorded manifest. Do not tune against private evaluation outcomes.

Create an evaluation configuration with `split: "evaluation"`, `repeats: 2`, and `seedExperiment` set to the absolute completed development-state directory. Use the same project, model, endpoint, native step/output/time limits and executable. The seed loader checks those bindings and copies only supported experimental lessons; private grades are not passed to the next task. Each repeat gets a separate library branch, and tasks remain ordered within their family.

Allocate the remaining whole-pilot budget after subtracting calibration's retained spending and active time. The calibration and evaluation directories are distinct experiments; their configured limits are not an automatic shared account quota. Their combined ceiling must remain within the declared pilot envelope.

Validate the evaluation directory, then run it with its frozen configuration. The 16 held-out tasks run twice under each arm:

| Arm | Behavior |
| --- | --- |
| A | One ordinary native execution. |
| B | Up to five ordinary attempts within the same trial caps as C, selected using protected visible checks, build success and original order. |
| C | One bounded improvement step, at most four native executions and four editor/reflection sessions. |

Arm order rotates by task. The ceilings are equal for B/C; actual expenditure can differ and is reported. Execution is currently sequential, staying below the global two-session concurrency ceiling. Global and per-trial reservations include compaction and editor calls. A failed admission stops that budget from dispatching more calls.

## Inspect and resume

```sh
/absolute/private/forge-harness-pilot report -state /absolute/private/pilot-development
/absolute/private/forge-harness-pilot resume \
  -state /absolute/private/pilot-development \
  -config /absolute/private/development.json
```

An OS process lock prevents concurrent owners. The manifest binds the dataset, executable digest, build/runtime information and configuration. A completed report is returned without calling the model again. Resume reuses retained completed trials and their spending. An intent without its terminal trial record is uncertain: the command does not silently repeat it, retains a conservative maximum reservation and reports an incomplete experiment. Do not erase state to conceal or retry an uncertain operation.

The private state contains content-addressed code snapshots, full transcripts and grader diagnostics, immutable stage references, selections, lessons and the report. Each private check identifies the output artifact, trusted cases, checker, Go version and confinement version. Report diagnostics are bounded; the full diagnostic bytes remain in CAS.

## Interpret the report

`complete` means every planned trial has a scored outcome. Solver timeout, budget failure and missing output are failed outcomes. Provider/grader infrastructure problems are unscored and make the experiment incomplete. Protected-input tampering is a failed integrity check. The report counts every trial with an observed integrity violation, including a rejected probe followed by a safe fallback; such a fallback cannot restore expansion eligibility.

The report includes actual token counts, conservative dollar bounds, detailed cache-aware cost estimates when known, resolved provider model identifiers when supplied, latency, fallbacks and paired results. Missing comparisons do not become wins. Confidence intervals bootstrap whole tasks, preserving repeated observations in their task cluster.

`expansionEligible` applies the plan's exploratory gates: a complete two-repeat evaluation, at least four net C wins over B, limited A regressions, nonnegative results in at least three families, zero integrity violations, complete cost information and acceptable measured cost per success. It authorizes no publication or code shipping. A small authored fixture study cannot establish broad production effectiveness; a frozen/growing-library ablation needs fresh tasks.

Lessons remain private experimental hypotheses. To reuse one in ordinary work, review the report and present the proposed guidance through a currently authorized Forge task's existing Docs/Skills proposal tools. A maintainer reviews and publishes an immutable revision. The pilot CLI cannot publish, alter defaults or manufacture a company run identity.

## Developer verification

```sh
go test ./internal/harness/...
go test -race ./internal/harness/improve ./internal/harnesseval
go test -tags=integration -race ./internal/harnesseval \
  -run 'TestNativeImprovementEndToEnd|TestPairedSchedulerEndToEnd|TestCrashResumeDoesNotReplay' -count=1
go test ./internal/uiservice -run 'TestNative|TestDesktopNative' -count=1
```

The end-to-end protocol fixtures use deterministic model adapters with the real native loop, file tools, sealed snapshots and confined Go checks. The paired scheduler test exercises A/B/C over two repeats, earliest-retry tie selection, library isolation, accounting and completed-run resume. These verify execution mechanics, not model improvement. Use the live command-line calibration/evaluation reports for model-backed outcomes. `FORGE_VALIDATE_ALL_FIXTURES=1 go test ./internal/harnesseval -run '^TestBehavioralFixtures$'` performs the full fixture contract check directly; the command-line `validate` path additionally retains its prerequisite receipt. The [implementation checkpoint](forge-harness-improvement-pilot-checkpoint-2026-10-24.md) distinguishes completed calibration from the prospective effectiveness study.
