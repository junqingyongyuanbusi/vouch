// Package differ — P1.5 — differential comparison and flaky arbitration.
//
// Probe runs report facts; this package owns the interpretation: delta
// (regressions / new_passing / removed_tests / skip_changes / flaky_absorbed)
// and the evidence verdict. See differ.go and flaky.go.
package differ
