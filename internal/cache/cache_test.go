package cache_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/junqingyongyuanbusi/vouch/internal/bundle"
	"github.com/junqingyongyuanbusi/vouch/internal/cache"
	"github.com/junqingyongyuanbusi/vouch/internal/selector"
)

type probeResult struct {
	Pass int      `json:"pass"`
	Fail int      `json:"fail"`
	IDs  []string `json:"ids"`
}

func key(base, probe, env string, targets []string, full bool) cache.Key {
	return cache.Key{
		BaseRef:        base,
		Probe:          probe,
		EnvFingerprint: env,
		TargetsHash:    cache.TargetsHashFrom(targets, full),
	}
}

func TestCache_MissThenHit(t *testing.T) {
	store := cache.NewStore(t.TempDir())
	k := key("abc123", "test", "sha256:env1", []string{"src/add.ts"}, false)
	var out probeResult
	if hit, err := store.Get(k, &out); err != nil || hit {
		t.Fatalf("first Get should miss: hit=%v err=%v", hit, err)
	}
	want := probeResult{Pass: 2, Fail: 0, IDs: []string{"a", "b"}}
	if err := store.Put(k, want); err != nil {
		t.Fatalf("Put: %v", err)
	}
	var got probeResult
	hit, err := store.Get(k, &got)
	if err != nil || !hit {
		t.Fatalf("second Get should hit: hit=%v err=%v", hit, err)
	}
	if got.Pass != 2 || len(got.IDs) != 2 {
		t.Fatalf("cached value mismatch: %+v", got)
	}
}

func TestCache_InvalidationByKeyParts(t *testing.T) {
	store := cache.NewStore(t.TempDir())
	base := key("abc123", "test", "sha256:env1", []string{"src/add.ts"}, false)
	if err := store.Put(base, probeResult{Pass: 1}); err != nil {
		t.Fatal(err)
	}
	// Same inputs → hit
	if hit, _ := store.Get(base, &probeResult{}); !hit {
		t.Fatal("same key should hit")
	}
	cases := map[string]cache.Key{
		"base_ref changed": key("def456", "test", "sha256:env1", []string{"src/add.ts"}, false),
		"probe changed":    key("abc123", "build", "sha256:env1", []string{"src/add.ts"}, false),
		"env changed":      key("abc123", "test", "sha256:env2", []string{"src/add.ts"}, false),
		"targets changed":  key("abc123", "test", "sha256:env1", []string{"src/other.ts"}, false),
		"full vs narrowed": key("abc123", "test", "sha256:env1", []string{"src/add.ts"}, true),
	}
	for name, k := range cases {
		if k.Hash() == base.Hash() {
			t.Fatalf("%s: hash must differ", name)
		}
		if hit, _ := store.Get(k, &probeResult{}); hit {
			t.Fatalf("%s: must be a miss", name)
		}
	}
}

func TestCache_CorruptEntryIsMissNotFailure(t *testing.T) {
	root := t.TempDir()
	store := cache.NewStore(root)
	k := key("abc123", "test", "sha256:env1", nil, true)
	if err := store.Put(k, probeResult{Pass: 1}); err != nil {
		t.Fatal(err)
	}
	// Corrupt the entry
	files, _ := os.ReadDir(store.Dir)
	if len(files) != 1 {
		t.Fatalf("want 1 entry, got %d", len(files))
	}
	if err := os.WriteFile(filepath.Join(store.Dir, files[0].Name()), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	hit, err := store.Get(k, &probeResult{})
	if err != nil {
		t.Fatalf("corrupt entry must not error: %v", err)
	}
	if hit {
		t.Fatal("corrupt entry must be treated as a miss")
	}
}

func TestHash_MatchesBundleFingerprintPrimitive(t *testing.T) {
	// TargetsHashFrom must be stable and order-insensitive.
	a := cache.TargetsHashFrom([]string{"b", "a"}, false)
	b := cache.TargetsHashFrom([]string{"a", "b"}, false)
	if a != b {
		t.Fatal("targets hash must be order-insensitive")
	}
	if a == cache.TargetsHashFrom([]string{"a", "b"}, true) {
		t.Fatal("full-run flag must change the hash")
	}
	if len(a) != len(bundle.EnvFingerprint("x")) {
		t.Fatalf("targets hash should use the sha256:64hex form, got %q", a)
	}
}

// TestIntegration_SecondRunHitsBaseline shows the P1.3 mechanism end to end:
// selector narrows → baseline result cached → second identical run is a cache
// hit (base side would be skipped by the scheduler).
func TestIntegration_SecondRunHitsBaseline(t *testing.T) {
	root := t.TempDir()
	// Build a minimal repo layout for the python path map.
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "tests"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "add.py"), []byte("def add(a,b): return a+b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tests", "test_add.py"), []byte("def test_add(): pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	profile := bundle.ProjectProfile{
		Language:   []string{"python"},
		Commands:   map[string]bundle.Command{"test": {Cmd: "pytest -q", Source: "pyproject.toml#tool.pytest"}},
		Confidence: bundle.ConfidenceHigh,
	}
	sel := selector.Select(root, []string{"src/add.py"}, profile)
	if sel.FullRun {
		t.Fatalf("expected narrowing, got %s", sel.Reason)
	}
	env := bundle.ProfileFingerprint("go1.24", "", "", "lockhash", profile)
	k := cache.Key{BaseRef: "base-sha", Probe: "test", EnvFingerprint: env, TargetsHash: cache.TargetsHashFrom(sel.Targets, sel.FullRun)}

	store := cache.NewStore(root)
	if hit, _ := store.Get(k, &probeResult{}); hit {
		t.Fatal("cold run must miss")
	}
	if err := store.Put(k, probeResult{Pass: 1}); err != nil {
		t.Fatal(err)
	}
	var got probeResult
	hit, err := store.Get(k, &got)
	if err != nil || !hit {
		t.Fatalf("warm run must hit baseline cache: hit=%v err=%v", hit, err)
	}
	if got.Pass != 1 {
		t.Fatalf("cached value=%+v", got)
	}
	// A *different* target set → different key → miss.
	// (Selecting tests/test_add.py directly yields the same target set and
	// therefore the same key — that reuse is intentional, not a bug.)
	if err := os.WriteFile(filepath.Join(root, "src", "sub.py"), []byte("def sub(a,b): return a-b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tests", "test_sub.py"), []byte("def test_sub(): pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sel2 := selector.Select(root, []string{"src/sub.py"}, profile)
	if sel2.FullRun || sel2.Targets[0] != "tests/test_sub.py" {
		t.Fatalf("expected mapping to tests/test_sub.py, got %+v", sel2)
	}
	k2 := cache.Key{BaseRef: "base-sha", Probe: "test", EnvFingerprint: env, TargetsHash: cache.TargetsHashFrom(sel2.Targets, sel2.FullRun)}
	if hit, _ := store.Get(k2, &probeResult{}); hit {
		t.Fatal("different target set must miss")
	}
}
