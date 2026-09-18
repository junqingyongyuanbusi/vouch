// Package verdict — VOUCH differential certifier.
//
// Dependency direction (PLAN-V2 §10): cmd → orchestration
// (detector/selector/planner/scheduler) → execution
// (worktree/sandbox/probe) → evidence (bundle/cache/differ/verdict).
// This package must not import packages above it in the chain.
package verdict
