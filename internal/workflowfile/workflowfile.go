// Package workflowfile owns the parsed workflow YAML representation: loading,
// extraction of action refs, local composite discovery, and comment-preserving
// rewriting. It intentionally has no dependency on the lockfile or resolver
// packages.
package workflowfile

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"gopkg.in/yaml.v3"
)

// File is the parsed workflow YAML the CLI rewrites in-place.
// It carries the original byte content alongside the parsed node tree so
// RewriteActionRefs can do anchored, comment-preserving substitution.
type File struct {
	Path    string
	Content []byte
	root    yaml.Node
}

// Load reads and parses a workflow file.
func Load(path string) (*File, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading workflow: %w", err)
	}

	return Parse(path, content)
}

// Parse builds a File from already-loaded workflow content.
func Parse(path string, content []byte) (*File, error) {
	f := &File{
		Path:    path,
		Content: content,
	}
	if err := yaml.Unmarshal(content, &f.root); err != nil {
		return nil, fmt.Errorf("parsing workflow YAML: %w", err)
	}

	return f, nil
}

// RefScan is the classified result of walking a workflow's `uses:` values.
type RefScan struct {
	// Refs are remote repository action references (owner/repo[/path]@ref).
	Refs []parserlock.ActionRef
	// LocalPaths are `./…` local composite action references (reusable
	// workflows are excluded — they resolve differently).
	LocalPaths []string
	// SelfRepoRefs are valid `$/…` self-referencing actions. Inherently
	// pinned: they resolve against the defining repo at the running ref.
	SelfRepoRefs []string
	// SelfRepoRefErrs are malformed `$/…@ref` values — the invalid form.
	SelfRepoRefErrs []string
	// Warnings are non-fatal parse notes (e.g. expression-based uses:).
	Warnings []string
}

// ExtractActionRefs finds and classifies all uses: references in the workflow.
func (f *File) ExtractActionRefs() RefScan {
	var scan RefScan
	seen := make(map[string]bool)
	seenLocal := make(map[string]bool)
	seenSelf := make(map[string]bool)

	walkYAML(&f.root, func(key, value string) {
		if key != "uses" {
			return
		}
		value = strings.TrimSpace(value)
		if strings.Contains(value, "${") {
			scan.Warnings = append(scan.Warnings, fmt.Sprintf("skipping unparseable uses: value %q (expressions are not supported)", value))
			return
		}
		if IsSelfRepoAction(value) {
			if seenSelf[value] {
				return
			}
			seenSelf[value] = true
			// Bare `$/…` is a legal self-repo reference at both step level
			// (action dir) and job level (reusable-workflow file): "this repo
			// at the running SHA", inherently pinned. Only the `@ref` form is
			// invalid — a self-reference has no external ref to pin.
			if SelfRepoRefHasVersion(value) {
				scan.SelfRepoRefErrs = append(scan.SelfRepoRefErrs, value)
			} else {
				scan.SelfRepoRefs = append(scan.SelfRepoRefs, value)
			}
			return
		}
		if strings.HasPrefix(value, "./") {
			if parserlock.IsLocalReusableWorkflow(value) {
				return
			}
			if !seenLocal[value] {
				seenLocal[value] = true
				scan.LocalPaths = append(scan.LocalPaths, value)
			}
			return
		}
		actionRef := parserlock.ParseActionRef(value)
		if actionRef != nil {
			dedupKey := actionRef.FullName() + "@" + actionRef.Ref
			if !seen[dedupKey] {
				seen[dedupKey] = true
				scan.Refs = append(scan.Refs, *actionRef)
			}
		}
	})

	return scan
}

// DiscoverWorkflows finds all workflow files in .github/workflows/ relative to
// the current directory. Returns nil if the directory doesn't exist.
func DiscoverWorkflows() ([]string, error) {
	return DiscoverWorkflowsIn(filepath.Join(".github", "workflows"))
}

// DiscoverWorkflowsIn finds all workflow files (*.yml, *.yaml) in dir.
// Returns nil if the directory doesn't exist.
func DiscoverWorkflowsIn(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", dir, err)
	}

	var paths []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := filepath.Ext(entry.Name())
		if ext == ".yml" || ext == ".yaml" {
			paths = append(paths, filepath.Join(dir, entry.Name()))
		}
	}
	sort.Strings(paths)
	return paths, nil
}

// FindRepoRoot returns the git repository root containing startPath, or "" when
// startPath is not inside a git repository.
func FindRepoRoot(startPath string) string {
	return findRepoRoot(startPath)
}

// DiscoverCompositeActionFiles walks the repository rooted at root and returns
// the paths of all action definition files (action.yml / action.yaml). The
// .git directory is skipped. Non-composite action files are included; callers
// migrate them with MigrateLocalActionsToSelfRepo, which no-ops when a file has
// no local `./…` steps.
func DiscoverCompositeActionFiles(root string) ([]string, error) {
	if root == "" {
		return nil, nil
	}
	var paths []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == "action.yml" || d.Name() == "action.yaml" {
			paths = append(paths, p)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("discovering action files under %s: %w", root, err)
	}
	sort.Strings(paths)
	return paths, nil
}

// ExtractLocalCompositeRefs reads action.yml files from local paths relative
// to the workflow file's directory and returns any repository action refs
// found in their steps.
func ExtractLocalCompositeRefs(workflowPath string, localPaths []string) ([]parserlock.ActionRef, []string) {
	var refs []parserlock.ActionRef
	var warnings []string
	seen := make(map[string]bool)

	repoRoot := findRepoRoot(workflowPath)
	if repoRoot == "" {
		if len(localPaths) > 0 {
			warnings = append(warnings, "can't resolve local action paths: not in a git repository")
		}
		return nil, warnings
	}

	for _, localPath := range localPaths {
		relPath := strings.TrimPrefix(localPath, "./")
		actionDir := filepath.Join(repoRoot, relPath)
		if !isWithinRoot(repoRoot, actionDir) {
			warnings = append(warnings, fmt.Sprintf("refusing to read action file outside repo root: %s", localPath))
			continue
		}

		actionContent, err := os.ReadFile(filepath.Join(actionDir, "action.yml"))
		if err != nil {
			actionContent, err = os.ReadFile(filepath.Join(actionDir, "action.yaml"))
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("can't read action file for %s: %v", localPath, err))
				continue
			}
		}

		uses, parseErr := parseActionYAMLForUses(actionContent)
		if parseErr != nil {
			warnings = append(warnings, fmt.Sprintf("can't parse action file for %s: %v", localPath, parseErr))
			continue
		}

		for _, use := range uses {
			actionRef := parserlock.ParseActionRef(use)
			if actionRef != nil {
				dedupKey := actionRef.FullName() + "@" + actionRef.Ref
				if !seen[dedupKey] {
					seen[dedupKey] = true
					refs = append(refs, *actionRef)
				}
			}
		}
	}

	return refs, warnings
}

func walkYAML(node *yaml.Node, fn func(key, value string)) {
	walkYAMLNodes(node, func(keyNode, valueNode *yaml.Node) {
		if keyNode.Kind == yaml.ScalarNode && valueNode.Kind == yaml.ScalarNode {
			fn(keyNode.Value, valueNode.Value)
		}
	})
}

// maxYAMLWalkDepth bounds recursion in walkYAMLNodes so a hostile or
// pathological workflow tree cannot stack-overflow the parser.
const maxYAMLWalkDepth = 100

func walkYAMLNodes(node *yaml.Node, fn func(keyNode, valueNode *yaml.Node)) {
	walkYAMLNodesDepth(node, fn, 0)
}

func walkYAMLNodesDepth(node *yaml.Node, fn func(keyNode, valueNode *yaml.Node), depth int) {
	if node == nil || depth > maxYAMLWalkDepth {
		return
	}
	switch node.Kind {
	case yaml.DocumentNode:
		for _, child := range node.Content {
			walkYAMLNodesDepth(child, fn, depth+1)
		}
	case yaml.MappingNode:
		for i := 0; i < len(node.Content)-1; i += 2 {
			key := node.Content[i]
			val := node.Content[i+1]
			fn(key, val)
			walkYAMLNodesDepth(val, fn, depth+1)
		}
	case yaml.SequenceNode:
		for _, child := range node.Content {
			walkYAMLNodesDepth(child, fn, depth+1)
		}
	}
}

func findRepoRoot(startPath string) string {
	absPath, err := filepath.Abs(filepath.Dir(startPath))
	if err != nil {
		return ""
	}
	for {
		if _, err := os.Stat(filepath.Join(absPath, ".git")); err == nil {
			return absPath
		}
		parent := filepath.Dir(absPath)
		if parent == absPath {
			return ""
		}
		absPath = parent
	}
}

func isWithinRoot(root, candidate string) bool {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	absCandidate, err := filepath.Abs(candidate)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absRoot, absCandidate)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".."
}

func parseActionYAMLForUses(content []byte) ([]string, error) {
	var action struct {
		Runs struct {
			Using string `yaml:"using"`
			Steps []struct {
				Uses string `yaml:"uses"`
			} `yaml:"steps"`
		} `yaml:"runs"`
	}
	if err := yaml.Unmarshal(content, &action); err != nil {
		return nil, err
	}
	if action.Runs.Using != "composite" {
		return nil, nil
	}

	var uses []string
	for _, step := range action.Runs.Steps {
		if step.Uses != "" {
			uses = append(uses, step.Uses)
		}
	}
	return uses, nil
}

// KeyFromPath converts a workflow path discovered on disk (relative to the
// repo root or cwd) into the repo-relative key used inside the lockfile.
func KeyFromPath(workflowPath string) string {
	cleaned := filepath.ToSlash(filepath.Clean(workflowPath))
	cleaned = strings.TrimPrefix(cleaned, "./")
	return cleaned
}
