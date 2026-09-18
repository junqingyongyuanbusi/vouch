package detector_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junqingyongyuanbusi/vouch/internal/bundle"
	"github.com/junqingyongyuanbusi/vouch/internal/detector"
)

func TestDetect_Fixtures(t *testing.T) {
	cases := []struct {
		name     string
		fixture  string
		wantLang string
		wantTest string
	}{
		{"ts-vitest", "testdata/fixtures/ts-vitest", "typescript", "package.json#scripts.test"},
		{"go-std", "testdata/fixtures/go-std", "go", "go.mod#default"},
		{"py-pytest", "testdata/fixtures/py-pytest", "python", "pyproject.toml#tool.pytest"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := findRepoRoot(t)
			fixturePath := filepath.Join(root, tc.fixture)
			profile, err := detector.Detect(fixturePath)
			if err != nil {
				t.Fatalf("Detect: %v", err)
			}
			if len(profile.Language) == 0 || !contains(profile.Language, tc.wantLang) {
				t.Fatalf("want lang %q in %v", tc.wantLang, profile.Language)
			}
			cmd, ok := profile.Commands["test"]
			if !ok {
				t.Fatalf("want test command, got %+v", profile.Commands)
			}
			if !strings.Contains(cmd.Source, tc.wantTest) {
				t.Fatalf("want source contain %q, got %q", tc.wantTest, cmd.Source)
			}
			// Source must be auditable: either yaml line (#L>0) or pkg anchor (#scripts/#default/#tool)
			if cmd.Source == "" {
				t.Fatalf("source empty")
			}
			if strings.Contains(cmd.Source, "#L") {
				parts := strings.Split(cmd.Source, "#L")
				if len(parts) != 2 || parts[1] == "0" || parts[1] == "" {
					t.Fatalf("source line invalid: %q", cmd.Source)
				}
			} else if !(strings.Contains(cmd.Source, "#scripts.") || strings.Contains(cmd.Source, "#default") || strings.Contains(cmd.Source, "#tool.")) {
				t.Fatalf("source must contain #L or #scripts/#default/#tool, got %q", cmd.Source)
			}
		})
	}
}

func TestDetect_ActionsMatrixAndGaps(t *testing.T) {
	dir := t.TempDir()
	wfDir := filepath.Join(dir, ".github", "workflows")
	if err := os.MkdirAll(wfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Workflow with matrix and services
	yml := `
name: ci
on: push
jobs:
  test:
    strategy:
      matrix:
        node: [18, 20]
    services:
      db:
        image: postgres
    steps:
      - run: pnpm vitest run
`
	if err := os.WriteFile(filepath.Join(wfDir, "ci.yml"), []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	profile, err := detector.Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if _, ok := profile.Commands["test"]; !ok {
		t.Fatalf("want test command from workflow, got %+v", profile.Commands)
	}
	// Must have gaps for matrix/services and confidence medium
	hasMatrix := false
	hasService := false
	for _, g := range profile.Gaps {
		if strings.Contains(g, "matrix") {
			hasMatrix = true
		}
		if strings.Contains(g, "services") {
			hasService = true
		}
	}
	if !hasMatrix || !hasService {
		t.Fatalf("want gaps for matrix/services, got %v", profile.Gaps)
	}
	if profile.Confidence != "medium" {
		t.Fatalf("want medium confidence for matrix, got %q", profile.Confidence)
	}
	// Source must have real line number
	cmd := profile.Commands["test"]
	if !strings.Contains(cmd.Source, ".github/workflows/ci.yml#L") {
		t.Fatalf("want source .github/workflows/ci.yml#L, got %q", cmd.Source)
	}
}

func TestDetect_ProfileOverride(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".vouch"), 0o755); err != nil {
		t.Fatal(err)
	}
	override := `{"language":["custom"],"package_manager":"custom","commands":{"test":{"cmd":"echo hi","source":".vouch/profile.json"}},"confidence":"high","gaps":[]}`
	if err := os.WriteFile(filepath.Join(dir, ".vouch", "profile.json"), []byte(override), 0o644); err != nil {
		t.Fatal(err)
	}
	profile, err := detector.Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(profile.Language) == 0 || profile.Language[0] != "custom" {
		t.Fatalf("override not respected: %+v", profile)
	}
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func hasUsableTest(cmds map[string]bundle.Command) bool {
	c, ok := cmds["test"]
	return ok && strings.TrimSpace(c.Cmd) != ""
}
