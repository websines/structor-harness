# Native harness improvement pilot — implementation checkpoint

24 October 2026. The opt-in implementation has completed live development calibration. **No improvement over ordinary Forge or a retry control has been established.** The prospective 16-task, two-repeat comparison is configured but has not been run.

The [design baseline](forge-harness-improvement-pilot-v0.1.md) specifies the method and expansion gates. The [operator runbook](forge-harness-improvement-pilot-runbook-v0.1.md) describes the command and retained state.

## Implemented behavior

The desktop and pilot share `harness/native`, preserving the native instructions, cache ordering, transcript handling and existing usage observation. `harness/improve` adds validated guidance profiles, reference/proposal/probe/composition/reflection stages, experimental lessons, immutable records and admission budgets. `harnesseval` supplies sealed source snapshots, independent execution environments, a private behavioral checker and paired A/B/C reports. The command supports validation, run, resume and report operations.

Every executor starts from the original source. The final improvement artifact comes from a fresh composed execution, or a retained reference fallback. Editors receive public evidence only. Expected answers stay in the trusted host process; the candidate runtime contains an input-only compiled program. Private grading occurs after all stream selections are sealed. Lessons cannot publish shared guidance or change ordinary-run defaults.

## Verification

| Check | Observed result |
| --- | --- |
| All native harness package tests | Passed. |
| Full `uiservice` and `forge-app` tests | Passed; macOS linker deployment-target warnings remained nonfatal. Packaged desktop installation was not tested. |
| Improvement package race checks | Passed, including concurrent admission, source identity, invalid profiles, and durable reference fallback after resource exhaustion. |
| Native improvement integration | Passed with real file tools, confined Go execution, composed output selection and evidence-linked reflection. The adapter is deterministic; this verifies mechanics, not model capability. |
| Full integration/race suite | Passed; final full run took 325.935 seconds. |
| Crash/resume integration | Passed with a real helper process and a request confirmed as dispatched. Resume retained uncertain spending and made no replacement model call. |
| Private-answer isolation | Passed: expected answers were absent from the candidate executable, and its execution directory contained only that executable. |
| Runtime-file capture regression | Passed after reproducing Go local telemetry creating binary files under `HOME`; runtime home now resides under excluded `.forge` state. |
| Protected-input reporting | Passed: test edits, deletion of an empty dependency file, and a rejected probe followed by a safe final artifact remain integrity violations. |
| Fixture contracts | All 24 original/gold/two-mutation contracts passed: 96 checks on the frozen executable. |
| Real completed-run resume and report | With the provider key empty, both returned bytes identical to the live report, SHA-256 `a9e41e27b4d2475832e8c64a80aecee85ff1891f5e521de7c2f86e6d7b1f097d`. |
| Paired A/B/C scheduler integration | Passed with the race detector: two repeats, six trial records, 20 native executions, 68 deterministic model calls, paired grading and completed-run resume; 413.663 seconds. |
| Go static checks and formatting | Passed for changed harness/evaluation/command packages. |

The scheduler check uses one controlled fixture, two repeats, a deterministic adapter, real native execution and independent grading. Its synthetic admission receipt is a lifecycle-test fixture, not benchmark-validation evidence. It checks all three arm allocations, public-only retry selection, repeat isolation, usage, paired reports and completed-run idempotence. Its intentionally different A/B/C results must never be presented as measured model gains.

## Live development result

`calibration-07` used the configured native provider, GPT-6 Luna, for eight development tasks, with arm C only.

| Measure | Result |
| --- | --- |
| Completion | Eight of eight trials scored; report `complete: true`. |
| Private acceptance | **7/8 passed.** |
| Native executions | 32; no reference fallbacks. |
| Integrity | One general probe edited `public_test.go` and was rejected. Its final composed artifact preserved the test. The violation remains counted. |
| Usage | 214 model calls, including editor/reflection calls; 407,968 input and 20,340 output tokens; zero unknown-usage calls. |
| Detailed cache-aware estimate | US$0.040936515, using the recorded rates; not an invoice. |
| Conservative admission-rate estimate | US$0.117247. |
| Summed trial time | Approximately 45 minutes, excluding one-time preparation and final private grading. |

The `shared-fee` final artifact passed its public test but failed private acceptance/regression cases: one returned `95` where `107` was required, and negative cases returned `-2` where `0` was required. This is a concrete public-check false positive caught by the independent grader. The final artifact's test integrity passed; its behavior did not.

This result verifies that the model-backed loop executes, retains evidence and exposes failures. A single-arm development run provides no C−A or C−B effectiveness estimate. Normal Forge execution and published guidance remain unchanged.

## Retained engineering history

The first live calibration (`calibration-04`) was stopped after four repeated source-capture failures. Go local telemetry, rather than candidate source, caused those rejections. Its report and transcripts remain retained. A second engineering calibration (`calibration-05`) completed two full improvement trials and began a third before being stopped to freeze the integrity-report correction. Its private grading was canceled; those public trajectories are not private performance results.

Together these earlier runs used 15 native executions, ten editor sessions and 87 model calls: 127,522 input and 7,360 output tokens, with a conservative US$0.0374005 estimate. Including the completed development run, retained live usage is 301 model calls, with a conservative US$0.1546475 estimate and a detailed cache-aware estimate of US$0.05449058. All resources are deducted from the same US$5 estimated envelope.

An intermediate validation caught a known-good case failing under a two-second program-query allowance. The fixed limit is now ten seconds for every arm, and both the affected fixture and full catalog passed. A proposed same-volume temporary-cache placement later failed a known-good stock-reservation check before any evaluation model calls. It was rejected. The original host temporary-directory setting remains active; the affected fixture passed its targeted recheck there. The failed check and environment decision remain retained.

## Frozen artifacts

Local private evidence is under `.local/harness-pilot/`, excluded from version control. Provider credentials are not stored in configuration, source bundles or reports. The runtime source bundle contains 49 task-relevant source/dependency files. The scheduler integration test was added separately after the runtime freeze; it does not change the executable. Its source is retained in `verification-07.zip`. Final checks found no changes to the frozen source files, executable or six pinned runtime binaries.

- Baseline checkout: `bbc29576a250f0fd139d65a1b924749edb102566`, with unrelated existing work preserved.
- Source bundle: `implementation-07.zip`, SHA-256 `dc0de40994960fab18cd59c0c97a26db40e62ed9bf7917e96912b3458819023c`.
- Executable SHA-256: `310bc31bbba9973edc490cfc661bfa444457d843405f2b6640e3661ed4b673fa`.
- Fixture catalog SHA-256: `4e00edaf3e90d415a778d9c2e6ad518a4fe483ceacb3fa5ecf8aa45963479747`.
- Runtime: Go 1.27.0, macOS/arm64; operator fingerprints retain the compiler, linker, assembler, Go driver and confinement utilities.
- Upstream method reference: Harness2 `1cfe254e8d25b15f37048c3730134686abe60335`; implementation is in Go, with no copied upstream runtime or prompts.
- Private development report: `.local/harness-pilot/development-07-report.json`; prerequisite receipt: `validation-07.json`.

Validation binds the catalog, checker and executable digest. The evaluation directory retains the exact successful receipt with an explicit origin record. Source changes require a new validated executable before paid dispatch.

## Next measurement and limits

The frozen full comparison is 16 held-out tasks × two repeats × A/B/C. It must use the same executable and native configuration, separate repeat libraries, original source snapshots and no private feedback during improvement. The research schedule is bounded at 352 native executions and 160 editor/reflection sessions; the 15 earlier engineering executions and ten editor sessions give aggregate maxima of 367 and 170 within the same token, dollar and active-time envelope. The prepared launcher deducts all retained spending before admission.

At the completed calibration's average, the comparison has a rough conservative forecast of 7.5 hours and US$1.17 at admission rates. This is a feasibility estimate, not a measured comparison or guarantee. Whole-task confidence intervals, regressions, integrity violations, fallbacks, latency and measured cost must accompany its pass rates. Unknown spending or incomplete trials block expansion eligibility. No report authorizes publication or a normal-run default change.

These small authored Go fixtures are an exploratory coding pilot. The recovery family exercises configuration/error-handling functions; it is not a representative production outage benchmark. The study cannot establish broad production performance, isolate the library's causal contribution, or constitute a production OS security audit. A larger fresh-task study and human review remain necessary before shared guidance or desktop behavior changes.
