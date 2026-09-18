// Package gitexclude — local (.git/info/exclude) ignore management.
//
// vouch writes runtime state and agent hook files into the audited repository;
// hiding them locally (never touching the project's .gitignore) keeps them out
// of snapshots, diffs and the user's `git status`.
package gitexclude

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Add appends the given patterns to <repoRoot>/.git/info/exclude when missing.
// Missing files/directories are simply reported as an error for the caller to
// ignore: exclusion is best-effort and must never fail a verify.
func Add(repoRoot string, patterns ...string) error {
	path, err := excludePath(repoRoot)
	if err != nil {
		return err
	}
	existing, _ := os.ReadFile(path)
	body := string(existing)
	var add []string
	for _, p := range patterns {
		if p == "" || strings.Contains(body, "\n"+p+"\n") {
			continue
		}
		add = append(add, p)
	}
	if len(add) == 0 {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = f.WriteString("\n# vouch runtime state\n" + strings.Join(add, "\n") + "\n")
	return err
}

// excludePath resolves <repoRoot>/.git/info/exclude through git itself: in a
// linked worktree or submodule `.git` is a file, so a hardcoded join fails
// silently and the exclusions never apply.
func excludePath(repoRoot string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--git-path", "info/exclude")
	cmd.Dir = repoRoot
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("resolve git exclude path: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	p := strings.TrimSpace(out.String())
	if p == "" {
		return "", fmt.Errorf("resolve git exclude path: empty result")
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(repoRoot, p)
	}
	return p, nil
}
