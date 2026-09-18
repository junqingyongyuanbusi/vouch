// Package selector — P1.3 — affected-test selection (vitest related / jest
// --findRelatedTests / go list import graph / pytest path map); falls back to
// FullRun when mapping is unreliable. See selector.go.
//
// Both sides of a differential run MUST use the same Selection: the cache key
// includes the selected targets hash, and comparing base-full against
// candidate-narrowed would invalidate the delta.
package selector
