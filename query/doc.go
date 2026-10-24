// Package query is the Query Layer (system design §4.10, §10): ONE read
// abstraction over the Factory Lineage Graph, two interchangeable backends behind
// a single seam, and the three hot traversals that run inside their SLOs.
//
// The seam is GraphIndexPort (port.go). Every read — audit, impact, replay,
// retrieval — goes through it, so the backend is a deployment choice, never a
// caller concern:
//
//   - Recursive CTE over Postgres  — the baseline and the conformance oracle. The
//     §5.5 relational tables (obligations / evidence / supersessions / edges) are
//     the source projection; a `WITH RECURSIVE` walk over the edge table serves
//     each traversal. This is what production runs first.
//   - Graph index (optional)       — a traversal accelerator qualified in later,
//     strictly optional, never load-bearing; the tables stay the rebuildable
//     source.
//   - In-memory graph              — this package's implementation (index.go):
//     small graphs traverse in process behind the SAME port. It is the reference
//     the CTE backend is a conformance swap for.
//
// Determinism is a law here, not a nicety (Law 3, §5.6). No traversal takes a
// timestamp; every result is content-address / id sorted; every declared query
// result is pinned to a projection_hash so the same preserved inputs yield a
// byte-identical answer (declared.go, §10.3). Retrieval is structural + lexical +
// graph only — no vector store, no embeddings (retrieval.go, §10.4) — and every
// item it emits resolves to a real fact id / path@commit. Every traversal hop
// carries edge provenance (canonical vs inferred); a gate refuses an inferred
// edge as proof.
package query
