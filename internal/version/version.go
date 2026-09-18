// Package version — build-time version injection for goreleaser.
//
// Set via -ldflags:
//
//	-X github.com/junqingyongyuanbusi/vouch/internal/version.Version={{.Version}}
//	-X github.com/junqingyongyuanbusi/vouch/internal/version.Commit={{.Commit}}
package version

// Version is the release tag (e.g. v0.1.0). Default is the P0 skeleton version.
var Version = "0.1.0"

// Commit is the git SHA at build time.
var Commit = "unknown"
