// Package cache — P1.3 — BaselineCache: content-addressed base-side probe results.
//
// Key = (base_ref, probe, env_fingerprint, targets_hash). Same content-addressing
// mechanism as BundleStore; no server, no DB. Invalidation is by construction:
// changing base_ref, probe config, the environment fingerprint (see
// bundle.ProfileFingerprint), or the selected test targets yields a different
// key, so a stale baseline can never be reused. `vouch gc` clears the directory.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/junqingyongyuanbusi/vouch/internal/bundle"
)

// Key identifies one cached baseline probe result.
type Key struct {
	BaseRef        string // git ref/commit the base side was taken from
	Probe          string // probe name, e.g. "test"
	EnvFingerprint string // bundle.ProfileFingerprint(...) of the run environment
	TargetsHash    string // hash of the selected targets (empty = full run)
}

// Hash is the content address for the key (sortable, filesystem-safe).
func (k Key) Hash() string {
	h := sha256.New()
	for _, part := range []string{k.BaseRef, k.Probe, k.EnvFingerprint, k.TargetsHash} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// TargetsHashFrom returns a stable hash for a selector target set.
func TargetsHashFrom(targets []string, fullRun bool) string {
	sorted := append([]string{}, targets...)
	sort.Strings(sorted)
	flag := "narrow"
	if fullRun {
		flag = "full"
	}
	return bundle.EnvFingerprint(flag, strings.Join(sorted, "\n"))
}

// Store persists baseline results under a directory (typically .vouch/cache/baseline).
type Store struct {
	Dir string
}

// NewStore returns a store rooted at repoRoot/.vouch/cache/baseline.
func NewStore(repoRoot string) *Store {
	return &Store{Dir: filepath.Join(repoRoot, ".vouch", "cache", "baseline")}
}

// Get loads a cached value into v. Returns (false, nil) on miss or corruption —
// a damaged cache entry must never fail a verify run, it just costs a re-run.
func (s *Store) Get(k Key, v any) (bool, error) {
	data, err := os.ReadFile(s.path(k))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return false, nil // corrupt entry = miss, not a hard failure
	}
	return true, nil
}

// Put writes v atomically (tmp + rename). A write failure is reported: silently
// losing a cache write would silently change timing, not correctness.
func (s *Store) Put(k Key, v any) error {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	path := s.path(k)
	tmp, err := os.CreateTemp(s.Dir, "baseline-*.json.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// Clear removes the whole baseline cache directory (used by `vouch gc`).
func (s *Store) Clear() error {
	if err := os.RemoveAll(s.Dir); err != nil {
		return fmt.Errorf("clear baseline cache: %w", err)
	}
	return nil
}

func (s *Store) path(k Key) string {
	return filepath.Join(s.Dir, k.Hash()+".json")
}
