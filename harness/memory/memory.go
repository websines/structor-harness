// Package memory folds the harness SKILL SYSTEM and FAILURE SIGNALS into the
// learning store, completing "everything is memory": a discovered skill and a
// tool failure stop being ephemeral, harness-local state and become record-backed
// FLG facts — acceptance-gated, recallable, and cross-session.
//
// The learning store (internal/learning) is governed team memory over the
// append-only flg record (Law 1: no memory asset exists that is not
// reconstructable from the canonical record). This package writes into that same
// record:
//
//   - a discovered skill is folded in as a learning CANDIDATE (a learning.Skill:
//     Title=Description, Body=Body, Provenance=[Source]). It is acceptance-gated —
//     it recalls as a candidate until a promotion fact exists (§11.1) — so a skill
//     is no longer merely a file on disk but a governed asset with provenance.
//
//   - a tool failure is folded in as a learning FAILURE fact. correct.FailureMemory
//     is in-memory-per-agent and forgets across compaction/resume; this failure
//     store lives in the record, so it survives the process and is recalled by
//     signature on any later session.
//
// Because the learning store shares the SAME flg.Store the record/context is built
// over, once skills and failures are recorded they are facts the ranked, cited
// verified-context block (H4b) can surface — skills as candidates, failures as
// facts, both selected by relevance without this package ever touching the prompt.
//
// Everything here is deterministic: identities are content-derived (sha256 of the
// failing call), recall order comes from the record's projection, and there is no
// clock. Standard library only.
package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/harness/skill"
	"github.com/websines/structor-harness/learning"
	"github.com/websines/structor-harness/runtime"
)

// maxReasonChars head-bounds any error excerpt folded into a failure fact, so an
// error blob cannot ride unbounded into the record (correct's capped-preview idea).
const maxReasonChars = 500

// RecordSkills folds discovered skills into the learning store as
// acceptance-gated CANDIDATES: each skill.Skill becomes a learning.Skill with
// Title=Description, Body=Body and Provenance=[Source], keyed by a stable id
// derived from its name. It is idempotent-ish — a skill whose id is already a
// (non-retired) candidate in the store is skipped, so re-folding the same
// discovery adds nothing. Returns the ids NEWLY recorded, sorted for a
// deterministic result independent of input order. A skill with an empty name is
// ignored; an AddCandidate error skips just that skill (the signature carries no
// error, and one bad skill must not sink the batch).
func RecordSkills(ls *learning.Store, skills []skill.Skill) []string {
	// Existing candidates (and promoted skills) already in the record — the dedup
	// set. Recall omits retired skills, matching "idempotent-ish": a retired skill
	// with the same name may legitimately be re-folded.
	existing := make(map[string]bool)
	for _, sk := range ls.Recall("") {
		existing[sk.ID] = true
	}

	var recorded []string
	for _, sk := range skills {
		if sk.Name == "" {
			continue
		}
		id := skillID(sk.Name)
		if existing[id] {
			continue // already a candidate: fold nothing (idempotent-ish)
		}
		if _, err := ls.AddCandidate(learning.Skill{
			ID:         id,
			Title:      sk.Description,      // L0 summary
			Body:       sk.Body,             // L1 raw
			Provenance: []string{sk.Source}, // the file it was learned from
		}); err != nil {
			continue // skip this one; a bad skill must not sink the batch
		}
		existing[id] = true // dedup within this batch too
		recorded = append(recorded, id)
	}
	sort.Strings(recorded)
	return recorded
}

// RecordFailure folds one failure into the learning store as a learning.Failure:
// Summary = "<action>: <reason>", Signature = signature (the fingerprint recall
// keys on), Prevents = prevents (the lesson to apply next time). The id is derived
// from the signature, so the same failing call folds to the same logical fact —
// recall dedups a recurrence while the record keeps every append (Law 1).
func RecordFailure(ls *learning.Store, action, reason, signature, prevents string) error {
	_, err := ls.RecordFailure(learning.Failure{
		ID:        failureID(signature),
		Summary:   fmt.Sprintf("%s: %s", action, reason),
		Signature: signature,
		Prevents:  prevents,
	})
	return err
}

// FailureMiddleware records a learning.Failure whenever a wrapped tool returns
// IsError — the persistent, record-backed failure store. Where correct.FailureMemory
// is in-memory (and forgotten on resume), this one lives in the flg record and is
// recalled by signature on any later session. Action is the tool name, Reason a
// bounded excerpt of the error value, and Signature = sha256(tool+args)[:16]. A
// successful call records nothing; the tool result the model sees is never altered
// — the memory is a side record.
func FailureMiddleware(ls *learning.Store) hx.ToolMiddleware {
	return func(next hx.ToolExecFn) hx.ToolExecFn {
		return func(in hx.ToolExecutionInput) hx.ToolResult {
			res := next(in)
			if res.IsError {
				_ = RecordFailure(ls,
					in.Name,
					excerpt(string(res.Value), maxReasonChars),
					signature(in.Name, in.Args),
					preventsFor(in.Name),
				)
			}
			return res
		}
	}
}

// Component folds the memory seam onto the runtime: at activation it discovers
// skills across the ranked roots and folds them into the learning store (they
// become acceptance-gated candidates in the record), and it installs the failure
// middleware on the ToolRuntime so every failing tool call is remembered as a
// record-backed fact. It injects KeyTools and provides nothing; the middleware
// installation is a revertible effect (unloading withdraws it). The skill fold is
// an append into the append-only record and needs no inverse — facts are permanent.
func Component(ls *learning.Store, roots []skill.Root) *runtime.Component {
	return &runtime.Component{
		Name:   "harness/memory",
		Inject: []string{hx.KeyTools},
		Apply: func(ctx *runtime.Context) error {
			// Discover + fold skills into the learning store (record-backed candidates).
			skills, err := skill.DiscoverFilesystem(roots)
			if err != nil {
				return err
			}
			RecordSkills(ls, skills)

			// Install the failure middleware on the ToolRuntime. The runtime accepts
			// middleware through an optional interface; asserting it here keeps this
			// package decoupled from the concrete tools package.
			tr, err := get[hx.ToolRuntime](ctx, hx.KeyTools)
			if err != nil {
				return err
			}
			mwUser, ok := tr.(interface {
				Use(hx.ToolMiddleware) func()
			})
			if !ok {
				return fmt.Errorf("memory: tool runtime does not support middleware")
			}
			return ctx.Use(func() (func(), error) {
				return mwUser.Use(FailureMiddleware(ls)), nil
			})
		},
	}
}

// skillID is the stable, name-derived identity a folded skill carries, so
// re-folding the same discovery recognizes it (RecordSkills dedup) and the record
// realizes one logical skill per name.
func skillID(name string) string { return "skill:" + name }

// failureID derives a folded failure's identity from its signature, so a recurring
// failing call folds to the same logical fact (recall dedups the recurrence).
func failureID(signature string) string { return "failure:" + signature }

// signature is the failing-call fingerprint: sha256(tool+args) truncated to 16 hex
// chars — deterministic and stable across sessions, so the same call recalls the
// same remembered failure.
func signature(tool string, args []byte) string {
	sum := sha256.Sum256(append([]byte(tool), args...))
	return hex.EncodeToString(sum[:])[:16]
}

// preventsFor is the default "lesson" folded onto a middleware-recorded failure —
// the guidance a future run reads before repeating a call that already failed.
func preventsFor(tool string) string {
	return fmt.Sprintf("the call to %q returned an error; change the approach or arguments before retrying", tool)
}

// excerpt trims s and head-bounds it at max, appending an omitted-count marker when
// it truncates, so an error blob cannot ride unbounded into a fact.
func excerpt(s string, max int) string {
	s = strings.TrimSpace(s)
	if max > 0 && len(s) > max {
		return s[:max] + fmt.Sprintf("… (+%d more chars)", len(s)-max)
	}
	return s
}

// get resolves the service at key and asserts it to T — the by-key resolution the
// spine uses instead of importing a concrete sibling. ctx.Get enforces the
// component's declared confinement.
func get[T any](ctx *runtime.Context, key string) (T, error) {
	var zero T
	v, err := ctx.Get(key)
	if err != nil {
		return zero, err
	}
	t, ok := v.(T)
	if !ok {
		return zero, fmt.Errorf("memory: service %q has unexpected type %T", key, v)
	}
	return t, nil
}
