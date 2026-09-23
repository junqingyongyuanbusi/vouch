package builtin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"

	"github.com/junqingyongyuanbusi/vouch/internal/probe"
)

// Build is the built-in build probe: success/failure + optional artifact hash.
type Build struct{}

func (Build) Name() string     { return "build" }
func (Build) Kind() probe.Kind { return probe.KindDeterministic }

func (Build) Run(ctx context.Context, in Input) (probe.RunResult, error) {
	if in.Command == "" {
		return probe.Inconclusive("no build command in profile", in.Role), nil
	}
	out, exit, timedOut, err := runShell(ctx, in)
	if err != nil {
		return probe.Inconclusive("run: "+err.Error(), in.Role), nil
	}
	if timedOut {
		return probe.Inconclusive("command killed by budget", in.Role), nil
	}
	if missingRunner(out, exit) {
		return probe.Inconclusive("runner not available ("+in.Command+")", in.Role), nil
	}
	data := map[string]interface{}{}
	data["exit_code"] = exit
	if h, ok := ArtifactHash(in.Workdir); ok {
		data["artifact_sha256"] = h
	}
	verdict := probe.VerdictPass
	summary := "build passed"
	if exit != 0 {
		verdict = probe.VerdictFail
		summary = "build failed"
		data["stderr_tail"] = tail(out, 2000)
	}
	return probe.RunResult{Verdict: verdict, Summary: summary, Data: data}, nil
}

// ArtifactHash hashes dist/ (or bin/) contents when present. Best effort:
// repos without artifacts simply omit the hash rather than fabricating one.
func ArtifactHash(workdir string) (string, bool) {
	for _, dir := range []string{"dist", "bin"} {
		root := filepath.Join(workdir, dir)
		info, err := os.Stat(root)
		if err != nil || !info.IsDir() {
			continue
		}
		var files []string
		_ = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
			if err == nil && !fi.IsDir() {
				files = append(files, p)
			}
			return nil
		})
		if len(files) == 0 {
			continue
		}
		sort.Strings(files)
		h := sha256.New()
		for _, f := range files {
			rel, _ := filepath.Rel(workdir, f)
			h.Write([]byte(rel))
			if b, err := os.ReadFile(f); err == nil {
				_ = b
				inner := sha256.Sum256(b)
				h.Write(inner[:])
			}
		}
		return "sha256:" + hex.EncodeToString(h.Sum(nil)), true
	}
	return "", false
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
