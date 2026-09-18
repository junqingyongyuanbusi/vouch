//go:build canary

package detector_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junqingyongyuanbusi/vouch/internal/detector"
)

// TestCanary_RealRepos implements the P1.1 hard metric:
// 3 fixtures + 5 real small/medium repos → zero-config detection success ≥ 7/8.
// Run with: go test -tags canary ./internal/detector -run TestCanary -v
type repoCase struct {
	name string
	url  string
}

var realRepos = []repoCase{
	{"golang-example", "https://github.com/golang/example"},
	{"psf-requests", "https://github.com/psf/requests"},
	{"pallets-flask", "https://github.com/pallets/flask"},
	{"sindresorhus-execa", "https://github.com/sindresorhus/execa"},
	{"expressjs-express", "https://github.com/expressjs/express"},
}

func TestCanary_RealRepos(t *testing.T) {
	if testing.Short() {
		t.Skip("canary clones real repos; skipped in -short")
	}
	cache := filepath.Join(os.TempDir(), "vouch-canary-repos")
	_ = os.MkdirAll(cache, 0o755)

	root := findRepoRoot(t)
	total, ok := 0, 0

	// 3 local fixtures
	for _, fixture := range []string{
		"testdata/fixtures/ts-vitest",
		"testdata/fixtures/go-std",
		"testdata/fixtures/py-pytest",
	} {
		total++
		p, err := detector.Detect(filepath.Join(root, fixture))
		if err != nil {
			t.Errorf("fixture %s: %v", fixture, err)
			continue
		}
		if hasUsableTest(p.Commands) && p.Confidence != "low" {
			ok++
			t.Logf("PASS fixture %s: lang=%v pm=%v test=%q confidence=%s",
				fixture, p.Language, p.PackageManager, p.Commands["test"].Cmd, p.Confidence)
		} else {
			t.Errorf("FAIL fixture %s: commands=%v confidence=%s gaps=%v", fixture, p.Commands, p.Confidence, p.Gaps)
		}
	}

	// 5 real repos
	for _, rc := range realRepos {
		total++
		dir := filepath.Join(cache, rc.name)
		if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
			cmd := exec.Command("git", "clone", "--depth", "1", "--quiet", rc.url, dir)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("clone %s: %v\n%s", rc.url, err, out)
				continue
			}
		}
		p, err := detector.Detect(dir)
		if err != nil {
			t.Errorf("detect %s: %v", rc.name, err)
			continue
		}
		if hasUsableTest(p.Commands) && p.Confidence != "low" {
			ok++
			src := p.Commands["test"].Source
			if src == "" {
				t.Errorf("FAIL %s: source must be auditable, got empty", rc.name)
			}
			if strings.Contains(src, "#L") {
				line := src[strings.Index(src, "#L")+2:]
				if line == "" || line == "0" {
					t.Errorf("FAIL %s: #L line must be >0, got %q", rc.name, src)
				}
			}
			t.Logf("PASS %s: lang=%v pm=%v test=%q src=%s confidence=%s gaps=%v",
				rc.name, p.Language, p.PackageManager, p.Commands["test"].Cmd, src, p.Confidence, p.Gaps)
		} else {
			if len(p.Gaps) == 0 {
				t.Errorf("FAIL %s: failed detection must carry field-precise gaps, got none", rc.name)
			}
			fieldPrecise := false
			for _, g := range p.Gaps {
				if strings.Contains(g, "commands.") || strings.Contains(g, "package_manager") || strings.Contains(g, "language") {
					fieldPrecise = true
					break
				}
			}
			if !fieldPrecise {
				t.Errorf("FAIL %s: gaps not field-precise: %v", rc.name, p.Gaps)
			}
			t.Errorf("FAIL %s: commands=%v confidence=%s gaps=%v", rc.name, p.Commands, p.Confidence, p.Gaps)
		}
	}

	t.Logf("CANARY RESULT: %d/%d (need >=7/8)", ok, total)
	if ok < 7 {
		t.Fatalf("zero-config detection %d/%d < 7/8", ok, total)
	}
}
