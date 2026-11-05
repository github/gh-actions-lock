// Package workflowfile owns the parsed workflow YAML representation: loading,
// extraction of action refs, local composite discovery, and comment-preserving
// rewriting. It intentionally has no dependency on the lockfile or resolver
// packages.
package workflowfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/uses"
	"gopkg.in/yaml.v3"
)

var errPathOutsideRoot = errors.New("path resolves outside repository root")

// SyntaxError reports content that isn't valid YAML, as opposed to a file
// that couldn't be read or a document that parsed but was rejected.
type SyntaxError struct {
	Err error
}

func (e *SyntaxError) Error() string { return "parsing workflow YAML: " + e.Err.Error() }

func (e *SyntaxError) Unwrap() error { return e.Err }

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
		return nil, &SyntaxError{Err: err}
	}

	var invalid []string
	walkUses(&f.root, func(value string, _ bool) {
		if isUsesExpression(value) {
			invalid = append(invalid, strings.TrimSpace(value))
		}
	})
	if len(invalid) > 0 {
		return nil, usesExpressionErr(invalid)
	}

	return f, nil
}

// isUsesExpression reports whether a `uses:` value contains an expression.
// Malformed `$/…@ref` values are excluded so the more specific self
// repository diagnostic still wins.
func isUsesExpression(value string) bool {
	value = strings.TrimSpace(value)
	return strings.Contains(value, "${") && !SelfRepositoryRefHasVersion(value)
}

// usesExpressionErr reports an expression in `uses:`. GitHub Actions rejects
// the workflow outright at both step and job level (no context is in scope
// there), so parsing fails rather than skipping the line.
func usesExpressionErr(values []string) error {
	return fmt.Errorf("`uses:` can't contain an expression: %s", strings.Join(values, ", "))
}

// RefScan is the classified result of walking a workflow's `uses:` values.
type RefScan struct {
	// Refs are remote repository action references (owner/repo[/path]@ref).
	Refs []parserlock.ActionRef
	// LocalPaths are `./…` local composite action references (reusable
	// workflows are excluded — they resolve differently).
	LocalPaths []string
	// SelfRepositoryRefs are valid `$/…` self repository actions. Inherently
	// pinned: they resolve against the defining repo at the running ref.
	SelfRepositoryRefs []string
	// SelfRepositoryActionRefs are the step-level subset that must be scanned
	// locally for remote dependencies. Job-level refs point at reusable
	// workflows, which the workflow discovery pass scans independently.
	SelfRepositoryActionRefs []string
	// SelfRepositoryRefErrs are malformed `$/…@ref` values — the invalid form.
	SelfRepositoryRefErrs []string
	// MalformedRefs are the errors for `uses:` values the Actions backend
	// rejects (uses.Invalid). They are never skipped.
	MalformedRefs []string
}

// ExtractActionRefs finds and classifies all uses: references in the workflow.
func (f *File) ExtractActionRefs() RefScan {
	var scan RefScan
	seen := make(map[string]bool)
	seenLocal := make(map[string]bool)
	seenSelf := make(map[string]bool)
	seenSelfAction := make(map[string]bool)

	walkUses(&f.root, func(value string, stepLevel bool) {
		value = strings.TrimSpace(value)
		if SelfRepositoryRefHasVersion(value) {
			if !seenSelf[value] {
				seenSelf[value] = true
				scan.SelfRepositoryRefErrs = append(scan.SelfRepositoryRefErrs, value)
			}
			return
		}
		if IsSelfRepositoryAction(value) {
			// Bare `$/…` is a legal self repository reference at both step level
			// (action dir) and job level (reusable-workflow file): "this repo
			// at the running SHA", inherently pinned. Only the `@ref` form is
			// invalid — a self repository reference has no external ref to pin.
			if !seenSelf[value] {
				seenSelf[value] = true
				scan.SelfRepositoryRefs = append(scan.SelfRepositoryRefs, value)
			}
			if stepLevel && !seenSelfAction[value] {
				seenSelfAction[value] = true
				scan.SelfRepositoryActionRefs = append(scan.SelfRepositoryActionRefs, value)
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
		pos := uses.Job
		if stepLevel {
			pos = uses.Step
		}
		switch c := uses.Classify(value, pos); c.Kind {
		case uses.Invalid:
			if !seen[value] {
				seen[value] = true
				scan.MalformedRefs = append(scan.MalformedRefs, c.Err().Error())
			}
		case uses.Action:
			dedupKey := c.Action.FullName() + "@" + c.Action.Ref
			if !seen[dedupKey] {
				seen[dedupKey] = true
				scan.Refs = append(scan.Refs, c.Action)
			}
		}
	})

	return scan
}

// SelfRepositoryActionScan is the transitive dependency scan of in-repo
// actions reached from step-level `$/…` references.
type SelfRepositoryActionScan struct {
	Refs                  []parserlock.ActionRef
	SelfRepositoryRefs    []string
	SelfRepositoryRefErrs []string
	// LocalUses are the `./…` uses inside the scanned actions, each with the
	// `$/…` action that contains it.
	LocalUses     []LocalUse
	MalformedRefs []string
	// ActionFiles are the in-repo action definition files visited, in visit
	// order. Refs in these files are rewritable source, unlike `$/…` itself.
	ActionFiles []string
	Errors      []string
	// Images are the mutable `docker://` steps in the visited actions.
	Images []uses.Image
}

// LocalUse is a `./…` use and the `$/…` action whose steps contain it.
type LocalUse struct {
	Path   string
	Parent string
}

// ScanSelfRepositoryActions recursively reads in-repo actions reached through
// step-level `$/…` references. The self repository actions themselves remain
// inherently pinned; only remote dependencies found inside them are returned
// in Refs.
func ScanSelfRepositoryActions(workflowPath string, actionRefs []string) SelfRepositoryActionScan {
	var scan SelfRepositoryActionScan
	if len(actionRefs) == 0 {
		return scan
	}

	repoRoot, err := findRepoRoot(workflowPath)
	if err != nil {
		scan.Errors = append(scan.Errors, fmt.Sprintf("can't inspect self repository actions: %v", err))
		return scan
	}
	if repoRoot == "" {
		scan.Errors = append(scan.Errors, "can't inspect self repository actions: not in a git repository (set "+RepoRootEnv+" to override)")
		return scan
	}

	type pendingAction struct {
		ref   string
		depth int
	}
	pending := make([]pendingAction, 0, len(actionRefs))
	for _, ref := range actionRefs {
		pending = append(pending, pendingAction{ref: strings.TrimSpace(ref)})
	}

	seenActions := make(map[string]bool)
	seenRefs := make(map[string]bool)
	seenSelf := make(map[string]bool)
	seenInvalid := make(map[string]bool)
	seenLocal := make(map[LocalUse]bool)

	for len(pending) > 0 {
		current := pending[0]
		pending = pending[1:]
		if seenActions[current.ref] {
			continue
		}
		seenActions[current.ref] = true

		if current.depth > maxYAMLWalkDepth {
			scan.Errors = append(scan.Errors, fmt.Sprintf("self repository action recursion exceeded max depth %d at %s", maxYAMLWalkDepth, current.ref))
			continue
		}
		if SelfRepositoryRefHasVersion(current.ref) {
			if !seenInvalid[current.ref] {
				seenInvalid[current.ref] = true
				scan.SelfRepositoryRefErrs = append(scan.SelfRepositoryRefErrs, current.ref)
			}
			continue
		}

		relPath := strings.TrimPrefix(current.ref, selfRepositoryPrefix)
		actionDir := filepath.Join(repoRoot, filepath.FromSlash(relPath))
		if !isWithinRoot(repoRoot, actionDir) {
			scan.Errors = append(scan.Errors, fmt.Sprintf("refusing to read self repository action outside repo root: %s", current.ref))
			continue
		}

		actionUses, actionPath, err := readActionUses(repoRoot, actionDir)
		if err != nil {
			scan.Errors = append(scan.Errors, fmt.Sprintf("can't inspect self repository action %s: %v", current.ref, err))
			continue
		}
		scan.ActionFiles = append(scan.ActionFiles, actionPath)
		for _, rawUse := range actionUses {
			use := strings.TrimSpace(rawUse)
			switch {
			case SelfRepositoryRefHasVersion(use):
				if !seenInvalid[use] {
					seenInvalid[use] = true
					scan.SelfRepositoryRefErrs = append(scan.SelfRepositoryRefErrs, use)
				}
			case IsSelfRepositoryAction(use):
				if !seenSelf[use] {
					seenSelf[use] = true
					scan.SelfRepositoryRefs = append(scan.SelfRepositoryRefs, use)
				}
				pending = append(pending, pendingAction{ref: use, depth: current.depth + 1})
			case strings.HasPrefix(use, "./"):
				if lu := (LocalUse{Path: use, Parent: current.ref}); !seenLocal[lu] {
					seenLocal[lu] = true
					scan.LocalUses = append(scan.LocalUses, lu)
				}
			case strings.HasPrefix(use, "docker://"):
				img, ok := uses.ClassifyImage(use)
				img.Where = current.ref
				if ok && !slices.Contains(scan.Images, img) {
					scan.Images = append(scan.Images, img)
				}
			default:
				switch c := uses.Classify(use, uses.Step); c.Kind {
				case uses.Invalid:
					if !seenRefs[use] {
						seenRefs[use] = true
						scan.MalformedRefs = append(scan.MalformedRefs, c.Err().Error())
					}
				case uses.Action:
					key := c.Action.FullName() + "@" + c.Action.Ref
					if !seenRefs[key] {
						seenRefs[key] = true
						scan.Refs = append(scan.Refs, c.Action)
					}
				}
			}
		}
	}

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

// FindRepoRoot returns the repository root containing startPath, or "" when
// startPath is not inside a git repository. RepoRootEnv overrides discovery;
// an unusable override is an error.
func FindRepoRoot(startPath string) (string, error) {
	return findRepoRoot(startPath)
}

// DiscoverCompositeActionFiles walks the repository rooted at root and returns
// the paths of all action definition files (action.yml / action.yaml). The
// .git directory is skipped. When root is itself a checkout, so is any
// subdirectory that is a separate checkout (worktree, submodule or nested
// clone); see inNestedCheckout. A RepoRootEnv root without .git may hold the
// real checkout below it, so nothing is skipped there. Non-composite action
// files are included; callers migrate them with
// MigrateLocalActionsToSelfRepository, which no-ops when a file has no local
// `./…` steps.
func DiscoverCompositeActionFiles(root string) ([]string, error) {
	if root == "" {
		return nil, nil
	}
	w := actionFileWalker{root: root, skipNested: isCheckout(root)}
	if err := w.walk(root); err != nil {
		return nil, fmt.Errorf("discovering action files under %s: %w", root, err)
	}
	sort.Strings(w.paths)
	return w.paths, nil
}

type actionFileWalker struct {
	root       string
	skipNested bool
	paths      []string
}

// walk reads each directory once and checks its listing for a .git entry,
// so detecting nested checkouts costs no extra syscall per directory.
func (w *actionFileWalker) walk(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if w.skipNested && dir != w.root && hasEntry(entries, ".git") && isCheckout(dir) {
		return nil
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		switch {
		case e.IsDir():
			if e.Name() == ".git" {
				continue
			}
			if err := w.walk(p); err != nil {
				return err
			}
		case e.Name() == "action.yml" || e.Name() == "action.yaml":
			if err := ValidatePathWithinRoot(w.root, p); err != nil {
				return err
			}
			info, err := os.Stat(p)
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("action definition %s is not a regular file", p)
			}
			// The walk already skipped nested checkouts, so only a symlinked
			// action file can still point into one.
			if w.skipNested && e.Type()&fs.ModeSymlink != 0 && inNestedCheckout(w.root, p) {
				continue
			}
			w.paths = append(w.paths, p)
		}
	}
	return nil
}

func hasEntry(entries []fs.DirEntry, name string) bool {
	return slices.ContainsFunc(entries, func(e fs.DirEntry) bool { return e.Name() == name })
}

// isCheckout reports whether dir is the top of a git checkout. A worktree or
// submodule has a .git file and a clone has a .git directory. Stat follows
// symlinks, so a dangling .git link does not count.
func isCheckout(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// inNestedCheckout reports whether file, after resolving symlinks, lies in a
// separate checkout below root. Resolving matters because a symlink outside
// the nested checkout can point into it. As in DiscoverCompositeActionFiles,
// this only applies when root is itself a checkout. Callers have already
// validated file with ValidatePathWithinRoot, so a resolution error here
// means the tree changed underneath us; treat it as nested to stay safe.
//
// EvalSymlinks doesn't resolve Windows junctions (Go reports them as
// ModeIrregular), so a junction on the path hides where it leads. Those
// count as nested too, at the cost of not migrating a junction that stays
// inside this checkout.
func inNestedCheckout(root, file string) bool {
	if !isCheckout(root) {
		return false
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return true
	}
	resolvedFile, err := filepath.EvalSymlinks(file)
	if err != nil {
		return true
	}
	rel, err := filepath.Rel(resolvedRoot, resolvedFile)
	if err != nil || !filepath.IsLocal(rel) {
		return true
	}
	for dir := filepath.Dir(rel); dir != "."; dir = filepath.Dir(dir) {
		p := filepath.Join(resolvedRoot, dir)
		if !isPlainDir(p) || isCheckout(p) {
			return true
		}
	}
	return false
}

// isPlainDir uses Lstat so a junction, which Stat would follow, doesn't count.
func isPlainDir(p string) bool {
	info, err := os.Lstat(p)
	return err == nil && info.IsDir()
}

// walkUses visits `uses:` values at the only places GitHub Actions honors
// them: `jobs.<id>.uses`, `jobs.<id>.steps[*].uses` in a workflow, and
// `runs.steps[*].uses` in a composite action definition. A `uses` key
// anywhere else (an env var, a `with:` input) is ordinary data, not a
// reference.
func walkUses(node *yaml.Node, fn func(value string, stepLevel bool)) {
	root := resolveAlias(documentRoot(node))
	walkStepUses(mapValue(root, "runs"), compositeSteps, fn)

	jobs := mapValue(root, "jobs")
	if jobs == nil || jobs.Kind != yaml.MappingNode {
		return
	}
	for i := 1; i < len(jobs.Content); i += 2 {
		job := resolveAlias(jobs.Content[i])
		if job == nil || job.Kind != yaml.MappingNode {
			continue
		}
		if use := scalarValue(mapValue(job, "uses")); use != "" {
			fn(use, false)
		}
		walkStepUses(job, jobSteps, fn)
	}
}

// stepsContext is where a `steps:` sequence appears.
type stepsContext int

const (
	// jobSteps is `jobs.<id>.steps`, which may contain `parallel:` groups.
	jobSteps stepsContext = iota
	// compositeSteps is `runs.steps`; composite actions don't support `parallel:`.
	compositeSteps
)

// walkStepUses visits the `uses:` of each step in owner's `steps:` sequence.
// A `parallel:` group is a flat list of steps; it is not walked recursively.
func walkStepUses(owner *yaml.Node, ctx stepsContext, fn func(value string, stepLevel bool)) {
	for _, step := range stepMappings(mapValue(owner, "steps")) {
		if use := scalarValue(mapValue(step, "uses")); use != "" {
			fn(use, true)
		}
		if ctx != jobSteps {
			continue
		}
		for _, inner := range stepMappings(mapValue(step, "parallel")) {
			if use := scalarValue(mapValue(inner, "uses")); use != "" {
				fn(use, true)
			}
		}
	}
}

func stepMappings(steps *yaml.Node) []*yaml.Node {
	if steps == nil || steps.Kind != yaml.SequenceNode {
		return nil
	}
	var out []*yaml.Node
	for _, node := range steps.Content {
		if step := resolveAlias(node); step != nil && step.Kind == yaml.MappingNode {
			out = append(out, step)
		}
	}
	return out
}

func documentRoot(node *yaml.Node) *yaml.Node {
	if node != nil && node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		return node.Content[0]
	}
	return node
}

// resolveAlias dereferences YAML anchors so an aliased job or step is walked
// like the node it points at.
func resolveAlias(node *yaml.Node) *yaml.Node {
	for i := 0; node != nil && node.Kind == yaml.AliasNode && i < maxYAMLWalkDepth; i++ {
		node = node.Alias
	}
	return node
}

func mapValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i < len(node.Content)-1; i += 2 {
		if node.Content[i].Kind == yaml.ScalarNode && node.Content[i].Value == key {
			return resolveAlias(node.Content[i+1])
		}
	}
	return nil
}

func scalarValue(node *yaml.Node) string {
	if node == nil || node.Kind != yaml.ScalarNode {
		return ""
	}
	return node.Value
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

// RepoRootEnv overrides `.git` discovery for callers relocking a partial
// checkout without `.git`. The workflow's directory must lie inside it.
const RepoRootEnv = "GH_ACTIONS_LOCK_REPO_ROOT"

// findRepoRoot returns the repository root containing startPath. It honours
// RepoRootEnv when set; otherwise it walks up for `.git` and returns "" with a
// nil error when none is found. A non-nil error means the override is unusable.
func findRepoRoot(startPath string) (string, error) {
	absPath, err := filepath.Abs(filepath.Dir(startPath))
	if err != nil {
		return "", nil
	}
	if root := os.Getenv(RepoRootEnv); root != "" {
		return overrideRepoRoot(root, absPath)
	}
	for {
		if isCheckout(absPath) {
			return absPath, nil
		}
		parent := filepath.Dir(absPath)
		if parent == absPath {
			return "", nil
		}
		absPath = parent
	}
}

func overrideRepoRoot(root, startDir string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("%s=%s: %w", RepoRootEnv, root, err)
	}
	info, err := os.Stat(absRoot)
	if err != nil {
		return "", fmt.Errorf("%s=%s: %w", RepoRootEnv, root, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s=%s: not a directory", RepoRootEnv, root)
	}
	if err := ValidatePathWithinRoot(absRoot, startDir); err != nil {
		return "", fmt.Errorf("%s=%s does not contain %s", RepoRootEnv, root, startDir)
	}
	return absRoot, nil
}

// ValidateInRepo rejects a path that resolves, through symlinks, outside the
// repository containing it. A path outside any repository, or one that does
// not exist yet, has nothing to escape.
func ValidateInRepo(path string) error {
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return nil
	}
	root, err := findRepoRoot(path)
	if err != nil || root == "" {
		return err
	}
	return ValidatePathWithinRoot(root, path)
}

// LocalActionRemoteUses returns the remote actions that the composite at the
// `./…` path localPath uses, read from the checkout containing workflowPath.
// It returns nil when the action can't be read there: the runner workspace
// may hold something else, so nothing is known about it.
func LocalActionRemoteUses(workflowPath, localPath string) []string {
	root, err := findRepoRoot(workflowPath)
	if err != nil || root == "" {
		return nil
	}
	actionDir := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(localPath, "./")))
	if !isWithinRoot(root, actionDir) {
		return nil
	}
	actionUses, _, err := readActionUses(root, actionDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, use := range actionUses {
		use = strings.TrimSpace(use)
		if uses.Classify(use, uses.Step).Kind == uses.Action && !slices.Contains(out, use) {
			out = append(out, use)
		}
	}
	return out
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

// ValidatePathWithinRoot resolves symlinks in candidate and rejects paths that
// leave root. Callers use it before reading or rewriting repository-owned YAML.
func ValidatePathWithinRoot(root, candidate string) error {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolving repository root %s: %w", root, err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return fmt.Errorf("resolving repository root %s: %w", root, err)
	}
	absCandidate, err := filepath.Abs(candidate)
	if err != nil {
		return fmt.Errorf("resolving repository path %s: %w", candidate, err)
	}
	resolvedCandidate, err := filepath.EvalSymlinks(absCandidate)
	if err != nil {
		return fmt.Errorf("resolving repository path %s: %w", candidate, err)
	}
	if !isWithinRoot(resolvedRoot, resolvedCandidate) {
		return fmt.Errorf("%w: %s is outside %s", errPathOutsideRoot, candidate, root)
	}
	return nil
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
	var invalid []string
	for _, step := range action.Runs.Steps {
		switch {
		case step.Uses == "":
		case isUsesExpression(step.Uses):
			invalid = append(invalid, strings.TrimSpace(step.Uses))
		default:
			uses = append(uses, step.Uses)
		}
	}
	if len(invalid) > 0 {
		return nil, usesExpressionErr(invalid)
	}
	return uses, nil
}

// readActionUses returns the `uses:` values of an in-repo action definition
// along with the file they were read from, so callers can rewrite that file.
func readActionUses(repoRoot, actionDir string) ([]string, string, error) {
	var lastErr error
	for _, name := range []string{"action.yml", "action.yaml"} {
		actionPath := filepath.Join(actionDir, name)
		if err := ValidatePathWithinRoot(repoRoot, actionPath); err != nil {
			if errors.Is(err, errPathOutsideRoot) {
				return nil, "", err
			}
			lastErr = err
			continue
		}
		content, err := os.ReadFile(actionPath)
		if err != nil {
			lastErr = err
			continue
		}
		uses, err := parseActionYAMLForUses(content)
		if err != nil {
			return nil, "", fmt.Errorf("parsing %s: %w", name, err)
		}
		return uses, actionPath, nil
	}
	return nil, "", fmt.Errorf("reading action.yml or action.yaml: %w", lastErr)
}

// KeyFromPath converts a workflow path discovered on disk (relative to the
// repo root or cwd) into the repo-relative key used inside the lockfile.
func KeyFromPath(workflowPath string) string {
	cleaned := filepath.ToSlash(filepath.Clean(workflowPath))
	cleaned = strings.TrimPrefix(cleaned, "./")
	return cleaned
}
