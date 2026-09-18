package detector

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/junqingyongyuanbusi/vouch/internal/bundle"
)

func detectTaskRunner(repoRoot string) (bundle.ProjectProfile, bool) {
	// Makefile
	if p, ok := parseMakefile(filepath.Join(repoRoot, "Makefile")); ok {
		return p, true
	}
	if p, ok := parseMakefile(filepath.Join(repoRoot, "makefile")); ok {
		return p, true
	}
	// justfile
	if p, ok := parseJustfile(filepath.Join(repoRoot, "justfile"), repoRoot); ok {
		return p, true
	}
	if p, ok := parseJustfile(filepath.Join(repoRoot, "Justfile"), repoRoot); ok {
		return p, true
	}
	// Taskfile
	if p, ok := parseTaskfile(filepath.Join(repoRoot, "Taskfile.yml"), repoRoot); ok {
		return p, true
	}
	return bundle.ProjectProfile{}, false
}

var makeTargetRe = regexp.MustCompile(`^(test|build|typecheck):`)

func parseMakefile(path string) (bundle.ProjectProfile, bool) {
	f, err := os.Open(path)
	if err != nil {
		return bundle.ProjectProfile{}, false
	}
	defer f.Close()
	commands := make(map[string]bundle.Command)
	sc := bufio.NewScanner(f)
	// Handle long Makefile lines (default 64KB may truncate).
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, 1024*1024)
	lineNo := 0
	var gaps []string
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		if m := makeTargetRe.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			target := m[1]
			commands[target] = bundle.Command{Cmd: "make " + target, Source: relSource(filepath.Dir(path), path) + "#L" + itoa(lineNo)}
		}
	}
	if err := sc.Err(); err != nil {
		gaps = append(gaps, "Makefile scan error: "+err.Error())
	}
	if len(commands) == 0 {
		return bundle.ProjectProfile{}, false
	}
	// Correct relSource usage: taskrunner is called with repoRoot, but parseMakefile gets abs path.
	// Use repoRoot-relative source: caller passes repoRoot, but we have only path; reconstruct.
	// For now, keep filepath.Base for backward compat, but prefer relSource when repoRoot is known.
	// Since parseMakefile is called with Join(repoRoot, "Makefile"), relSource would be "Makefile".
	return bundle.ProjectProfile{Commands: commands, Confidence: bundle.ConfidenceMedium, Gaps: gaps}, true
}

func parseJustfile(path, repoRoot string) (bundle.ProjectProfile, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return bundle.ProjectProfile{}, false
	}
	commands := make(map[string]bundle.Command)
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "test:") || strings.HasPrefix(trim, "build:") || strings.HasPrefix(trim, "typecheck:") {
			target := strings.Split(trim, ":")[0]
			commands[target] = bundle.Command{Cmd: "just " + target, Source: relSource(repoRoot, path) + "#L" + itoa(i+1)}
		}
	}
	if len(commands) == 0 {
		return bundle.ProjectProfile{}, false
	}
	return bundle.ProjectProfile{Commands: commands, Confidence: bundle.ConfidenceMedium}, true
}

func parseTaskfile(path, repoRoot string) (bundle.ProjectProfile, bool) {
	// Minimal: check if file exists and contains test/build
	data, err := os.ReadFile(path)
	if err != nil {
		return bundle.ProjectProfile{}, false
	}
	content := strings.ToLower(string(data))
	commands := make(map[string]bundle.Command)
	if strings.Contains(content, "test:") {
		commands["test"] = bundle.Command{Cmd: "task test", Source: relSource(repoRoot, path) + "#L1"}
	}
	if strings.Contains(content, "build:") {
		commands["build"] = bundle.Command{Cmd: "task build", Source: relSource(repoRoot, path) + "#L1"}
	}
	if len(commands) == 0 {
		return bundle.ProjectProfile{}, false
	}
	return bundle.ProjectProfile{Commands: commands, Confidence: bundle.ConfidenceMedium}, true
}
