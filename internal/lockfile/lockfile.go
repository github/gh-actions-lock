// Package lockfile owns gh-actions-pin's internal workflow-file representation
// and resolver-time Dependency intermediate. Schema-level parsing lives in the
// sibling pkg/lockfile package.
package lockfile

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	parserlock "github.com/github/gh-actions-pin/pkg/lockfile"
	"gopkg.in/yaml.v3"
)

// DiscoverWorkflows finds all workflow files in .github/workflows/ relative to
// the current directory. Returns an empty slice if the directory doesn't exist.
func DiscoverWorkflows() ([]string, error) {
	dir := filepath.Join(".github", "workflows")
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

// Dependency is the resolver's in-memory view of a single pinned action:
// the lockfile-grammar pin (NWO@Ref:Algo-SHA) plus the discovered Tag /
// Branch / sub-action Path that the lockfile-on-disk format does not
// carry. It is the working shape between `uses:` parsing, resolver
// traversal, and lockfile serialization — never persisted on disk and
// not part of any public API.
type Dependency struct {
	NWO string // owner/repo (no path)
	// Path is the optional sub-action subpath as written in `uses:`
	// (e.g. "save" for actions/cache/save). It is preserved on the
	// in-memory dep so resolver-time graph traversal can fetch the
	// correct sub-action.yml, but it is NOT part of the lockfile pin
	// identity (the runner downloads at repo+sha granularity) and is
	// dropped at serialization time. Distinct subpaths in the same
	// repo+ref collapse to one lockfile entry.
	Path     string
	Ref      string // resolved ref as given in uses:
	SHA      string // full commit hash
	HashAlgo string // "sha1" or "sha256"
	// Tag is the discovered release/tag pointing at SHA, if any. Optional.
	// Populated by the pin-time discovery pass; not read from `uses:`.
	Tag string
	// Branch is the discovered branch containing SHA. Required at write
	// time — a commit not on any branch is an impostor / fork-network
	// signal. Populated by the pin-time discovery pass.
	Branch string
}

// FullName returns owner/repo or owner/repo/path. Used for human-facing
// display and for resolver-internal graph traversal where distinct
// subpaths must be treated as distinct nodes.
func (d Dependency) FullName() string {
	if d.Path != "" {
		return d.NWO + "/" + d.Path
	}
	return d.NWO
}

// Key returns the dependency key for deduplication: NWO@Ref. This matches
// what the runner downloads (one tarball per repo+ref) so distinct
// subpaths in the same repo+ref collapse to a single lockfile entry.
// Resolver-internal graph code that needs to differentiate subpaths uses
// FullName()+"@"+Ref directly.
func (d Dependency) Key() string {
	return d.NWO + "@" + d.Ref
}

// OwnerRepo splits NWO into owner and repo components.
func (d Dependency) OwnerRepo() (string, string) {
	owner, repo, _ := SplitNWO(d.NWO)
	return owner, repo
}

// HashAlgoOrDetect returns the hash algorithm, falling back to detection from SHA length.
func (d Dependency) HashAlgoOrDetect() string {
	if d.HashAlgo != "" {
		return strings.ToLower(d.HashAlgo)
	}
	return detectHashAlgo(d.SHA)
}

// String formats the dependency as a YAML list entry using the canonical
// (lowercased) pin form produced by the parser package.
func (d Dependency) String() string {
	pin, err := dependencyToPin(d)
	if err != nil {
		return fmt.Sprintf("%s@%s:%s-%s", d.NWO, d.Ref, d.HashAlgoOrDetect(), d.SHA)
	}
	return pin.String()
}

func detectHashAlgo(hash string) string {
	if len(hash) == 64 {
		return "sha256"
	}
	return "sha1"
}

// SHARefMismatch describes a dependency whose uses: ref looks like a full SHA
// but resolved to a different commit OID via a mutable path (e.g. a branch
// named after a SHA — the only forgery shape worth flagging).
type SHARefMismatch struct {
	Dep        Dependency
	ResolvedAs string
}

// TagObjectPeeler can dereference a 40-hex SHA into the commit it points at
// when the SHA is itself an annotated tag object. *resolver.Resolver
// satisfies this; tests pass a stub. A nil peeler disables tag-object
// recognition and reverts to the strict EqualFold(ref, sha) check.
type TagObjectPeeler interface {
	PeelTagObject(ctx context.Context, owner, repo, sha string) (commit string, ok bool)
}

// CheckSHARefMismatches inspects resolved dependencies for refs that look
// like full SHAs but resolved to different commit OIDs. Commit-OID pins and
// annotated-tag-object pins are content-addressed and immutable — those are
// honored as legitimate even when ref != sha. A nil peeler reverts to the
// strict comparison; pass a non-nil peeler in network-connected paths so
// tag-object pins (the immutable-release pattern) don't false-positive.
func CheckSHARefMismatches(ctx context.Context, deps []Dependency, peeler TagObjectPeeler) []SHARefMismatch {
	var mismatches []SHARefMismatch
	for _, dep := range deps {
		if !parserlock.IsFullSha(dep.Ref) || strings.EqualFold(dep.Ref, dep.SHA) {
			continue
		}
		if peeler != nil {
			owner, repo := dep.OwnerRepo()
			if commit, ok := peeler.PeelTagObject(ctx, owner, repo, dep.Ref); ok && strings.EqualFold(commit, dep.SHA) {
				continue
			}
		}
		mismatches = append(mismatches, SHARefMismatch{
			Dep:        dep,
			ResolvedAs: dep.SHA,
		})
	}
	return mismatches
}

// WorkflowFile is the parsed workflow YAML the CLI rewrites in-place.
// It carries the original byte content alongside the parsed node tree so
// RewriteActionRefs can do anchored, comment-preserving substitution.
type WorkflowFile struct {
	Path    string
	Content []byte
	root    yaml.Node
}

// Load reads and parses a workflow file.
func Load(path string) (*WorkflowFile, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading workflow: %w", err)
	}

	return Parse(path, content)
}

// Parse builds a WorkflowFile from already-loaded workflow content.
func Parse(path string, content []byte) (*WorkflowFile, error) {
	f := &WorkflowFile{
		Path:    path,
		Content: content,
	}
	if err := yaml.Unmarshal(content, &f.root); err != nil {
		return nil, fmt.Errorf("parsing workflow YAML: %w", err)
	}

	return f, nil
}

// ExtractActionRefs finds all uses: references to repository actions in the workflow.
func (f *WorkflowFile) ExtractActionRefs() ([]parserlock.ActionRef, []string, []string) {
	var refs []parserlock.ActionRef
	var warnings []string
	var localPaths []string
	seen := make(map[string]bool)
	seenLocal := make(map[string]bool)

	walkYAML(&f.root, func(key, value string) {
		if key != "uses" {
			return
		}
		value = strings.TrimSpace(value)
		if strings.Contains(value, "${") {
			warnings = append(warnings, fmt.Sprintf("can't pin expression-based uses: %s", value))
			return
		}
		if strings.HasPrefix(value, "./") {
			if parserlock.IsLocalReusableWorkflow(value) {
				return
			}
			if !seenLocal[value] {
				seenLocal[value] = true
				localPaths = append(localPaths, value)
			}
			return
		}
		actionRef := parserlock.ParseActionRef(value)
		if actionRef != nil {
			dedupKey := actionRef.FullName() + "@" + actionRef.Ref
			if !seen[dedupKey] {
				seen[dedupKey] = true
				refs = append(refs, *actionRef)
			}
		}
	})

	return refs, localPaths, warnings
}

// RewriteActionRefs rewrites targeted uses: refs in the original workflow
// content while preserving the surrounding formatting and comments.
func (f *WorkflowFile) RewriteActionRefs(replacements map[string]string) ([]byte, int, error) {
	if len(replacements) == 0 {
		return append([]byte(nil), f.Content...), 0, nil
	}

	lines := strings.Split(string(f.Content), "\n")
	changed := 0

	walkYAMLNodes(&f.root, func(keyNode, valueNode *yaml.Node) {
		if keyNode == nil || valueNode == nil || keyNode.Value != "uses" || valueNode.Kind != yaml.ScalarNode {
			return
		}
		// Skip aliases / nodes that came from anchors. Replacing one
		// anchor reference would silently change every other use site.
		if valueNode.Alias != nil || valueNode.Anchor != "" {
			return
		}

		oldValue := strings.TrimSpace(valueNode.Value)
		newValue, ok := replacements[oldValue]
		if !ok || newValue == "" || newValue == oldValue {
			return
		}

		// Anchor the rewrite at the YAML node's reported (line, column)
		// rather than scanning the line for the first occurrence of
		// oldValue. The previous strings.Index(...) approach would
		// happily substitute matching text inside a YAML comment that
		// preceded the value (e.g. `uses: foo/bar@v1  # bumped from foo/bar@v1`).
		lineIndex := valueNode.Line - 1
		colIndex := valueNode.Column - 1
		if lineIndex < 0 || lineIndex >= len(lines) || colIndex < 0 {
			return
		}
		line := lines[lineIndex]
		if colIndex+len(oldValue) > len(line) {
			return
		}
		// Quoted scalars report Column at the opening quote; the actual
		// value sits one byte further in.
		if valueNode.Style == yaml.SingleQuotedStyle || valueNode.Style == yaml.DoubleQuotedStyle {
			if colIndex+1+len(oldValue) > len(line) {
				return
			}
			if line[colIndex+1:colIndex+1+len(oldValue)] != oldValue {
				return
			}
			lines[lineIndex] = line[:colIndex+1] + newValue + line[colIndex+1+len(oldValue):]
			changed++
			return
		}
		if line[colIndex:colIndex+len(oldValue)] != oldValue {
			return
		}
		lines[lineIndex] = line[:colIndex] + newValue + line[colIndex+len(oldValue):]
		changed++
	})

	return []byte(strings.Join(lines, "\n")), changed, nil
}

func walkYAML(node *yaml.Node, fn func(key, value string)) {
	walkYAMLNodes(node, func(keyNode, valueNode *yaml.Node) {
		if keyNode.Kind == yaml.ScalarNode && valueNode.Kind == yaml.ScalarNode {
			fn(keyNode.Value, valueNode.Value)
		}
	})
}

// maxYAMLWalkDepth bounds recursion in walkYAMLNodes so a hostile or
// pathological workflow tree cannot stack-overflow the parser. yaml.v3 has
// its own document-parse limit; this is the post-parse-tree walker's own
// safety net.
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
		// Defense against `uses: ./../../etc/foo` style traversal: the
		// resolved directory must remain inside the discovered repo root.
		// Marginal blast radius (we only read action.yml + extract uses
		// strings) but the value here is an attacker-controlled string
		// from workflow YAML, so we treat it as untrusted.
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

// isWithinRoot reports whether candidate resolves to a path inside root
// (or root itself). Both inputs are cleaned and made absolute before
// comparison so that `..` traversal and symlink-free relative paths are
// caught uniformly.
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
