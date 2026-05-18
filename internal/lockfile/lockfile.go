// Package lockfile handles parsing and modifying workflow YAML files for action
// dependency pinning. It extracts uses: action references, manages the
// dependencies: section, and parses action.yml metadata for composite actions.
package lockfile

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
	NWO      string // owner/repo or owner/repo/path
	Ref      string // resolved ref as given in uses:
	SHA      string // full commit hash
	HashAlgo string // "sha1" or "sha256"
}

// Key returns the dependency key for deduplication.
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

// String formats the dependency as a YAML list entry.
func (d Dependency) String() string {
	return fmt.Sprintf("github.com/%s@%s:%s-%s", d.NWO, d.Ref, d.HashAlgoOrDetect(), strings.ToLower(d.SHA))
}

// ParseDependencyString parses a dependency entry string back into a Dependency.
func ParseDependencyString(s string) (Dependency, error) {
	const prefix = "github.com/"
	if !strings.HasPrefix(strings.ToLower(s), prefix) {
		return Dependency{}, fmt.Errorf("invalid dependency format (expected github.com/ prefix): %q", s)
	}
	s = s[len(prefix):]

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
		NWO:      nwoRef[0],
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
// but resolved to a different commit OID.
type SHARefMismatch struct {
	Dep        Dependency
	ResolvedAs string
}

// CheckSHARefMismatches inspects resolved dependencies for refs that look like
// full SHAs but resolved to different OIDs.
func CheckSHARefMismatches(deps []Dependency) []SHARefMismatch {
	var mismatches []SHARefMismatch
	for _, dep := range deps {
		if IsFullSHA(dep.Ref) && !strings.EqualFold(dep.Ref, dep.SHA) {
			mismatches = append(mismatches, SHARefMismatch{
				Dep:        dep,
				ResolvedAs: dep.SHA,
			})
		}
	}
	return mismatches
}

// ParseActionRef parses a uses: string into an ActionRef.
func ParseActionRef(uses string) *ActionRef {
	uses = strings.TrimSpace(uses)

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
	if len(segments) < 2 {
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

func isYAMLFile(path string) bool {
	return strings.HasSuffix(path, ".yml") || strings.HasSuffix(path, ".yaml")
}

func isReusableWorkflow(actionRef *ActionRef) bool {
	if actionRef.Path == "" {
		return false
	}
	if !strings.Contains(actionRef.Path, ".github/workflows/") {
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

// ReadDependencies extracts the current dependencies: section from the workflow.
// Exact duplicate entries (same key and SHA) are silently deduped.
// Conflicting duplicates (same key, different SHA) produce an error.
func (f *File) ReadDependencies() ([]Dependency, error) {
	var deps []Dependency
	seen := make(map[string]Dependency)

	doc := docNode(&f.root)
	if doc == nil {
		return nil, nil
	}

	for i := 0; i < len(doc.Content)-1; i += 2 {
		if doc.Content[i].Value != "dependencies" {
			continue
		}
		seq := doc.Content[i+1]
		if seq.Kind != yaml.SequenceNode {
			return nil, fmt.Errorf("dependencies: must be a sequence")
		}
		for _, item := range seq.Content {
			if containsControlChars(item.Value) {
				return nil, fmt.Errorf("dependency entry contains control characters (possible injection)")
			}
			dep, err := ParseDependencyString(item.Value)
			if err != nil {
				return nil, fmt.Errorf("parsing dependency entry: %w", err)
			}
			if prev, exists := seen[dep.Key()]; exists {
				if !strings.EqualFold(prev.SHA, dep.SHA) || prev.HashAlgo != dep.HashAlgo {
					return nil, fmt.Errorf("conflicting dependency entries for %s", dep.Key())
				}
				// Exact duplicate — skip silently.
				continue
			}
			seen[dep.Key()] = dep
			deps = append(deps, dep)
		}
		return deps, nil
	}

	return nil, nil
}

// WriteDependencies returns the workflow content with an updated dependencies: section.
// When parentMap is non-nil, transitive dependencies are grouped by the parent
// composite action that brings them in — the same dep may appear under multiple
// parents. The parser silently deduplicates exact duplicate entries.
func (f *File) WriteDependencies(deps []Dependency, parentMap map[string][]string) ([]byte, error) {
	content := string(f.Content)
	directRefs, _, _ := f.ExtractActionRefs()
	directKeys := make(map[string]bool, len(directRefs))
	for _, ref := range directRefs {
		directKeys[ref.FullName()+"@"+ref.Ref] = true
	}

	directDepsByKey := make(map[string]Dependency)
	var transitiveDeps []Dependency
	for _, dep := range deps {
		key := dep.Key()
		if directKeys[key] {
			directDepsByKey[key] = dep
			continue
		}
		if _, exists := directDepsByKey[key]; exists {
			continue
		}
		transitiveDeps = append(transitiveDeps, dep)
	}

	// Dedup transitive deps list.
	seenTransitive := make(map[string]bool)
	var dedupedTransitive []Dependency
	for _, dep := range transitiveDeps {
		if !seenTransitive[dep.Key()] {
			seenTransitive[dep.Key()] = true
			dedupedTransitive = append(dedupedTransitive, dep)
		}
	}
	transitiveDeps = dedupedTransitive

	directDeps := make([]Dependency, 0, len(directDepsByKey))
	for _, dep := range directDepsByKey {
		directDeps = append(directDeps, dep)
	}

	sort.Slice(directDeps, func(i, j int) bool {
		return directDeps[i].String() < directDeps[j].String()
	})

	var sb strings.Builder
	sb.WriteString("\n# Automatically generated and managed by gh-actions-pin\n")
	sb.WriteString("dependencies:\n")
	for _, dep := range directDeps {
		sb.WriteString("  - " + dep.String() + "\n")
	}

	if len(transitiveDeps) > 0 {
		writeTransitiveDeps(&sb, transitiveDeps, directDeps, parentMap)
	}

	content = removeDependenciesSection(content)
	content = strings.TrimRight(content, "\n") + "\n"
	content += sb.String()

	return []byte(content), nil
}

// writeTransitiveDeps writes the transitive dependencies section, grouped by
// parent composite action when parentMap is available.
func writeTransitiveDeps(sb *strings.Builder, transitiveDeps []Dependency, directDeps []Dependency, parentMap map[string][]string) {
	if len(directDeps) > 0 {
		sb.WriteString("\n")
	}

	// If no parent map or all transitive deps are unmapped, use flat format.
	if len(parentMap) == 0 {
		sb.WriteString("  # Transitive dependencies\n")
		sort.Slice(transitiveDeps, func(i, j int) bool {
			return transitiveDeps[i].String() < transitiveDeps[j].String()
		})
		for _, dep := range transitiveDeps {
			sb.WriteString("  - " + dep.String() + "\n")
		}
		return
	}

	// Group transitive deps by parent. A dep can appear under multiple parents.
	type parentGroup struct {
		parentKey string
		deps      []Dependency
	}
	groupMap := make(map[string]*parentGroup)
	var groupOrder []string
	var orphans []Dependency

	for _, dep := range transitiveDeps {
		parents, ok := parentMap[dep.Key()]
		if !ok || len(parents) == 0 {
			orphans = append(orphans, dep)
			continue
		}
		for _, pKey := range parents {
			g, exists := groupMap[pKey]
			if !exists {
				g = &parentGroup{parentKey: pKey}
				groupMap[pKey] = g
				groupOrder = append(groupOrder, pKey)
			}
			g.deps = append(g.deps, dep)
		}
	}

	sort.Strings(groupOrder)

	for i, pKey := range groupOrder {
		if i > 0 {
			sb.WriteString("\n")
		}
		g := groupMap[pKey]
		sort.Slice(g.deps, func(a, b int) bool {
			return g.deps[a].String() < g.deps[b].String()
		})
		sb.WriteString("  # Transitive dependencies (via " + pKey + ")\n")
		for _, dep := range g.deps {
			sb.WriteString("  - " + dep.String() + "\n")
		}
	}

	if len(orphans) > 0 {
		if len(groupOrder) > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString("  # Transitive dependencies\n")
		sort.Slice(orphans, func(i, j int) bool {
			return orphans[i].String() < orphans[j].String()
		})
		for _, dep := range orphans {
			sb.WriteString("  - " + dep.String() + "\n")
		}
	}
}

// reTransitiveVia matches the "# Transitive dependencies (via <dep-key>)" comment.
var reTransitiveVia = regexp.MustCompile(`#\s*Transitive dependencies\s*\(via\s+(.+?)\)`)

// ReadParentMap extracts the transitive dependency parent mapping from the
// dependencies: section comments. It parses "# Transitive dependencies (via X)"
// lines and associates subsequent dependency entries with the parent key.
// Returns child dep key → parent dep keys.
func (f *File) ReadParentMap() map[string][]string {
	parentMap := make(map[string][]string)
	lines := strings.Split(string(f.Content), "\n")

	inDeps := false
	var currentParent string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Detect start of dependencies section.
		if trimmed == "dependencies:" {
			inDeps = true
			continue
		}
		if !inDeps {
			continue
		}

		// Exit dependencies section on non-indented, non-empty line.
		if trimmed != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			break
		}

		// Check for transitive group comment.
		if m := reTransitiveVia.FindStringSubmatch(trimmed); len(m) == 2 {
			currentParent = m[1]
			continue
		}

		// Plain "# Transitive dependencies" comment (no via) clears parent.
		if strings.Contains(trimmed, "# Transitive dependencies") && !strings.Contains(trimmed, "(via") {
			currentParent = ""
			continue
		}

		// Parse dependency entry.
		if strings.HasPrefix(trimmed, "- ") {
			depStr := strings.TrimPrefix(trimmed, "- ")
			dep, err := ParseDependencyString(depStr)
			if err != nil {
				continue
			}
			if currentParent != "" {
				parentMap[dep.Key()] = appendUnique(parentMap[dep.Key()], currentParent)
			}
		}
	}

	return parentMap
}

func appendUnique(slice []string, s string) []string {
	for _, v := range slice {
		if v == s {
			return slice
		}
	}
	return append(slice, s)
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

		oldValue := strings.TrimSpace(valueNode.Value)
		newValue, ok := replacements[oldValue]
		if !ok || newValue == "" || newValue == oldValue {
			return
		}

		lineIndex := valueNode.Line - 1
		if lineIndex >= 0 && lineIndex < len(lines) {
			if idx := strings.Index(lines[lineIndex], oldValue); idx >= 0 {
				// Replace only the matched ref, preserving any trailing content.
				lines[lineIndex] = lines[lineIndex][:idx] + newValue + lines[lineIndex][idx+len(oldValue):]
				changed++
			}
		}
	})

	return []byte(strings.Join(lines, "\n")), changed, nil
}

var (
	reDepsSectionWithComment = regexp.MustCompile(`(?ms)^\n?# Automatically generated and managed by[^\n]*\ndependencies:\n(?:  (?:#.*|- .*)\n|\n)*`)
	reDepsSectionBare        = regexp.MustCompile(`(?ms)^dependencies:\n(?:  (?:#.*|- .*)\n|\n)*`)
)

func removeDependenciesSection(content string) string {
	content = reDepsSectionWithComment.ReplaceAllString(content, "")
	content = reDepsSectionBare.ReplaceAllString(content, "")
	return content
}

func walkYAML(node *yaml.Node, fn func(key, value string)) {
	walkYAMLNodes(node, func(keyNode, valueNode *yaml.Node) {
		if keyNode.Kind == yaml.ScalarNode && valueNode.Kind == yaml.ScalarNode {
			fn(keyNode.Value, valueNode.Value)
		}
	})
}

func walkYAMLNodes(node *yaml.Node, fn func(keyNode, valueNode *yaml.Node)) {
	if node == nil {
		return
	}
	switch node.Kind {
	case yaml.DocumentNode:
		for _, child := range node.Content {
			walkYAMLNodes(child, fn)
		}
	case yaml.MappingNode:
		for i := 0; i < len(node.Content)-1; i += 2 {
			key := node.Content[i]
			val := node.Content[i+1]
			fn(key, val)
			walkYAMLNodes(val, fn)
		}
	case yaml.SequenceNode:
		for _, child := range node.Content {
			walkYAMLNodes(child, fn)
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
