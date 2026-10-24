// Package runtime is the Component Runtime (system design §4.15): the lifecycle
// discipline by which every pluggable part of the engine — evidence collectors,
// harness adapters, surface integrations, sandbox drivers, queue consumers,
// posture policies — loads, reconfigures, and unloads. It is a pattern-and-
// semantics port of cordis's spatiotemporal composability (§19.7): the mechanic
// set, ported into idiomatic Go and owned by Forge, NOT a TypeScript dependency.
//
// The section's thesis is that the engine should obey the same laws as its own
// record. This package realizes the runtime half of that mapping:
//
//	The record's law                      The runtime's law (this package)
//	--------------------------------------------------------------------------
//	Supersession — every fact undoes  →   Revertible effect — every registration
//	  itself                                carries its tracked inverse; unload
//	                                        replays inverses in reverse (LIFO).
//	Auto-reopen, never auto-close     →   Reactive coeffects — a component stops
//	                                        the moment a declared dependency is
//	                                        withdrawn; only a fresh activation
//	                                        brings it back.
//	No recency, no fiat               →   Confluence — the quiescent composition
//	                                        is a function of the declared config
//	                                        alone, not of load order.
//	Contested never governs           →   A failed component reverts fully,
//	                                        records its error, and is withheld —
//	                                        never half-alive, never crash-looping.
//	Evidence declares its scope       →   Confinement — a component writes only
//	                                        the keys it declares to provide and
//	                                        reads only the keys it declares to
//	                                        need.
//
// The moving parts, in the order a reader meets them:
//
//   - Effect / disposerStack (effect.go) — a revertible effect installs
//     something and returns its inverse; a fiber's disposers revert in LIFO
//     order, exactly once (mechanic 1, mechanic 5 partial rollback).
//   - Component / Context (component.go) — a component is (inject, provide,
//     apply); the Context is its confined capability handle (mechanic 9).
//   - Fiber (fiber.go) — an instantiation with a four-state lifecycle, a parent
//     (fibers form a tree), a committed view, and its own effect table.
//   - Registry (registry.go) — the orchestrator. It only inserts (Load) and
//     retires (Remove); every lifecycle transition is DRIVEN by the target view
//     disagreeing with the committed view (mechanic 3), never set by hand.
//
// Deliberately simplified against §4.15: isolation realms (mechanic 7),
// interception (mechanic 8), the declarative loader with config write-back
// (mechanic 11), and the emission/acquisition boundary (mechanic 12) are named
// in the design but out of scope for this runtime core; the two laws — revertible
// effects and reactive coeffects — and the fiber lifecycle are ported in full.
//
// Formalization note: the accumulator-recovery invariant (a fiber's LIFO revert
// returns the observable service table to its pre-activation state) and the
// guarded-withdrawal invariant (a provider's bindings stay readable until every
// dependent that resolved to it has deactivated, and the withdrawal terminates
// without deadlock) are the candidate FIFTH Lean module,
// proofs/ForgeProofs/Runtime.lean (currently stubbed; §20). The Go code here is
// their operational witness, gated later by the Lean conformance the design
// prescribes for first-party components.
package runtime
