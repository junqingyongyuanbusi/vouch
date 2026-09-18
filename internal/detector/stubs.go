package detector

import (
	"os"
	"path/filepath"

	"github.com/junqingyongyuanbusi/vouch/internal/bundle"
)

// detectHeuristic is the final fallback. It still tries to name what it can
// infer from manifests/lock files, then reports field-precise gaps so the
// caller can render an actionable UNVERIFIED ("验不了就说验不了").
func detectHeuristic(repoRoot string) bundle.ProjectProfile {
	p := bundle.ProjectProfile{
		Commands:   map[string]bundle.Command{},
		Language:   []string{},
		Gaps:       []string{},
		Confidence: bundle.ConfidenceLow,
	}
	exists := func(rel string) bool {
		_, err := os.Stat(filepath.Join(repoRoot, rel))
		return err == nil
	}

	// Language / package manager from manifests (no commands: we do not guess how to run).
	switch {
	case exists("package.json"):
		p.Language = []string{"javascript"}
		if exists("tsconfig.json") {
			p.Language = []string{"typescript", "javascript"}
		}
		pm := "npm"
		switch {
		case exists("pnpm-lock.yaml"):
			pm = "pnpm"
		case exists("yarn.lock"):
			pm = "yarn"
		case exists("bun.lockb"):
			pm = "bun"
		}
		p.PackageManager = &pm
	case exists("go.mod"):
		p.Language = []string{"go"}
		pm := "go"
		p.PackageManager = &pm
	case exists("Cargo.toml"):
		p.Language = []string{"rust"}
		pm := "cargo"
		p.PackageManager = &pm
	case exists("pyproject.toml"), exists("setup.py"), exists("requirements.txt"):
		p.Language = []string{"python"}
		pm := "pip"
		if exists("poetry.lock") {
			pm = "poetry"
		} else if exists("uv.lock") {
			pm = "uv"
		}
		p.PackageManager = &pm
	}

	if len(p.Language) == 0 {
		p.Gaps = append(p.Gaps, "language: not detected (no package.json/go.mod/Cargo.toml/pyproject.toml)")
	}
	if p.PackageManager == nil {
		p.Gaps = append(p.Gaps, "package_manager: not detected")
	}
	p.Gaps = append(p.Gaps,
		"commands.test: not detected (no CI workflow step, task runner target, or package script)",
		"commands.build: not detected",
	)
	return p
}
