package detector

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/junqingyongyuanbusi/vouch/internal/bundle"
	"gopkg.in/yaml.v3"
)

// detectActions parses .github/workflows/*.yml and extracts run commands.
// It uses yaml.Node to preserve line numbers for source provenance.
func detectActions(repoRoot string) (bundle.ProjectProfile, bool) {
	pattern := filepath.Join(repoRoot, ".github", "workflows", "*.yml")
	matches, _ := filepath.Glob(pattern)
	pattern2 := filepath.Join(repoRoot, ".github", "workflows", "*.yaml")
	m2, _ := filepath.Glob(pattern2)
	matches = append(matches, m2...)
	if len(matches) == 0 {
		return bundle.ProjectProfile{}, false
	}

	commands := make(map[string]bundle.Command)
	var gaps []string
	found := false
	confidence := bundle.ConfidenceHigh

	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var root yaml.Node
		if err := yaml.Unmarshal(data, &root); err != nil {
			gaps = append(gaps, "workflow parse failed: "+filepath.Base(path))
			confidence = bundle.ConfidenceMedium
			continue
		}
		// Walk document → mapping.
		if len(root.Content) == 0 {
			continue
		}
		doc := root.Content[0]
		// Find "jobs" key.
		jobsNode := findMappingValue(doc, "jobs")
		if jobsNode == nil || jobsNode.Kind != yaml.MappingNode {
			continue
		}
		// Iterate jobs.
		for i := 0; i < len(jobsNode.Content); i += 2 {
			jobKey := jobsNode.Content[i]
			jobVal := jobsNode.Content[i+1]
			// Skip reusable workflow (uses: ...) and matrix handling note.
			if hasKey(jobVal, "uses") {
				gaps = append(gaps, "workflow "+filepath.Base(path)+" job "+jobKey.Value+" uses reusable workflow, skipped")
				confidence = bundle.ConfidenceMedium
				continue
			}
			if runs := findMappingValue(jobVal, "runs"); runs != nil {
				if using := findMappingValue(runs, "using"); using != nil && strings.Contains(strings.ToLower(using.Value), "composite") {
					gaps = append(gaps, "workflow "+filepath.Base(path)+" job "+jobKey.Value+" has composite action, skipped")
					confidence = bundle.ConfidenceMedium
					continue
				}
			}
			if strat := findMappingValue(jobVal, "strategy"); strat != nil {
				if mat := findMappingValue(strat, "matrix"); mat != nil {
					gaps = append(gaps, "workflow "+filepath.Base(path)+" job "+jobKey.Value+" has matrix, using first combination")
					confidence = bundle.ConfidenceMedium
				}
			}
			if svc := findMappingValue(jobVal, "services"); svc != nil {
				gaps = append(gaps, "workflow "+filepath.Base(path)+" job "+jobKey.Value+" has services, not fully supported")
			}
			steps := findMappingValue(jobVal, "steps")
			if steps == nil || steps.Kind != yaml.SequenceNode {
				continue
			}
			for _, step := range steps.Content {
				runNode := findMappingValue(step, "run")
				if runNode == nil {
					continue
				}
				run := strings.TrimSpace(runNode.Value)
				lower := strings.ToLower(run)
				// Heuristic: does run contain test/build/typecheck keywords?
				source := relSource(repoRoot, path) + "#L" + itoa(runNode.Line)
				switch {
				case containsAny(lower, []string{"test", "vitest", "jest", "pytest", "go test", "cargo test"}):
					if _, ok := commands["test"]; !ok {
						commands["test"] = bundle.Command{Cmd: run, Source: source}
						found = true
					}
				case containsAny(lower, []string{"build"}):
					if _, ok := commands["build"]; !ok {
						commands["build"] = bundle.Command{Cmd: run, Source: source}
						found = true
					}
				case strings.Contains(lower, "tsc") && strings.Contains(lower, "noemit"):
					if _, ok := commands["typecheck"]; !ok {
						commands["typecheck"] = bundle.Command{Cmd: run, Source: source}
						found = true
					}
				}
			}
		}
	}

	if !found {
		return bundle.ProjectProfile{}, false
	}
	// Language inferred as unknown here; heuristic will fill if needed, but for actions we mark as detected.
	profile := bundle.ProjectProfile{
		Language:   []string{},
		Commands:   commands,
		Confidence: confidence,
		Gaps:       gaps,
	}
	// Package manager not known from actions alone; leave nil and let downstream fill if needed.
	return profile, true
}

func findMappingValue(node *yaml.Node, key string) *yaml.Node {
	if node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i < len(node.Content); i += 2 {
		k := node.Content[i]
		v := node.Content[i+1]
		if k.Value == key {
			return v
		}
	}
	return nil
}

func hasKey(node *yaml.Node, key string) bool { return findMappingValue(node, key) != nil }

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func relSource(repoRoot, abs string) string {
	rel, err := filepath.Rel(repoRoot, abs)
	if err != nil {
		return abs
	}
	return rel
}

func itoa(i int) string { return strconv.Itoa(i) }
