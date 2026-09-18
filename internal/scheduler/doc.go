// Package scheduler — P1.5 — one verify run: detector → selector → worktree
// pair → builtin probes → differ → bundle. See scheduler.go for the ordering
// guarantees (single Selection for both sides, base-side cache, budget
// exhaustion recorded in unverified_claims).
package scheduler
