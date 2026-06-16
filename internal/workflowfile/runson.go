package workflowfile

import (
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// hostedRunnerLabels is a temporary restriction — only workflows running
// entirely on these labels are eligible for lockfile onboarding.
var hostedRunnerLabels = map[string]bool{
	// Linux x64
	"ubuntu-latest": true,
	"ubuntu-22.04":  true,
	"ubuntu-24.04":  true,
	"ubuntu-26.04":  true,

	// Linux ARM64
	"ubuntu-22.04-arm": true,
	"ubuntu-24.04-arm": true,
	"ubuntu-26.04-arm": true,

	// Linux slim
	"ubuntu-slim": true,

	// Linux firewall (feature-flagged)
	"ubuntu-24.04-firewall": true,
	"ubuntu-latest-firewall": true,

	// Windows x64
	"windows-latest":       true,
	"windows-2022":         true,
	"windows-2025":         true,
	"windows-2025-vs2026":  true,

	// Windows ARM64
	"windows-11-arm":          true,
	"windows-11-vs2026-arm":   true,

	// macOS (Apple Silicon / arm64)
	"macos-latest": true,
	"macos-14":     true,
	"macos-15":     true,
	"macos-26":     true,

	// macOS Intel (x64)
	"macos-15-intel": true,
	"macos-26-intel": true,

	// macOS large (Intel xl)
	"macos-14-large":     true,
	"macos-15-large":     true,
	"macos-26-large":     true,
	"macos-latest-large": true,

	// macOS xlarge (Apple Silicon xl)
	"macos-14-xlarge":     true,
	"macos-15-xlarge":     true,
	"macos-26-xlarge":     true,
	"macos-latest-xlarge": true,
}

// orgHostedLabels holds additional labels registered at runtime from the
// org's hosted-runners API. Protected by orgHostedMu for safe concurrent access.
var (
	orgHostedMu     sync.RWMutex
	orgHostedLabels map[string]bool
)

// RegisterOrgHostedLabels records additional runner labels as hosted (from
// the /orgs/{org}/actions/hosted-runners API). Call this before ParseAll so
// workflows using org-provisioned larger runners are not flagged as
// self-hosted. Safe to call multiple times; labels accumulate.
func RegisterOrgHostedLabels(labels []string) {
	if len(labels) == 0 {
		return
	}
	orgHostedMu.Lock()
	defer orgHostedMu.Unlock()
	if orgHostedLabels == nil {
		orgHostedLabels = make(map[string]bool, len(labels))
	}
	for _, l := range labels {
		orgHostedLabels[strings.ToLower(l)] = true
	}
}

// IsHostedRunnerLabel reports whether label is a known GitHub-hosted
// runner label. Checks both the built-in set and any org-registered labels.
func IsHostedRunnerLabel(label string) bool {
	lower := strings.ToLower(label)
	if hostedRunnerLabels[lower] {
		return true
	}
	orgHostedMu.RLock()
	defer orgHostedMu.RUnlock()
	return orgHostedLabels[lower]
}

// ExtractRunsOnLabels returns the set of runs-on labels across all jobs
// in the workflow. Each distinct label appears once (case-preserved).
// Expression-based labels (containing "${") are returned as-is.
func (f *File) ExtractRunsOnLabels() []string {
	seen := make(map[string]bool)
	var labels []string

	walkJobs(&f.root, func(runsOnNode *yaml.Node) {
		for _, label := range runsOnLabels(runsOnNode) {
			lower := strings.ToLower(label)
			if !seen[lower] {
				seen[lower] = true
				labels = append(labels, label)
			}
		}
	})

	return labels
}

// HasNonHostedRunnerLabels reports whether any job in the workflow uses a
// runs-on label that is not a known GitHub-hosted runner. Returns false
// when the workflow has no jobs or no runs-on keys (run-only workflows
// have no action refs to pin, so they are excluded separately).
func (f *File) HasNonHostedRunnerLabels() bool {
	return len(f.NonHostedRunnerLabels()) > 0
}

// NonHostedRunnerLabels returns the distinct non-hosted runner labels
// found across all jobs. Expression labels (containing "${") are
// returned as-is. The result is deduplicated case-insensitively but
// preserves original casing.
func (f *File) NonHostedRunnerLabels() []string {
	seen := make(map[string]bool)
	var labels []string

	walkJobs(&f.root, func(runsOnNode *yaml.Node) {
		for _, label := range runsOnLabels(runsOnNode) {
			lower := strings.ToLower(label)
			if seen[lower] {
				continue
			}
			if strings.Contains(label, "${") || !IsHostedRunnerLabel(label) {
				seen[lower] = true
				labels = append(labels, label)
			}
		}
	})
	return labels
}

// walkJobs iterates over the top-level `jobs:` mapping and calls fn with
// each job's `runs-on` value node. Jobs without runs-on are skipped.
func walkJobs(root *yaml.Node, fn func(runsOnNode *yaml.Node)) {
	if root == nil || root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return
	}
	doc := root.Content[0]
	if doc.Kind != yaml.MappingNode {
		return
	}

	// Find the "jobs" key.
	var jobsNode *yaml.Node
	for i := 0; i < len(doc.Content)-1; i += 2 {
		if doc.Content[i].Kind == yaml.ScalarNode && doc.Content[i].Value == "jobs" {
			jobsNode = doc.Content[i+1]
			break
		}
	}
	if jobsNode == nil || jobsNode.Kind != yaml.MappingNode {
		return
	}

	// Iterate jobs.
	for i := 0; i < len(jobsNode.Content)-1; i += 2 {
		jobVal := jobsNode.Content[i+1]
		if jobVal.Kind != yaml.MappingNode {
			continue
		}
		for j := 0; j < len(jobVal.Content)-1; j += 2 {
			key := jobVal.Content[j]
			if key.Kind == yaml.ScalarNode && key.Value == "runs-on" {
				fn(jobVal.Content[j+1])
				break
			}
		}
	}
}

// runsOnLabels extracts the label strings from a runs-on YAML node.
// Handles scalar ("ubuntu-latest"), sequence (["self-hosted", "linux"]),
// and mapping ({ group: "...", labels: [...] }) forms.
func runsOnLabels(node *yaml.Node) []string {
	if node == nil {
		return nil
	}
	switch node.Kind {
	case yaml.ScalarNode:
		return []string{strings.TrimSpace(node.Value)}
	case yaml.SequenceNode:
		var out []string
		for _, child := range node.Content {
			if child.Kind == yaml.ScalarNode {
				out = append(out, strings.TrimSpace(child.Value))
			}
		}
		return out
	case yaml.MappingNode:
		// { group: "...", labels: [...] } form — extract from labels key,
		// and include the group value as a non-hosted label.
		var out []string
		for i := 0; i < len(node.Content)-1; i += 2 {
			key := node.Content[i]
			val := node.Content[i+1]
			if key.Kind != yaml.ScalarNode {
				continue
			}
			switch key.Value {
			case "labels":
				out = append(out, runsOnLabels(val)...)
			case "group":
				if val.Kind == yaml.ScalarNode && val.Value != "" {
					out = append(out, val.Value)
				}
			}
		}
		return out
	}
	return nil
}
