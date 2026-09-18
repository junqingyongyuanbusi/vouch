package selector

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// goListPkg is the subset of `go list -json ./...` we need.
type goListPkg struct {
	ImportPath   string
	Dir          string
	GoFiles      []string
	TestGoFiles  []string
	XTestGoFiles []string
	Imports      []string
	// TestImports / XTestImports matter: a package can depend on another only
	// from its tests, and missing that edge under-selects tests (silent miss →
	// false VERIFIED), the worst failure mode for a differential verifier.
	TestImports  []string
	XTestImports []string
}

// selectGo builds the reverse import graph from `go list -json ./...` and
// returns the packages affected by the changed files (the changed packages
// plus every package that imports them, transitively).
func selectGo(repoRoot string, files []string) Selection {
	listCmd := exec.Command("go", "list", "-json", "./...")
	listCmd.Dir = repoRoot
	out, err := listCmd.Output()
	if err != nil {
		return Selection{FullRun: true, Mechanism: "full fallback", Reason: "go list failed: " + err.Error()}
	}
	pkgs, err := decodeGoList(out)
	if err != nil {
		return Selection{FullRun: true, Mechanism: "full fallback", Reason: "go list parse failed: " + err.Error()}
	}
	if len(pkgs) == 0 {
		return Selection{FullRun: true, Mechanism: "full fallback", Reason: "no packages found"}
	}

	// Reverse edges: importPath → packages importing it.
	importedBy := map[string][]string{}
	for _, p := range pkgs {
		deps := append(append(append([]string{}, p.Imports...), p.TestImports...), p.XTestImports...)
		for _, imp := range deps {
			importedBy[imp] = append(importedBy[imp], p.ImportPath)
		}
	}
	// Changed files → packages (source, internal test, external test files).
	direct := map[string]bool{}
	unmapped := false
	for _, f := range files {
		abs := filepath.Join(repoRoot, filepath.FromSlash(f))
		pkgDir := filepath.Dir(abs)
		base := filepath.Base(abs)
		found := false
		for _, p := range pkgs {
			if !samePath(p.Dir, pkgDir) {
				continue
			}
			for _, name := range append(append(append([]string{}, p.GoFiles...), p.TestGoFiles...), p.XTestGoFiles...) {
				if name == base {
					direct[p.ImportPath] = true
					found = true
				}
			}
		}
		if !found {
			unmapped = true
		}
	}
	if unmapped {
		return Selection{
			FullRun:   true,
			Mechanism: "full fallback",
			Reason:    "changed file does not map to any package (new file or non-Go file)",
		}
	}

	// Transitively include importers.
	affected := map[string]bool{}
	queue := make([]string, 0, len(direct))
	for p := range direct {
		affected[p] = true
		queue = append(queue, p)
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, up := range importedBy[cur] {
			if !affected[up] {
				affected[up] = true
				queue = append(queue, up)
			}
		}
	}

	// Targets are package directories relative to repo root.
	targets := make([]string, 0, len(affected))
	for _, p := range pkgs {
		if affected[p.ImportPath] {
			rel, err := filepath.Rel(repoRoot, p.Dir)
			if err != nil {
				rel = p.ImportPath
			}
			targets = append(targets, "./"+filepath.ToSlash(rel))
		}
	}
	sort.Strings(targets)
	return Selection{Targets: targets, Mechanism: "go list import graph"}
}

func decodeGoList(out []byte) ([]goListPkg, error) {
	var pkgs []goListPkg
	dec := json.NewDecoder(strings.NewReader(string(out)))
	for dec.More() {
		var p goListPkg
		if err := dec.Decode(&p); err != nil {
			return nil, err
		}
		pkgs = append(pkgs, p)
	}
	return pkgs, nil
}

// samePath compares two directories, resolving symlinks when possible
// (/var vs /private/var on macOS; worktrees under TMPDIR).
func samePath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && filepath.Clean(ra) == filepath.Clean(rb)
}
