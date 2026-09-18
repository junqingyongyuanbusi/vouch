package detector

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/junqingyongyuanbusi/vouch/internal/bundle"
)

func detectPkgManager(repoRoot string) (bundle.ProjectProfile, bool) {
	// package.json
	if p, ok := parsePackageJSON(filepath.Join(repoRoot, "package.json"), repoRoot); ok {
		return p, true
	}
	// Cargo.toml
	if p, ok := parseCargoToml(filepath.Join(repoRoot, "Cargo.toml"), repoRoot); ok {
		return p, true
	}
	// pyproject.toml
	if p, ok := parsePyProject(filepath.Join(repoRoot, "pyproject.toml"), repoRoot); ok {
		return p, true
	}
	// go.mod
	if _, err := os.Stat(filepath.Join(repoRoot, "go.mod")); err == nil {
		pm := "go"
		return bundle.ProjectProfile{
			Language:       []string{"go"},
			PackageManager: &pm,
			Commands: map[string]bundle.Command{
				"test": {Cmd: "go test ./...", Source: "go.mod#default"},
			},
			Confidence: bundle.ConfidenceHigh,
		}, true
	}
	return bundle.ProjectProfile{}, false
}

func parsePackageJSON(path, repoRoot string) (bundle.ProjectProfile, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return bundle.ProjectProfile{}, false
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil || len(pkg.Scripts) == 0 {
		return bundle.ProjectProfile{}, false
	}
	commands := make(map[string]bundle.Command)
	// Deterministic: sort keys, exact match first, then contains fallback without overwriting.
	keys := make([]string, 0, len(pkg.Scripts))
	for k := range pkg.Scripts {
		keys = append(keys, k)
	}
	// sort for determinism
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	// Exact matches first.
	for _, k := range keys {
		v := pkg.Scripts[k]
		lower := strings.ToLower(k)
		switch lower {
		case "test":
			commands["test"] = bundle.Command{Cmd: v, Source: "package.json#scripts." + k}
		case "build":
			commands["build"] = bundle.Command{Cmd: v, Source: "package.json#scripts." + k}
		case "typecheck":
			commands["typecheck"] = bundle.Command{Cmd: v, Source: "package.json#scripts." + k}
		}
	}
	// Contains fallback only if not already set (e.g. test:watch should not overwrite test).
	for _, k := range keys {
		v := pkg.Scripts[k]
		lower := strings.ToLower(k)
		if _, ok := commands["test"]; !ok && strings.Contains(lower, "test") {
			commands["test"] = bundle.Command{Cmd: v, Source: "package.json#scripts." + k}
		}
		if _, ok := commands["typecheck"]; !ok && (strings.Contains(lower, "typecheck") || strings.Contains(strings.ToLower(v), "tsc") && strings.Contains(strings.ToLower(v), "noemit")) {
			commands["typecheck"] = bundle.Command{Cmd: v, Source: "package.json#scripts." + k}
		}
	}
	if len(commands) == 0 {
		return bundle.ProjectProfile{}, false
	}
	pm := "npm"
	// pnpm detection via pnpm-lock.yaml
	if _, err := os.Stat(filepath.Join(repoRoot, "pnpm-lock.yaml")); err == nil {
		pm = "pnpm"
	}
	lang := []string{"typescript", "javascript"}
	if _, err := os.Stat(filepath.Join(repoRoot, "tsconfig.json")); err != nil {
		lang = []string{"javascript"}
	}
	return bundle.ProjectProfile{
		Language:       lang,
		PackageManager: &pm,
		Commands:       commands,
		Confidence:     bundle.ConfidenceHigh,
	}, true
}

func parseCargoToml(path, repoRoot string) (bundle.ProjectProfile, bool) {
	if _, err := os.Stat(path); err != nil {
		return bundle.ProjectProfile{}, false
	}
	pm := "cargo"
	return bundle.ProjectProfile{
		Language:       []string{"rust"},
		PackageManager: &pm,
		Commands: map[string]bundle.Command{
			"test":  {Cmd: "cargo test", Source: "Cargo.toml#default"},
			"build": {Cmd: "cargo build", Source: "Cargo.toml#default"},
		},
		Confidence: bundle.ConfidenceHigh,
	}, true
}

func parsePyProject(path, repoRoot string) (bundle.ProjectProfile, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return bundle.ProjectProfile{}, false
	}
	content := strings.ToLower(string(data))
	if !strings.Contains(content, "pytest") {
		return bundle.ProjectProfile{}, false
	}
	pm := "pip"
	return bundle.ProjectProfile{
		Language:       []string{"python"},
		PackageManager: &pm,
		Commands: map[string]bundle.Command{
			"test": {Cmd: "pytest -q", Source: "pyproject.toml#tool.pytest"},
		},
		Confidence: bundle.ConfidenceHigh,
	}, true
}
