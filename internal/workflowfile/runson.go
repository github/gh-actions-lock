package workflowfile

import (
	"strings"

	"gopkg.in/yaml.v3"
)

// hostedRunnerPrefixes are the label prefixes used by GitHub-hosted runners.
// A runs-on label that starts with any of these (case-insensitive) is
// considered hosted. The list covers standard runners and larger runners.
var hostedRunnerPrefixes = []string{
	"ubuntu-",
	"macos-",
	"windows-",
}

// IsHostedRunnerLabel reports whether label is a known GitHub-hosted runner
// label. Labels are matched case-insensitively against the known hosted
// runner prefixes (ubuntu-*, macos-*, windows-*).
func IsHostedRunnerLabel(label string) bool {
	lower := strings.ToLower(label)
	for _, prefix := range hostedRunnerPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
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
// are not excluded by this check — they have no runner labels at all).
func (f *File) HasNonHostedRunnerLabels() bool {
	found := false
	walkJobs(&f.root, func(runsOnNode *yaml.Node) {
		for _, label := range runsOnLabels(runsOnNode) {
			if strings.Contains(label, "${") {
				// Expression labels are opaque; treat as non-hosted to be safe.
				found = true
				return
			}
			if !IsHostedRunnerLabel(label) {
				found = true
				return
			}
		}
	})
	return found
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
