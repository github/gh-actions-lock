// Package lockfile handles parsing and modifying workflow YAML files for action
// dependency pinning. It extracts uses: action references, manages the
// dependencies: section, and parses action.yml metadata for composite actions.
package lockfile

// TODO seems like we're duplicating stuff that lives in the pkg/lockfile
// TODO split out per type maybe this file is big and complicated & we shoudl consider exporting these in pkg/lockfile
// TODO for the workflow type we should be clear that it is only internal, that should not be somethign someoen can use because it's partial.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

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

// ActionRef represents a parsed uses: reference to a repository action.
type ActionRef struct {
	Owner string // e.g. "actions"
	Repo  string // e.g. "checkout"
	Path  string // e.g. "save" for actions/cache/save@v4
	Ref   string // e.g. "v4" - tag, branch, or full SHA
	Raw   string // original uses: string
}

// NWO returns owner/repo (Name With Owner).
func (a ActionRef) NWO() string {
	if a.Owner == "" && a.Repo == "" {
		return ""
	}
	return a.Owner + "/" + a.Repo
}

// FullName returns owner/repo or owner/repo/path.
func (a ActionRef) FullName() string {
	if a.Path != "" {
		return a.Owner + "/" + a.Repo + "/" + a.Path
	}
	return a.Owner + "/" + a.Repo
}

// Dependency represents a pinned dependency entry in the dependencies: section.
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
	parts := strings.SplitN(d.NWO, "/", 3)
	if len(parts) < 2 {
		return "", ""
	}
	return parts[0], parts[1]
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

// ParseDependencyString parses a dependency entry string back into a
// Dependency.
func ParseDependencyString(s string) (Dependency, error) {
	if containsControlChars(s) {
		return Dependency{}, fmt.Errorf("dependency string contains control characters")
	}

	colonIdx := strings.LastIndex(s, ":")
	if colonIdx < 0 {
		return Dependency{}, fmt.Errorf("invalid dependency format (missing : separator): %q", s)
	}

	nwoRefPart := s[:colonIdx]
	algoHashPart := s[colonIdx+1:]

	nwoRef := strings.SplitN(nwoRefPart, "@", 2)
	if len(nwoRef) != 2 || nwoRef[0] == "" || nwoRef[1] == "" {
		return Dependency{}, fmt.Errorf("invalid dependency nwo@ref: %q", nwoRefPart)
	}

	pathParts := strings.SplitN(nwoRef[0], "/", 3)
	if len(pathParts) < 2 || pathParts[0] == "" || pathParts[1] == "" {
		return Dependency{}, fmt.Errorf("invalid dependency owner/repo: %q", nwoRef[0])
	}
	if len(pathParts) == 3 {
		// Sub-action paths are not part of the lockfile pin grammar — the
		// runner downloads at repo+sha granularity. Reject hand-edited
		// entries that include a path component.
		return Dependency{}, fmt.Errorf("dependency key must be owner/repo@ref (no sub-path): %q", nwoRef[0])
	}

	dashIdx := strings.Index(algoHashPart, "-")
	if dashIdx <= 0 || dashIdx == len(algoHashPart)-1 {
		return Dependency{}, fmt.Errorf("invalid dependency hash format: %q", algoHashPart)
	}

	algo := strings.ToLower(algoHashPart[:dashIdx])
	sha := strings.ToLower(algoHashPart[dashIdx+1:])
	if algo != "sha1" && algo != "sha256" {
		return Dependency{}, fmt.Errorf("unsupported hash algorithm %q", algo)
	}
	if !isHexString(sha) {
		return Dependency{}, fmt.Errorf("invalid %s hash: %q", algo, sha)
	}
	if (algo == "sha1" && len(sha) != 40) || (algo == "sha256" && len(sha) != 64) {
		return Dependency{}, fmt.Errorf("invalid %s hash length: %d", algo, len(sha))
	}

	return Dependency{
		NWO:      pathParts[0] + "/" + pathParts[1],
		Ref:      nwoRef[1],
		SHA:      sha,
		HashAlgo: algo,
	}, nil
}

func containsControlChars(s string) bool {
	for _, c := range s {
		if c <= 0x1F || c == 0x7F {
			return true
		}
	}
	return false
}

func detectHashAlgo(hash string) string {
	if len(hash) == 64 {
		return "sha256"
	}
	return "sha1"
}

// IsFullSHA reports whether s looks like a full commit hash.
func IsFullSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	return isHexString(s)
}

func isHexString(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
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
	PeelTagObject(owner, repo, sha string) (commit string, ok bool)
}

// CheckSHARefMismatches inspects resolved dependencies for refs that look
// like full SHAs but resolved to different commit OIDs. Commit-OID pins and
// annotated-tag-object pins are content-addressed and immutable — those are
// honored as legitimate even when ref != sha. A nil peeler reverts to the
// strict comparison; pass a non-nil peeler in network-connected paths so
// tag-object pins (the immutable-release pattern) don't false-positive.
func CheckSHARefMismatches(deps []Dependency, peeler TagObjectPeeler) []SHARefMismatch {
	var mismatches []SHARefMismatch
	for _, dep := range deps {
		if !IsFullSHA(dep.Ref) || strings.EqualFold(dep.Ref, dep.SHA) {
			continue
		}
		if peeler != nil {
			owner, repo := dep.OwnerRepo()
			if commit, ok := peeler.PeelTagObject(owner, repo, dep.Ref); ok && strings.EqualFold(commit, dep.SHA) {
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

// ParseActionRef parses a uses: string into an ActionRef. Returns nil for
// expressions, local paths, docker images, reusable workflows, malformed
// owner/repo segments, or any input containing control characters that
// would otherwise reach downstream URL/GraphQL builders.
func ParseActionRef(uses string) *ActionRef {
	uses = strings.TrimSpace(uses)

	if uses == "" || containsControlChars(uses) {
		return nil
	}
	if strings.HasPrefix(uses, "./") {
		return nil
	}
	if strings.HasPrefix(uses, "docker://") {
		return nil
	}
	if strings.Contains(uses, "${") {
		return nil
	}

	atParts := strings.SplitN(uses, "@", 2)
	if len(atParts) != 2 || atParts[1] == "" {
		return nil
	}
	ref := atParts[1]

	segments := strings.SplitN(atParts[0], "/", 3)
	if len(segments) < 2 || segments[0] == "" || segments[1] == "" {
		return nil
	}
	if !isValidOwnerOrRepo(segments[0]) || !isValidOwnerOrRepo(segments[1]) {
		return nil
	}

	actionRef := &ActionRef{
		Owner: segments[0],
		Repo:  segments[1],
		Ref:   ref,
		Raw:   uses,
	}
	if len(segments) == 3 {
		actionRef.Path = segments[2]
	}

	if isReusableWorkflow(actionRef) {
		return nil
	}

	return actionRef
}

// isValidOwnerOrRepo enforces the GitHub owner/repo character set. GitHub
// allows alphanumerics, hyphens, underscores, and periods; we reject
// anything else to keep these values safe for use in URL paths and GraphQL
// string literals without per-call escaping bugs.
func isValidOwnerOrRepo(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return false
		}
	}
	return true
}

func isYAMLFile(path string) bool {
	return strings.HasSuffix(path, ".yml") || strings.HasSuffix(path, ".yaml")
}

func isReusableWorkflow(actionRef *ActionRef) bool {
	if actionRef.Path == "" {
		return false
	}
	// Anchor on prefix: a reusable workflow is keyed by
	// `<owner>/<repo>/.github/workflows/<name>.yml`, so the relative
	// Path must START with `.github/workflows/`. Substring matching
	// would misclassify composite actions whose nested folder
	// happens to contain that segment (e.g. `tools/.github/workflows/`).
	if !strings.HasPrefix(actionRef.Path, ".github/workflows/") {
		return false
	}
	return isYAMLFile(actionRef.Path)
}

func isLocalReusableWorkflow(localPath string) bool {
	return isYAMLFile(localPath)
}

// ExecutionType describes how an action runs.
type ExecutionType string

const (
	ExecNode      ExecutionType = "node"
	ExecDocker    ExecutionType = "docker"
	ExecComposite ExecutionType = "composite"
	ExecUnknown   ExecutionType = "unknown"
)

// ActionMeta is the parsed subset of action.yml relevant to dependency resolution.
type ActionMeta struct {
	Name       string
	Execution  ExecutionType
	NestedUses []string
}

// ParseActionMeta parses an action.yml content string.
func ParseActionMeta(content string) (*ActionMeta, error) {
	var raw struct {
		Name string `yaml:"name"`
		Runs struct {
			Using string `yaml:"using"`
			Steps []struct {
				Uses string `yaml:"uses"`
			} `yaml:"steps"`
		} `yaml:"runs"`
	}

	if err := yaml.Unmarshal([]byte(content), &raw); err != nil {
		return nil, fmt.Errorf("parsing action.yml: %w", err)
	}

	meta := &ActionMeta{Name: raw.Name}

	using := strings.ToLower(raw.Runs.Using)
	switch {
	case using == "composite":
		meta.Execution = ExecComposite
		for _, step := range raw.Runs.Steps {
			if step.Uses != "" {
				meta.NestedUses = append(meta.NestedUses, step.Uses)
			}
		}
	case using == "docker":
		meta.Execution = ExecDocker
	case strings.HasPrefix(using, "node"):
		meta.Execution = ExecNode
	default:
		meta.Execution = ExecUnknown
	}

	return meta, nil
}

// File represents a parsed workflow file with its raw content.
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

// ExtractActionRefs finds all uses: references to repository actions in the workflow.
func (f *File) ExtractActionRefs() ([]ActionRef, []string, []string) {
	var refs []ActionRef
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
			if isLocalReusableWorkflow(value) {
				return
			}
			if !seenLocal[value] {
				seenLocal[value] = true
				localPaths = append(localPaths, value)
			}
			return
		}
		actionRef := ParseActionRef(value)
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
func (f *File) RewriteActionRefs(replacements map[string]string) ([]byte, int, error) {
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

func docNode(root *yaml.Node) *yaml.Node {
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		return root.Content[0]
	}
	if root.Kind == yaml.MappingNode {
		return root
	}
	return nil
}

// ExtractLocalCompositeRefs reads action.yml files from local paths relative
// to the workflow file's directory and returns any repository action refs
// found in their steps.
func ExtractLocalCompositeRefs(workflowPath string, localPaths []string) ([]ActionRef, []string) {
	var refs []ActionRef
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
			actionRef := ParseActionRef(use)
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
