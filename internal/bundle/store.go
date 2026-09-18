// Package bundle — store (P1.0) — content-addressed bundle persistence.
//
// Single source of truth: bundle.json contains the full ProofBundle (including evidence[]).
// evidence/*.json and blobs/ are human-inspectable projections only; Load reads bundle.json.
// BundleID = hex(sha256(diff_bytes + "\x00" + base_ref))[:8] — stable, 6-12 hex, see docs/bundle-schema.json.
// .vouch is gitignored; .vouch/tmp-* is used for hardlink-friendly temp (same filesystem for cp -al).
package bundle

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Store is a filesystem store rooted at repoRoot/.vouch.
type Store struct {
	RepoRoot string // repo root, e.g. "."
	Dir      string // ".vouch" by default; use ".vouch" for repo-local
}

// NewStore returns a store at repoRoot/.vouch.
func NewStore(repoRoot string) *Store {
	return &Store{RepoRoot: repoRoot, Dir: filepath.Join(repoRoot, ".vouch")}
}

func (s *Store) bundlesDir() string         { return filepath.Join(s.Dir, "bundles") }
func (s *Store) bundleDir(id string) string { return filepath.Join(s.bundlesDir(), id) }

// Save persists bundle as .vouch/bundles/<id>/bundle.json (single source of truth).
// It also writes evidence projections to evidence/<evID>.json and blobs to blobs/sha256-*/ for human inspection.
// bundleID must already satisfy ^[a-f0-9]{6,12}$ and match BundleIDFor() if caller cares about stability.
func (s *Store) Save(b ProofBundle, logs map[string][]byte) (string, error) {
	if err := b.Validate(); err != nil {
		return "", fmt.Errorf("bundle Validate: %w", err)
	}
	id := b.BundleID
	if id == "" {
		return "", fmt.Errorf("bundle_id required")
	}
	dir := s.bundleDir(id)
	if err := os.MkdirAll(filepath.Join(dir, "evidence"), 0o755); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(dir, "blobs"), 0o755); err != nil {
		return "", err
	}
	// bundle.json — single source of truth (contains evidence[]). Atomic via tmp+rename.
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return "", err
	}
	// Unique temp name: two concurrent verifies of the same diff share a
	// bundle id, and a fixed ".tmp" name would let them clobber each other.
	tmp, err := os.CreateTemp(dir, "bundle-*.json.tmp")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", err
	}
	if err := os.Rename(tmpName, filepath.Join(dir, "bundle.json")); err != nil {
		_ = os.Remove(tmpName)
		return "", err
	}
	// evidence projections (optional, not read by Load).
	for _, ev := range b.Evidence {
		evData, err := json.MarshalIndent(ev, "", "  ")
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(dir, "evidence", ev.ID+".json"), evData, 0o644); err != nil {
			return "", err
		}
	}
	// blobs — key is like "sha256-abc/test.log" (without "blobs/" prefix per Save contract);
	// if caller passes "blobs/..." we trim it to avoid double prefix .vouch/bundles/<id>/blobs/blobs/...
	for name, content := range logs {
		for strings.HasPrefix(name, "blobs/") {
			name = strings.TrimPrefix(name, "blobs/")
		}
		clean := filepath.Clean(name)
		if filepath.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || strings.Contains(clean, string(filepath.Separator)+".."+string(filepath.Separator)) || strings.HasSuffix(clean, string(filepath.Separator)+"..") {
			return "", fmt.Errorf("invalid blob name %q: path traversal", name)
		}
		p := filepath.Join(dir, "blobs", filepath.FromSlash(clean))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(p, content, 0o644); err != nil {
			return "", err
		}
	}
	return id, nil
}

// Load reads bundle.json (single source of truth). Evidence projections are ignored.
func (s *Store) Load(id string) (ProofBundle, error) {
	data, err := os.ReadFile(filepath.Join(s.bundleDir(id), "bundle.json"))
	if err != nil {
		return ProofBundle{}, err
	}
	var b ProofBundle
	if err := json.Unmarshal(data, &b); err != nil {
		return ProofBundle{}, err
	}
	if err := b.Validate(); err != nil {
		return ProofBundle{}, fmt.Errorf("loaded bundle invalid: %w", err)
	}
	return b, nil
}

// BundleMeta is a light entry for List().
type BundleMeta struct {
	BundleID string        `json:"bundle_id"`
	Verdict  GlobalVerdict `json:"verdict"`
	SavedAt  time.Time     `json:"saved_at,omitempty"`
}

// List returns bundle ids sorted lexicographically by BundleID (stable, 6-12 hex).
// Corrupt bundles are not silently skipped; they are returned as UNVERIFIED so `vouch show`/`gc` can surface them.
func (s *Store) List() ([]BundleMeta, error) {
	entries, err := os.ReadDir(s.bundlesDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []BundleMeta
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		if !reBundleID.MatchString(id) {
			continue
		}
		b, err := s.Load(id)
		if err != nil {
			out = append(out, BundleMeta{BundleID: id, Verdict: Unverified})
			continue
		}
		meta := BundleMeta{BundleID: id, Verdict: b.Verdict}
		if fi, err := os.Stat(filepath.Join(s.bundleDir(id), "bundle.json")); err == nil {
			meta.SavedAt = fi.ModTime()
		}
		out = append(out, meta)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BundleID < out[j].BundleID })
	return out, nil
}

// BundleIDFor generates a stable bundle ID from diff bytes and base ref.
// Uses sha256(diff + "\x00" + baseRef) hex truncated to 8 chars (within 6-12).
func BundleIDFor(diff []byte, baseRef string) string {
	buf := make([]byte, 0, len(diff)+1+len(baseRef))
	buf = append(buf, diff...)
	buf = append(buf, 0)
	buf = append(buf, baseRef...)
	h := DiffSHA256(buf)
	// h is "sha256:<64hex>"
	hexPart := strings.TrimPrefix(h, "sha256:")
	if len(hexPart) < 8 {
		return hexPart
	}
	return hexPart[:8]
}

// Latest returns the most recently written bundle (by bundle.json mtime).
// Bundle ids are content hashes, so id order carries no time meaning.
func (s *Store) Latest() (BundleMeta, bool, error) {
	metas, err := s.List()
	if err != nil || len(metas) == 0 {
		return BundleMeta{}, false, err
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].SavedAt.After(metas[j].SavedAt) })
	return metas[0], true, nil
}
