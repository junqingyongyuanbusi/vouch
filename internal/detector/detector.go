// Package detector — P1.1 — project sniffing cascade per PLAN-V2 §7.1.
//
// Priority: .github/workflows/*.yml (CI) → task runner (Makefile/justfile/Taskfile.yml)
// → package manager scripts (package.json/Cargo.toml/pyproject.toml/go.mod) → heuristic (lock files).
// Every Command carries its source with line number (via yaml.Node). Gaps are explicit.
// .vouch/profile.json override is always first.
package detector

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/junqingyongyuanbusi/vouch/internal/bundle"
)

// Detect returns a ProjectProfile for repoRoot. It never guesses — on miss it
// returns confidence low with gaps.
//
// Cascade levels are merged, not first-hit-stop: the highest level wins per
// command key, but lower levels fill missing keys. Real repos constantly mix
// them (e.g. a workflow that only runs `uv build` plus a pyproject pytest
// config), and stopping at the first non-empty level is exactly what breaks
// zero-config detection. Every filled command keeps its own auditable source.
func Detect(repoRoot string) (bundle.ProjectProfile, error) {
	// 0. Explicit override always wins.
	if p, ok, err := loadOverride(repoRoot); err != nil {
		return bundle.ProjectProfile{}, err
	} else if ok {
		return p, nil
	}

	merged := bundle.ProjectProfile{
		Commands: map[string]bundle.Command{},
		Language: []string{},
		Gaps:     []string{},
	}
	// Priority order: CI workflows → task runners → package manager scripts.
	for _, level := range []func(string) (bundle.ProjectProfile, bool){
		detectActions, detectTaskRunner, detectPkgManager,
	} {
		p, ok := level(repoRoot)
		if !ok {
			continue
		}
		mergeProfile(&merged, p)
	}

	if len(merged.Commands) == 0 {
		return detectHeuristic(repoRoot), nil
	}
	enrichProfile(&merged, repoRoot)
	return merged, nil
}

// mergeProfile fills missing keys from lower-priority levels and keeps the
// highest confidence seen. Existing keys are never overwritten.
func mergeProfile(dst *bundle.ProjectProfile, src bundle.ProjectProfile) {
	for k, v := range src.Commands {
		if _, exists := dst.Commands[k]; !exists {
			dst.Commands[k] = v
		}
	}
	dst.Gaps = append(dst.Gaps, src.Gaps...)
	if confidenceRank(src.Confidence) > confidenceRank(dst.Confidence) {
		dst.Confidence = src.Confidence
	}
}

func confidenceRank(c bundle.Confidence) int {
	switch c {
	case bundle.ConfidenceHigh:
		return 3
	case bundle.ConfidenceMedium:
		return 2
	case bundle.ConfidenceLow:
		return 1
	default:
		return 0
	}
}

func loadOverride(repoRoot string) (bundle.ProjectProfile, bool, error) {
	path := filepath.Join(repoRoot, ".vouch", "profile.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return bundle.ProjectProfile{}, false, nil
		}
		return bundle.ProjectProfile{}, false, err
	}
	var p bundle.ProjectProfile
	if err := json.Unmarshal(data, &p); err != nil {
		return bundle.ProjectProfile{}, false, err
	}
	// Override is explicit, trust it as high confidence but keep its gaps.
	if p.Confidence == "" {
		p.Confidence = bundle.ConfidenceHigh
	}
	return p, true, nil
}

func enrichProfile(p *bundle.ProjectProfile, repoRoot string) {
	if len(p.Language) == 0 {
		if _, err := os.Stat(filepath.Join(repoRoot, "package.json")); err == nil {
			if _, err := os.Stat(filepath.Join(repoRoot, "tsconfig.json")); err == nil {
				p.Language = []string{"typescript", "javascript"}
			} else {
				p.Language = []string{"javascript"}
			}
		} else if _, err := os.Stat(filepath.Join(repoRoot, "go.mod")); err == nil {
			p.Language = []string{"go"}
		} else if _, err := os.Stat(filepath.Join(repoRoot, "Cargo.toml")); err == nil {
			p.Language = []string{"rust"}
		} else if _, err := os.Stat(filepath.Join(repoRoot, "pyproject.toml")); err == nil {
			p.Language = []string{"python"}
		}
	}
	if p.PackageManager == nil {
		if _, err := os.Stat(filepath.Join(repoRoot, "pnpm-lock.yaml")); err == nil {
			pm := "pnpm"
			p.PackageManager = &pm
		} else if _, err := os.Stat(filepath.Join(repoRoot, "package-lock.json")); err == nil {
			pm := "npm"
			p.PackageManager = &pm
		} else if _, err := os.Stat(filepath.Join(repoRoot, "yarn.lock")); err == nil {
			pm := "yarn"
			p.PackageManager = &pm
		} else if _, err := os.Stat(filepath.Join(repoRoot, "go.mod")); err == nil {
			pm := "go"
			p.PackageManager = &pm
		} else if _, err := os.Stat(filepath.Join(repoRoot, "Cargo.lock")); err == nil {
			pm := "cargo"
			p.PackageManager = &pm
		} else if _, err := os.Stat(filepath.Join(repoRoot, "poetry.lock")); err == nil {
			pm := "poetry"
			p.PackageManager = &pm
		} else if _, err := os.Stat(filepath.Join(repoRoot, "requirements.txt")); err == nil {
			pm := "pip"
			p.PackageManager = &pm
		}
	}
	if p.Language == nil {
		p.Language = []string{}
	}
	if p.Commands == nil {
		p.Commands = map[string]bundle.Command{}
	}
	if p.Gaps == nil {
		p.Gaps = []string{}
	}
}
