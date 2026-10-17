package workflowfile

import (
	"fmt"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ValidateLocalActionMigrations checks caller checkout context for the files
// being migrated. workflowPaths must include every workflow in the repository:
// shared composite actions can also be called by workflows outside a scoped run.
func ValidateLocalActionMigrations(workflowPaths, migrationPaths []string) error {
	migrations := make(map[string]bool, len(migrationPaths))
	for _, path := range migrationPaths {
		absPath, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		resolvedPath, err := filepath.EvalSymlinks(absPath)
		if err != nil {
			return err
		}
		migrations[resolvedPath] = true
	}
	for _, path := range workflowPaths {
		absPath, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		root, err := findRepoRoot(absPath)
		if err != nil {
			return err
		}
		if err := ValidatePathWithinRoot(root, absPath); err != nil {
			return err
		}
		resolvedPath, err := filepath.EvalSymlinks(absPath)
		if err != nil {
			return err
		}
		f, err := Load(resolvedPath)
		if err != nil {
			return err
		}
		v := checkoutValidator{repoRoot: root, migrations: migrations, files: make(map[string]*File)}
		if err := v.validate(f, v.replacements(f)); err != nil {
			return fmt.Errorf("checking checkout context in %s: %w", path, err)
		}
	}
	return nil
}

type checkoutRef struct {
	path string
	node *yaml.Node
}

type checkoutValidator struct {
	repoRoot   string
	migrations map[string]bool // nil means all local action references are candidates
	files      map[string]*File
	active     map[checkoutContext]bool
	results    map[checkoutContext]*checkoutRef
}

type checkoutContext struct {
	steps *yaml.Node
	ref   *yaml.Node
}

func (f *File) validateCheckoutRefsForMigration(replacements map[string]string) error {
	root, err := findRepoRoot(f.Path)
	if err != nil {
		return err
	}
	v := checkoutValidator{repoRoot: root, files: make(map[string]*File)}
	return v.validate(f, replacements)
}

func (v *checkoutValidator) replacements(f *File) map[string]string {
	if v.migrations != nil && !v.migrations[f.Path] {
		return nil
	}
	return f.localActionReplacements(v.repoRoot)
}

func (v *checkoutValidator) validate(f *File, replacements map[string]string) error {
	root := resolveAlias(documentRoot(&f.root))
	if _, err := v.steps(f, mapValue(root, "runs"), replacements, nil, 0); err != nil {
		return err
	}
	jobs := mapValue(root, "jobs")
	if jobs == nil || jobs.Kind != yaml.MappingNode {
		return nil
	}
	for i := 1; i < len(jobs.Content); i += 2 {
		if _, err := v.steps(f, resolveAlias(jobs.Content[i]), replacements, nil, 0); err != nil {
			return err
		}
	}
	return nil
}

func (v *checkoutValidator) steps(f *File, owner *yaml.Node, replacements map[string]string, customRef *checkoutRef, depth int) (*checkoutRef, error) {
	if depth > maxYAMLWalkDepth {
		return nil, fmt.Errorf("checking checkout context in %s: composite action nesting exceeds %d", f.Path, maxYAMLWalkDepth)
	}
	steps := mapValue(owner, "steps")
	if steps == nil || steps.Kind != yaml.SequenceNode {
		return customRef, nil
	}
	context := checkoutContext{steps: steps}
	if customRef != nil {
		context.ref = customRef.node
	}
	if result, ok := v.results[context]; ok {
		return result, nil
	}
	if v.active[context] {
		return nil, fmt.Errorf("checking checkout context in %s: composite action cycle", f.Path)
	}
	if v.active == nil {
		v.active = make(map[checkoutContext]bool)
		v.results = make(map[checkoutContext]*checkoutRef)
	}
	v.active[context] = true
	defer delete(v.active, context)
	for _, stepNode := range steps.Content {
		step := resolveAlias(stepNode)
		uses := strings.TrimSpace(scalarValue(mapValue(step, "uses")))
		action, _, hasRef := strings.Cut(uses, "@")
		if hasRef && strings.EqualFold(action, "actions/checkout") {
			ref := mapValue(mapValue(step, "with"), "ref")
			value := strings.TrimSpace(scalarValue(ref))
			if value != "" && !isRunningCommitRef(value) {
				customRef = &checkoutRef{path: f.Path, node: ref}
			}
			// Keep earlier custom refs: a later checkout can be conditional.
		}
		if replacement, ok := replacements[uses]; ok && customRef != nil {
			return nil, fmt.Errorf("cannot migrate %s to %s in %s: preceding actions/checkout uses ref %q at %s:%d; $/ resolves to the running commit, not the checked-out ref; use --no-migrate-local-actions to keep local references", uses, replacement, f.Path, customRef.node.Value, customRef.path, customRef.node.Line)
		}
		var localPath string
		switch {
		case IsSelfRepositoryAction(uses):
			localPath = "./" + strings.TrimPrefix(uses, selfRepositoryPrefix)
		case strings.HasPrefix(uses, "./"):
			localPath = uses
		default:
			continue
		}
		if v.repoRoot == "" || !localActionExists(v.repoRoot, localPath) {
			continue
		}
		actionDir := filepath.Join(v.repoRoot, strings.TrimPrefix(localPath, "./"))
		child, ok := v.files[actionDir]
		if !ok {
			content, path, err := readActionFile(v.repoRoot, actionDir)
			if err != nil {
				return nil, err
			}
			path, err = filepath.EvalSymlinks(path)
			if err != nil {
				return nil, err
			}
			child, err = Parse(path, content)
			if err != nil {
				return nil, err
			}
			v.files[actionDir] = child
		}
		// A composite inherits the caller's checkout state and can change the
		// workspace seen by subsequent steps, even if its invocation is conditional.
		runs := mapValue(documentRoot(&child.root), "runs")
		nextRef, err := v.steps(child, runs, v.replacements(child), customRef, depth+1)
		if err != nil {
			return nil, err
		}
		customRef = nextRef
	}
	v.results[context] = customRef
	return customRef, nil
}

func isRunningCommitRef(value string) bool {
	if !strings.HasPrefix(value, "${{") || !strings.HasSuffix(value, "}}") {
		return false
	}
	return strings.TrimSpace(value[3:len(value)-2]) == "github.sha"
}
