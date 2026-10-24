# Forge native harness improvement pilot — v0.1

**Date:** 24 October 2026  
**Status:** Design baseline. An opt-in implementation and its verification procedure are described in the [pilot runbook](forge-harness-improvement-pilot-runbook-v0.1.md). No performance gain is established by this plan.  
**Source inspected:** `structor-forge` at `bbc29576a250f0fd139d65a1b924749edb102566`, with existing unrelated working-tree changes.  
**Decision:** Implement a restricted Harness2 improvement loop in Go around Forge's existing native executor. Start with private experimental runs, behavioral checks and reviewed reuse of guidance.

## 1. Outcome and scope

The pilot answers: **Does adapting Forge's execution guidance improve completion of unseen coding tasks enough to justify its extra cost, compared with spending the same budget on ordinary attempts?**

Use Harness2's two connected mechanisms:

1. Within a task, observe a baseline, propose one general and one task-specific harness change, probe both, compose a successor and execute it.
2. After the task, retain the adopted configuration and supported lessons to inform subsequent proposals.

Implement only `k=1`. Each complete improvement trial has four executor runs, three editor sessions (two proposals and one composition), and at most one reflection session. Each executor starts from the same original code and task inputs; composition combines harness changes, not candidate code patches. The composed run produces its own code. This follows the structure of the [pinned upstream recursion implementation](https://github.com/google-research/harness2/blob/1cfe254e8d25b15f37048c3730134686abe60335/harness2/core/recursion.py).

The first editable surface is additive workflow guidance, tool-use guidance and Markdown skills. Keep model/provider, hard rules, tool implementations, permissions, checker commands, dependencies and execution budgets fixed. Scripts, executable plugins, additional tools, subagent orchestration and deeper recursion are later work. Changes to standing rules cannot be smuggled into an editable prompt section: validate protected content and enforce authority in code.

Adapt the method in Go, with the upstream revision and design recorded in each experiment manifest. Ship no Python/OpenCode runtime dependency in the desktop pilot. If prompts or source are copied, preserve the upstream Apache-2.0 notice and attribution. A Python Forge substrate adapter for running upstream benchmarks remains a separate optional integration; [the upstream substrate interface](https://github.com/google-research/harness2/blob/1cfe254e8d25b15f37048c3730134686abe60335/harness2/substrate/base.py) is a useful compatibility reference.

## 2. Reuse map and current gaps

Paths in this design are relative to the Forge repository root. The reuse map records the pre-implementation baseline; consult the runbook for implemented entry points.

| Existing component | Reuse | Required change or boundary |
| --- | --- | --- |
| `engine/internal/uiservice/native.go`, `native_cache.go` | Native composition, cancellation/join, provider caching, transcript retention and usage collection | Extract a shared execution seam so the pilot and desktop run the same implementation. Preserve the existing desktop behavior and transcript schema. |
| `engine/internal/harness/spine/spine.go` | `Options.Sections`, `SkillRoots`, compaction, self-correction, tools and agent factory | Materialize a validated immutable profile before `Build`; keep profile changes outside the core tool loop. |
| `engine/internal/harness/preset/` and `app/` | Existing composition concepts | Use a fresh registry and workspace for each pilot execution. Scoped overlays are views, not authority confinement; generated executable extensions are outside pilot scope. |
| `engine/internal/harness/session/`, `record/` | Retained session events, content addressing and artifact digests | Add experiment/profile/stage references without embedding credentials or exposing unrestricted CAS reads. |
| `engine/internal/skillopt/` | Failure harvesting, edit application and version identifiers | Reuse helpers when they fit. The existing skill-only optimizer is not a task replay/comparison engine. |
| `engine/internal/harness/selfimprove/` | Failure-trigger and single-flight patterns | Leave its automatic promotion path disabled in pilot runs. The new orchestrator owns all improvement work and budgets. |
| `engine/internal/harness/skilleval/`, `learning/acceptance.go` | Declarative checks and candidate history | Existing stored checks use substring presence/absence in skill text. `Accept` requires all supplied cases to pass but does not measure improvement against a running baseline. Neither establishes behavioral superiority. |
| `engine/internal/feature/fixed_command_*`, `workspace_fixed_check*`, `uiservice/run_workspace_check*` | Fixed command digests, confined checks, exact target binding, durable execution intent and receipts | Extract only the generic runner mechanics needed by local evaluation. Preserve genuine shared-workspace authority chains; never fabricate company identities or permissions for fixtures. |
| `engine/internal/uiservice/agent_documents.go`, `agent_document_tools.go` | Attributed skill proposals, exact inspected target, conflict checks and human publication | Submit reusable results through the real active-run proposal path. An experiment record alone does not authorize publication or impersonation of an agent run. |
| `engine/internal/harness/llm/usage.go` | Actual usage and rate-based cost estimates, including cache accounting | Add reservation/admission checks for all calls, including editors, reflection and compaction. Unknown usage remains unknown. |

The inspected desktop builder enables compaction, ranked context, skills and self-correction; it does **not** supply the spine's optional `Learning` or `SelfImprove`. Enabling those options alone would not implement this plan. Shared guidance also requires explicit human publication today; see the [guidance checkpoint](ui/51-agent-authored-guidance-guide-checkpoint-2026-10-07.md).

## 3. Implementation shape

Keep the existing Go component architecture and introduce three small packages plus a pilot command:

| Proposed path | Responsibility |
| --- | --- |
| `engine/internal/harness/native/` | Shared single-execution driver extracted from the native builder. Accept an already authorized execution request, adapter, captured inputs and profile; return the final text, retained transcript references, usage and terminal state. No task admission or company authority decisions. |
| `engine/internal/harness/improve/` | Profile validation, the bounded Propose–Probe–Compose state machine, public contrasts, experimental lesson library and budget admission. Consume small runner/editor/recorder interfaces for deterministic tests. |
| `engine/internal/harnesseval/` | Fixture manifests, source materialization, external behavioral graders, trial scheduling and comparative reports. Private grader inputs never enter `improve` or the native driver. |
| `engine/cmd/forge-harness-pilot/` | Explicit `validate`, `run`, `resume` and `report` operations using a dedicated experiment state root and configuration file. Initial entry point; no desktop UI is required for the first measurement. |

`uiservice` remains the owner of task/run identity and execution authority. Its existing builder delegates the execution body to `harness/native`. Keep the import direction `uiservice -> native`, `harnesseval -> native/improve`; the harness packages must not import `uiservice`.

Preserve `native_cache.go`'s prompt prefix ordering, cache scope, redaction and accounting behavior during extraction. Keep existing transcript JSON fields compatible; use optional additions or a separate experiment envelope. Regression-test ordinary native runs before introducing candidate profiles. The pilot command must not become an alternative way to invoke shared task tools without their original authorization.

### Immutable records

Define versioned records with canonical serialization and content digests:

- **Profile:** base profile digest; allowed prompt sections; exact skills and their content digests; tool/policy fingerprints; source guidance revisions. Protect the base separately from model-editable fields. Bound added guidance to 8 KiB and four skills of at most 8 KiB each; reject unknown fields, unsafe paths, symlinks and executable attachments. Account for added prompt tokens in every arm.
- **Experiment:** dataset/split digest, Forge revision and dirty source manifest, upstream pin, executor/editor model identifiers, provider/config fingerprint without secrets, image/toolchain digests, budgets, arm ordering, repeat identifiers and initial library digests.
- **Stage:** experiment/trial/task identity, parent stage, profile/input/library digests, execution intent, output code manifest, transcript references, public check receipts, usage and terminal reason.
- **Lesson:** scope and applicable conditions; adopted profile; observed contrast; supporting stage/receipt digests; known failure conditions and contradictions. Label it an experimental hypothesis until independently evaluated/reviewed.
- **Evaluation:** sealed final output digest, private checker version, original/public test integrity, requirement-level outcomes, timeout/infrastructure classification and audit references.

Store large bytes once in the CAS. For the CLI, retain a small append-only chain of stage records and a rebuildable index under the dedicated experiment root. Existing desktop databases and private operation journals remain durable application state. Persist intent before a paid execution and retain interrupted/uncertain attempts; a missing reply never authorizes silently repeating a billed call.

## 4. Task-level procedure

1. **Admit:** validate the experiment, immutable base profile, source/context snapshot, confinement and budgets. Pin published guidance, visible tests, dependency cache and library snapshot. Create a fresh workspace and registry for each execution.
2. **Reference:** run unchanged Forge once and save its code, transcript, public checks and usage. Tool errors and loops are observations, not automatic task failure verdicts.
3. **Propose:** request two validated profile patches from the editor. The general proposal targets planning/tool-use weaknesses; the task-specific proposal targets the current workflow. Both see the same reference and approved earlier lessons. They can edit only the declared guidance fields.
4. **Probe:** execute both profiles independently from the original source. Run the same visible checks on each output. No candidate receives another candidate's modified workspace or session memory.
5. **Compose:** supply the reference, both profile diffs, public trajectories/checks and output artifacts to the editor. Require each retained edit to cite an observed contrast. It may preserve the base. Validate the composed profile and execute it from the original source as the fourth run.
6. **Seal:** retain the composed output as the primary improvement-arm deliverable. Do not select the best candidate using private grades. If proposal/composition fails validation or execution cannot finish within budget, record a fallback and return the reference artifact when available. Retain all costs; no extra model repair session is allowed in this pilot.
7. **Reflect:** after sealing, spend at most one session extracting applicable lessons from public contrasts. Record no lesson when support is missing. Private evaluation results remain inaccessible.

For each later task, start again from the common base. Retrieve at most three earlier relevant lessons for proposals; do not silently replace the base with the previous task's profile. Scope experimental libraries by experiment/repeat, project, task family, base digest and executor model/configuration. Keep task order fixed within a family; different families may run concurrently within the global cap. Repeats use separate library branches, initialized from the same frozen development library. No future-task material is available early.

This gives both levels of recursion while keeping experimental lessons separate from shared, published skills. The first pilot measures their combined effect. It cannot isolate the causal contribution of the library; a later frozen-library versus growing-library ablation needs fresh tasks.

## 5. Behavioral evaluation contract

The unit being evaluated is **the resulting program and its observable behavior**, not whether an instruction contains desired words and not whether an agent says it succeeded.

### Fixture set

Prepare 24 small but realistic, independently checkable repository tasks: six in each family below. Use eight for fixture development/calibration (two per family) and seal 16 for prospective evaluation (four per family). Split by originating incident/template, excluding near-duplicate fixes across splits. Record provenance and limitations; this is an exploratory coding pilot, not a representative product benchmark.

| Family | Example task | Behavioral checks |
| --- | --- | --- |
| Bug repair | Fix duplicate processing or a retry edge case | Reproduce the original defect; test unseen boundary cases and existing behavior. |
| Contract change | Extend an input/output contract while preserving old clients | Exercise old/new payloads, error cases and serialization through actual calls. |
| Multi-file refactor | Move shared logic without changing behavior | Build dependents; run integration/property cases that cross the changed modules. |
| Tool/recovery work | Repair a build/configuration failure with misleading intermediate output | Confirm the final checkout builds/runs, captures the requested artifact and handles the original failure; check protected files remain intact. |

Each task includes an immutable source snapshot, user-visible request, visible checks, trusted private acceptance/regression cases, a known-good patch and deliberately bad mutations. Before any paid run, establish that the original source fails the intended requirement, the known-good patch passes all required checks, and plausible bad patches are caught. Include test deletion, constant-return implementations and a fix that breaks an old client among grader mutations.

### Grader isolation

Run the evaluator against the sealed output digest in a separate disposable environment. Supply trusted tests/check commands from outside candidate-controlled source, and verify their digests. Candidate-authored tests can add information but cannot replace those tests. Preserve and assess the original visible test suite. Changes to protected checks, policy or dependencies produce a failed task, not an easier task.

The solver/editor/reflection environments receive no private tests, expected answers, grading logs, checker mounts or credentials. Disjoint directories under the same unrestricted user do not establish this boundary. Use a disposable container/VM with explicit mounts, a verified confinement mechanism and no host state or container socket; run grading in another environment. Provider calls are mediated by the host/proxy; shell tools do not receive provider secrets. Preload dependencies and restrict network access. Existing `SandboxProvider.Available()` can report no confinement: the pilot must then refuse execution, and must exercise attempted grader/host-file access in preflight tests.

Only public checks and task outputs may guide proposals, composition, retry selection and experimental lessons. Release private results to reporting after the full evaluation stream and all deliverables are sealed. This prevents a later task from learning hidden feedback through an earlier reflection. The upstream [experiment guide](https://github.com/google-research/harness2/blob/1cfe254e8d25b15f37048c3730134686abe60335/docs/EXPERIMENTS.md) documents why unrestricted access to public benchmark grading data can invalidate a comparison; this pilot uses private cases and verifies their isolation.

### Outcome and comparison

Primary success requires delivery of a usable code artifact, all required private acceptance cases, the trusted regression suite and integrity checks. Keep per-requirement outcomes for diagnosis. Do not count an LLM preference rating as a pass. Review qualitative plans/explanations blindly only as a secondary measure.

Run these three arms on every evaluation task, twice, with identical starting snapshots and pinned executor configuration:

| Arm | Allocation | Final output |
| --- | --- | --- |
| A: ordinary Forge | One unchanged native execution | That execution's artifact. |
| B: retry control | Unchanged harness; up to five fresh attempts within the same total input/output, dollar and elapsed-time caps as C | A predeclared selection rule using only visible checks: most passed public checks, then successful build, then earliest attempt. No private grades or reusable lessons. |
| C: improvement | Four executor runs, three editor sessions and up to one reflection, sharing one trial budget | Composed artifact or recorded reference fallback. |

The repeat identifier pins ordering, not provider randomness. Rotate arm order across tasks to reduce cache/load bias; record cache usage and actual model version when exposed. B may use saved editor budget for attempts, within its five-attempt ceiling. Equal ceilings do not imply equal actual cost: report actual spend and budget saturation for every arm, and do not label unequal bills as cost-matched.

Count solver timeout, invalid output, exhausted budget and tampering as unsuccessful task outcomes. A private grader infrastructure failure is **unscored**, remains visible and makes the paired trial incomplete; it is never dropped from a favorable denominator or replaced by a different task. A provider/transport retry consumes budget and is retained. Declare any rerun rule before evaluation and apply it to all arms; ambiguous already-dispatched paid calls are not automatically replayed.

Report pass rate, C−A and C−B paired differences, baseline successes lost, family-level outcomes, actual total/cost per successful task, tokens, auxiliary usage, latency, fallbacks and incomplete trials. Show paired confidence intervals clustered by task: two repeats on one task are not two independent tasks. Given only 16 held-out tasks, report uncertainty and authorize only a larger study if results are promising.

## 6. Explicit pilot ceilings

These are proposed operational defaults for a future run. Creating this plan performs no provider calls or spending.

| Limit | Default |
| --- | --- |
| Scope | One private project, native harness, coding fixtures only; experiment disabled in normal runs |
| Development | Eight calibration tasks, one C trial each |
| Evaluation | 16 sealed tasks × two repeats × A/B/C |
| Maximum executions | 32 calibration + 32 A + 160 B + 128 C = **352 executor runs** |
| Maximum editor/reflection sessions | 32 calibration + 128 evaluation = **160 sessions** |
| Concurrency | Two active model sessions globally; task order retained within each library stream |
| Executor | At most 24 steps, five minutes and 48,000 context tokens per execution; bound output tokens at dispatch |
| Editor/reflection | At most 12 steps and 90 seconds per session; text/profile outputs only |
| B/C trial budget | 500,000 total input tokens, 40,000 output tokens, US$5 estimated maximum and 30 minutes; includes all auxiliary calls |
| Entire pilot | 10 million input tokens, one million output tokens, US$250 estimated maximum and 24 active hours; first exhausted limit stops admission |

The count limits are maxima, not a promise that all trials fit the global allowance. Calibrate on the development split, then freeze feasible per-trial limits before opening evaluation. If measured costs predict an incomplete run, revise the plan/budget before evaluation rather than silently truncating hard tasks. If a cap nevertheless prevents completion, publish an incomplete report and make no graduation claim.

Use operator-supplied, recorded rates for both executor and editor models. Reserve the conservative upper bound for each provider request before dispatch, including compaction. Reconcile with observed usage; retain reservations for ambiguous/unknown usage. Reject a request whose bound would exceed a cap. A streaming response can incur charges before cancellation, so describe dollar limits as estimates, record overshoot and bound outstanding requests/output lengths. No missing rate or usage value becomes zero. If dollar accounting is unavailable, the costed pilot cannot start; fixture validation and deterministic tests remain available.

Cancellation stops the entire experiment tree and joins active processes/sessions. Use the experiment context, not detached `context.Background()` work. Resume verifies source/profile/model/checker digests and reuses completed stages. Interrupted execution becomes an explicit uncertain/aborted attempt; it requires a newly admitted attempt and reserved budget before any rerun. Budget and duration limits govern resources, not proof or promotion authority.

## 7. Delivery sequence and acceptance

Each row is an independently reviewable implementation change. Keep existing unrelated working-tree changes outside these changes.

| Change | Deliverable | Acceptance before proceeding |
| --- | --- | --- |
| 1. Fixtures and evaluation manifest | `harnesseval` fixture contracts, the 24-case inventory, development/private split, trusted commands and gold/bad patches | Every fixture validates without an LLM; intended defect reproduced; gold passes; bad mutations fail; hidden material cannot be read from the solver environment. |
| 2. Shared native execution and immutable profiles | Extract `harness/native`; adapt `uiservice/native.go`/`native_cache.go`; canonical profile validator and materializer | Existing native behavior, cancellation, redaction, context ordering, caching and full usage preserved. Concurrent runs do not share writable context/skills/workspaces. Ordinary runs stay unchanged by default. |
| 3. Independent behavioral runner | Sealed output capture; reuse generic fixed-command execution/confinement mechanics; private result schema | Check a copied exact code manifest; tampered/missing tests, unavailable confinement, timeout and grader failure produce the declared outcomes. No forged workspace/company authority. |
| 4. Bounded improvement state machine | Reference, two proposals/probes, composition, final execution; fake runner/editor support | Deterministic end-to-end test checks the four execution inputs are identical except allowed profile changes, final output comes from composition, invalid changes fall back, and private grading data never crosses the interface. |
| 5. Lessons, accounting and recovery | Scoped library, cited contrasts, immutable stage chain, shared call admission and resume | Repeat/library isolation, no future-task or hidden-score retrieval, concurrent budget reservations, auxiliary charges, cancellation/join, crash recovery and no automatic uncertain replay all covered. Reusable guidance is a proposal, never auto-published. |
| 6. Pilot CLI and comparative report | A/B/C scheduler, preflight, config/example, report and one development exercise | Smoke tests need no credentials; live preflight records exact configuration and estimates coverage/cost; freeze evaluation only after development gates pass. |

Start implementation with change 1. Agree the trusted behavioral contract before connecting an optimizer. Work on the driver extraction and grader can follow once the fixture contract is stable; no parallel agents are required by this plan.

For implementation validation, run focused tests for the new packages and changed native/cache/usage, fixed-command and document-proposal paths. Run relevant race checks for concurrent admission, stage retention and cancellation. If shared workspace/checker types change, include their existing validation and authority-recovery suites. Preserve resolver semantics; run conformance vectors if code touching that boundary changes. A documentation-only plan does not warrant rebuilding the desktop or running paid benchmarks.

## 8. Decision after the pilot

Freeze these exploratory gates before evaluation. They govern whether to fund a larger experiment, not whether an agent may publish guidance or ship code:

- Complete and retain all planned paired outcomes; no unresolved grader failures or missing accounting. Verify all structural/isolation invariants, with zero critical authority, integrity or hidden-feedback violations.
- C improves held-out success over B by at least 10 percentage points. With 32 task-repeat pairs, this requires at least four more successful paired outcomes net of losses. Also report task-clustered uncertainty; do not claim statistical confirmation from the threshold alone.
- At least three of four families are nonnegative versus B, and C loses no more than one A-successful task-repeat pair. Investigate every regression, including public-check false positives.
- C's actual cost per successful task is at most 1.25× B's, with measured latency acceptable for the proposed use case. If either arm has no successes or material unknown spend, this comparison is inconclusive.

If gates fail, retain the report and diagnose whether guidance edits, evaluation coverage or budget allocation caused it; leave normal execution unchanged. If gates pass, expand on fresh tasks and add a frozen/growing-library ablation before broad adoption. Optimize cost or trigger recursion only for difficult/failed tasks in a later experiment, since the first pilot measures a fixed allocation.

For reviewed reuse, present an experiment-backed suggestion through a currently authorized Forge run, using the existing Docs/Skills proposal path with evidence digests. Retain exact target version/digest and separate original agent contribution from human edits. A maintainer reviews and publishes; later assignments capture the published revision. An offline CLI report can be reviewed as an artifact but cannot manufacture an active-run origin.

Only after the larger study should the desktop add an explicit per-run improvement option, with an estimated budget, child stages, checks and fallback outcome. Shared/company execution requires genuine per-run and per-check permissions for every added attempt; an existing single-run launch is not blanket authority for probes. Rollback disables the option and restores the captured published guidance revision while preserving experiment history. Executable plugins, subagents and higher `k` each need their own measured extension proposal.

## 9. Plan completion versus implementation completion

This document specifies the integration and pilot; it is not execution evidence. The linked runbook describes the opt-in implementation. Implementation checks and live experiment reports must establish their own completion and effectiveness claims. Normal-run defaults and published guidance remain outside the pilot’s promotion authority.
