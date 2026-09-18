// fingerprint.go — P1.0 — stable env/profile hashing for BaselineCache.
//
// ProfileFingerprint is the cache-key component that must be stable and
// single-sourced. It reuses bundle.EnvFingerprint (which joins with \x00)
// and documents the exact concatenation order so `go version` string jitter
// cannot silently miss the cache.
package bundle

import (
	"fmt"
	"runtime"
	"sort"
	"strings"
)

// ProfileFingerprint returns a stable 64hex env fingerprint for BaselineCache.
// Order is fixed and separator is \x00 (same as EnvFingerprint primitive):
//
//	goVersion \x00 runtime.GOOS/GOARCH \x00 nodeVersion \x00 pnpmVersion \x00 lockHash \x00 sortedCommands \x00 sortedLanguage \x00 packageManager
//	\x00 testSelection \x00 confidence
//
// GOOS/GOARCH is intentionally in the key: BaselineCache is local, not shared across
// platforms; different arch must not reuse a baseline (go test cache semantics). The
// invalidation doc (PLAN-V2 §7.3) lists this as an env-fingerprint change.
//
// Callers pass the already-collected strings; missing values should be "" (not omitted)
// to keep the key stable. The function does not shell out — collection is caller's job.
func ProfileFingerprint(goVersion, nodeVersion, pnpmVersion, lockHash string, profile ProjectProfile) string {
	// Sorted commands for determinism: "key=cmd@source"
	var cmds []string
	for k, c := range profile.Commands {
		cmds = append(cmds, fmt.Sprintf("%s=%s@%s", k, c.Cmd, c.Source))
	}
	sort.Strings(cmds)
	joined := ""
	for _, c := range cmds {
		joined += c + "\n"
	}
	// TestSelection must be stable: avoid %v on pointer (address semantics).
	var ts string
	if profile.TestSelection == nil {
		ts = "nil"
	} else {
		ts = fmt.Sprintf("%v:%s", profile.TestSelection.Supported, profile.TestSelection.Mechanism)
	}
	// Language / PackageManager / Gaps must be in key — otherwise ts-vitest and go-std with same Commands collide.
	langs := append([]string{}, profile.Language...)
	sort.Strings(langs)
	sortedLang := strings.Join(langs, ",")
	pm := ""
	if profile.PackageManager != nil {
		pm = *profile.PackageManager
	}
	gaps := append([]string{}, profile.Gaps...)
	sort.Strings(gaps)
	sortedGaps := strings.Join(gaps, ",")
	return EnvFingerprint(
		goVersion,
		runtime.GOOS+"/"+runtime.GOARCH,
		nodeVersion,
		pnpmVersion,
		lockHash,
		joined,
		sortedLang,
		pm,
		sortedGaps,
		ts,
		string(profile.Confidence),
	)
}
