package lockfile

import (
	"fmt"
	"sort"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"gopkg.in/yaml.v3"
)

// marshalDeterministic emits YAML with stable key ordering. Map keys in Go
// maps are randomized; for a lockfile we want byte-stable output across runs.
//
// All string keys and values whose content is user-supplied (pin strings,
// workflow paths, tag/branch/commit, version) are emitted single-quoted so
// the file round-trips identically regardless of YAML scalar-resolution
// quirks: pin keys carry colons, tags can look like floats ("1.0"), refs
// can collide with YAML 1.1 booleans ("y", "no", "on", "off"). Schema
// field names (version, actions, workflows, tag, branch, …) stay
// unquoted because they're hardcoded and trivially safe.
func marshalDeterministic(file parserlock.File) ([]byte, error) {
	root := &yaml.Node{Kind: yaml.MappingNode}
	addQuotedField(root, "version", file.Version)

	if len(file.Workflows) > 0 {
		keys := make([]string, 0, len(file.Workflows))
		for k := range file.Workflows {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		workflowsNode := &yaml.Node{Kind: yaml.MappingNode}
		for _, k := range keys {
			deps := append([]string(nil), file.Workflows[k]...)
			sort.Strings(deps)
			seq := &yaml.Node{Kind: yaml.SequenceNode}
			for _, d := range deps {
				seq.Content = append(seq.Content, quotedScalar(d))
			}
			workflowsNode.Content = append(workflowsNode.Content,
				quotedScalar(k),
				seq,
			)
		}
		root.Content = append(root.Content,
			plainScalar("workflows"),
			workflowsNode,
		)
	}

	if len(file.Dependencies) > 0 {
		keys := make([]string, 0, len(file.Dependencies))
		for k := range file.Dependencies {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		depsNode := &yaml.Node{Kind: yaml.MappingNode}
		for _, k := range keys {
			a := file.Dependencies[k]
			entry := &yaml.Node{Kind: yaml.MappingNode}
			if a.Tag != "" {
				addQuotedField(entry, "tag", a.Tag)
			}
			if a.Branch != "" {
				addQuotedField(entry, "branch", a.Branch)
			}
			if a.Commit != "" {
				addQuotedField(entry, "commit", a.Commit)
			}
			addIntField(entry, "owner_id", a.OwnerID)
			addIntField(entry, "repo_id", a.RepoID)
			if len(a.Uses) > 0 {
				uses := append([]string(nil), a.Uses...)
				sort.Strings(uses)
				usesSeq := &yaml.Node{Kind: yaml.SequenceNode}
				for _, u := range uses {
					usesSeq.Content = append(usesSeq.Content, quotedScalar(u))
				}
				entry.Content = append(entry.Content,
					plainScalar("uses"),
					usesSeq,
				)
			}
			depsNode.Content = append(depsNode.Content,
				quotedScalar(k),
				entry,
			)
		}
		root.Content = append(root.Content,
			plainScalar("dependencies"),
			depsNode,
		)
	}

	doc := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}}
	return yaml.Marshal(doc)
}

// plainScalar is for hardcoded schema field names (version, actions, …).
func plainScalar(name string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Value: name}
}

// quotedScalar single-quotes user-supplied string values so YAML
// auto-typing can never reinterpret them as numbers, booleans, dates, etc.
func quotedScalar(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Style: yaml.SingleQuotedStyle, Value: value}
}

func addQuotedField(parent *yaml.Node, key, value string) {
	parent.Content = append(parent.Content,
		plainScalar(key),
		quotedScalar(value),
	)
}

func addIntField(parent *yaml.Node, key string, value int64) {
	parent.Content = append(parent.Content,
		plainScalar(key),
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: fmt.Sprintf("%d", value)},
	)
}
