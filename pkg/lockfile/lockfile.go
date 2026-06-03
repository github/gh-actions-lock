package lockfile

import (
	"fmt"
	"regexp"
	"strconv"

	"gopkg.in/yaml.v3"
)

// ParseError describes a failure to parse a dependency lockfile.
//
// Line and Column, when non-zero, are the 1-indexed position within the
// lockfile contents that the failure refers to. They index the lockfile
// itself, never a consumer's workflow file, so callers can anchor diagnostics
// on the lockfile (.github/workflows/actions.lock) rather than scraping
// yaml.v3's error string themselves.
//
// Column is populated for semantic failures Parse detects itself (it walks the
// retained YAML node tree to the offending key/value). It is left zero for raw
// yaml.v3 decode failures, whose errors report only a line: a malformed
// document has no node tree to read a column from, and yaml.v3 type errors
// carry a line but no column.
//
// Msg is the human-readable reason with yaml.v3's "yaml:" package prefix and
// leading position removed.
type ParseError struct {
	Line   int
	Column int
	Msg    string
	err    error
}

func (e *ParseError) Error() string {
	switch {
	case e.Line > 0 && e.Column > 0:
		return fmt.Sprintf("line %d, column %d: %s", e.Line, e.Column, e.Msg)
	case e.Line > 0:
		return fmt.Sprintf("line %d: %s", e.Line, e.Msg)
	default:
		return e.Msg
	}
}

func (e *ParseError) Unwrap() error {
	return e.err
}

// yamlLinePattern matches the 1-indexed position gopkg.in/yaml.v3 embeds in its
// error messages: "yaml: line N: ..." for syntax errors, or "  line N: ..."
// within an "unmarshal errors:" block for type errors.
var yamlLinePattern = regexp.MustCompile(`line (\d+):`)

// leadingYAMLPosition matches yaml.v3's "yaml:" package prefix and any
// immediately following "line N:" position.
var leadingYAMLPosition = regexp.MustCompile(`^yaml: (line \d+: )?`)

// newYAMLParseError converts a gopkg.in/yaml.v3 error into a ParseError,
// lifting the line number out of the message so consumers receive it as
// structured data instead of having to scrape the string themselves.
func newYAMLParseError(err error) *ParseError {
	msg := err.Error()
	line := 0
	if m := yamlLinePattern.FindStringSubmatch(msg); m != nil {
		if n, convErr := strconv.Atoi(m[1]); convErr == nil {
			line = n
		}
	}
	return &ParseError{
		Line: line,
		Msg:  leadingYAMLPosition.ReplaceAllString(msg, ""),
		err:  err,
	}
}

// Version is the only supported lockfile schema version.
const Version = "v0.0.1"

// Path is the canonical repo-relative location of the dependency lockfile.
const Path = ".github/workflows/actions.lock"

// File is the parsed lockfile shape.
//
//	# .github/workflows/actions.lock
//	version: v0.0.1
//	workflows:
//	  .github/workflows/deploy.yml:
//	    - actions/checkout@v6:sha1-8e8c483db84b4bee98b60c0593521ed34d9990e8
//	dependencies:
//	  actions/checkout@v4.3.1:sha1-34e114876b0b11c390a56381ad16ebd13914f8d5:
//	    tag: v4.3.1
//	    branch: main
//	    commit: sha1-34e114876b0b11c390a56381ad16ebd13914f8d5
//	    owner_id: 44036562
//	    repo_id: 197814629
//	    uses:
//	      - actions/cache@v4.0.0:sha1-...
//
// The Go-side field name `Actions` corresponds to the YAML key `dependencies:`
// — the lockfile's deduplicated action DAG. Each entry's `uses:` list names
// the action's direct nested dependencies, reusing the same canonical pin
// keys. Workflow entries hold the full transitive closure as a flat list of
// pin keys for cold readability.
type File struct {
	Version   string              `yaml:"version"`
	Actions   map[string]Action   `yaml:"dependencies"`
	Workflows map[string][]string `yaml:"workflows"`

	// node retains the parsed YAML tree so callers can resolve positions for
	// their own diagnostics via Position/KeyPosition. It is nil on the
	// zero-value File returned alongside an error. yaml.v3 ignores this
	// unexported field during decoding.
	node *yaml.Node
}

// Position returns the 1-indexed line and column of the value node reached by
// following path as a sequence of mapping keys from the lockfile root (e.g.
// Position("version") points at the version value). ok is false when the path
// can't be resolved or no node tree was retained.
func (f File) Position(path ...string) (line, col int, ok bool) {
	v := f.valueNode(path)
	if v == nil {
		return 0, 0, false
	}
	return v.Line, v.Column, true
}

// KeyPosition is like Position but resolves the position of the final path
// segment's *key* node rather than its value. It is the right anchor for map
// entries whose key is the meaningful token (e.g. a dependency pin key or a
// workflow path under "workflows").
func (f File) KeyPosition(path ...string) (line, col int, ok bool) {
	if len(path) == 0 {
		return 0, 0, false
	}
	m := docMapping(f.node)
	for _, key := range path[:len(path)-1] {
		_, v := mappingEntry(m, key)
		if v == nil {
			return 0, 0, false
		}
		m = v
	}
	k, _ := mappingEntry(m, path[len(path)-1])
	if k == nil {
		return 0, 0, false
	}
	return k.Line, k.Column, true
}

// valueNode walks path from the lockfile root mapping, returning the value
// node of the final segment, or nil when any segment is missing.
func (f File) valueNode(path []string) *yaml.Node {
	m := docMapping(f.node)
	var v *yaml.Node
	for _, key := range path {
		_, v = mappingEntry(m, key)
		if v == nil {
			return nil
		}
		m = v
	}
	return v
}

// docMapping unwraps a document node to its top-level mapping, returning nil
// when n is not a mapping (or is absent).
func docMapping(n *yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	if n.Kind == yaml.DocumentNode {
		if len(n.Content) == 0 {
			return nil
		}
		n = n.Content[0]
	}
	if n.Kind != yaml.MappingNode {
		return nil
	}
	return n
}

// mappingEntry returns the key and value nodes for key within a mapping node,
// or (nil, nil) when m is not a mapping or the key is absent.
func mappingEntry(m *yaml.Node, key string) (k, v *yaml.Node) {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil, nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i], m.Content[i+1]
		}
	}
	return nil, nil
}

// LookupWorkflow returns the dependency closure for the given repo-relative
// workflow key (e.g. ".github/workflows/deploy.yml"). The returned bool
// reports whether the key was found.
func (f File) LookupWorkflow(workflowKey string) ([]string, bool) {
	w, ok := f.Workflows[workflowKey]
	return w, ok
}

// Action carries the per-action metadata recorded in the lockfile under the
// pin key.
//
// Tag is the discovered release/tag at the commit, if one exists. Optional.
//
// Branch is the branch that contains the pinned commit. Writers MUST refuse
// to record an Action without a Branch — a commit not on any branch is an
// impostor / fork-network signal. Readers tolerate absence for compatibility
// with lockfiles written before branch discovery was introduced.
//
// Commit holds the digest in algo-prefixed form (e.g. "sha1-..." or
// "sha256-...").
//
// Uses lists the action's direct nested dependencies (composite action
// `uses:` steps) as canonical pin keys. Empty for leaf actions.
type Action struct {
	Tag     string   `yaml:"tag,omitempty"`
	Branch  string   `yaml:"branch,omitempty"`
	Commit  string   `yaml:"commit,omitempty"`
	OwnerID int64    `yaml:"owner_id"`
	RepoID  int64    `yaml:"repo_id"`
	Uses    []string `yaml:"uses,omitempty"`
}

// Parse unmarshals YAML lockfile contents and verifies the version is
// supported. It does not validate the actions or workflows sections — that
// belongs to the consumer (e.g. gh-actions-pin's doctor command).
//
// Action map keys and workflow dependency entries are canonicalized via
// ParsePin so downstream lookups by canonical key (e.g. pin.String()) match
// regardless of the source casing of owner/repo/algo/hex in the YAML.
// Entries that do not parse as a valid pin are left untouched; consumers
// can flag them via diagnostics. Workflow path keys are NOT canonicalized
// — filesystem paths are case-sensitive on the platforms we run on.
func Parse(contents []byte) (File, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(contents, &root); err != nil {
		return File{}, newYAMLParseError(err)
	}
	var f File
	if err := root.Decode(&f); err != nil {
		return File{}, newYAMLParseError(err)
	}
	// Retain the tree so semantic errors below (and consumers) can resolve
	// precise line+column positions within the lockfile.
	f.node = &root

	if f.Version == "" {
		// No version node to point at; anchor at the top of the document.
		pe := &ParseError{Msg: "dependency lockfile version is required"}
		if m := docMapping(f.node); m != nil {
			pe.Line, pe.Column = m.Line, m.Column
		}
		return File{}, pe
	}
	if f.Version != Version {
		pe := &ParseError{Msg: fmt.Sprintf("unsupported dependency lockfile version %q", f.Version)}
		if l, c, ok := f.Position("version"); ok {
			pe.Line, pe.Column = l, c
		}
		return File{}, pe
	}
	if conflictKey, err := canonicalizeActions(&f); err != nil {
		pe := &ParseError{Msg: err.Error(), err: err}
		if l, c, ok := f.KeyPosition("dependencies", conflictKey); ok {
			pe.Line, pe.Column = l, c
		}
		return File{}, pe
	}
	canonicalizeWorkflowDependencies(&f)
	return f, nil
}

// canonicalizeActions rewrites the Actions map so every key is the
// canonical form of its pin (Pin.String()). A conflict between two
// different source casings of the same pin is a parse error — the file
// would be ambiguous about which Action metadata applies. On conflict it
// returns the offending source key so callers can locate it in the YAML tree.
func canonicalizeActions(f *File) (string, error) {
	if len(f.Actions) == 0 {
		return "", nil
	}
	out := make(map[string]Action, len(f.Actions))
	for key, action := range f.Actions {
		canonical := key
		if pin, ok := ParsePin(key); ok {
			canonical = pin.String()
		}
		// Canonicalize Uses entries too so cross-references resolve.
		if len(action.Uses) > 0 {
			canonUses := make([]string, len(action.Uses))
			for i, u := range action.Uses {
				if pin, ok := ParsePin(u); ok {
					canonUses[i] = pin.String()
				} else {
					canonUses[i] = u
				}
			}
			action.Uses = canonUses
		}
		if existing, dup := out[canonical]; dup {
			if !equalAction(existing, action) {
				return key, fmt.Errorf("duplicate action key %q after canonicalization with differing metadata", canonical)
			}
			continue
		}
		out[canonical] = action
	}
	f.Actions = out
	return "", nil
}

func equalAction(a, b Action) bool {
	if a.Tag != b.Tag || a.Branch != b.Branch || a.Commit != b.Commit ||
		a.OwnerID != b.OwnerID || a.RepoID != b.RepoID {
		return false
	}
	if len(a.Uses) != len(b.Uses) {
		return false
	}
	for i := range a.Uses {
		if a.Uses[i] != b.Uses[i] {
			return false
		}
	}
	return true
}

// canonicalizeWorkflowDependencies rewrites every workflow's pin list to
// canonical pin strings (Pin.String()) so lookups into the Actions map are
// casing-agnostic. Unparseable entries are preserved verbatim for downstream
// diagnostics to flag.
func canonicalizeWorkflowDependencies(f *File) {
	for path, deps := range f.Workflows {
		if len(deps) == 0 {
			continue
		}
		canonicalized := make([]string, len(deps))
		for i, dep := range deps {
			if pin, ok := ParsePin(dep); ok {
				canonicalized[i] = pin.String()
			} else {
				canonicalized[i] = dep
			}
		}
		f.Workflows[path] = canonicalized
	}
}
