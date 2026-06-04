package lockfile

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// ExecutionType describes how an action runs.
type ExecutionType string

const (
	ExecNode      ExecutionType = "node"
	ExecDocker    ExecutionType = "docker"
	ExecComposite ExecutionType = "composite"
	ExecUnknown   ExecutionType = "unknown"
)

// ActionMeta is the parsed subset of `action.yml` (or `action.yaml`)
// relevant to dependency resolution: the action's name, how it executes,
// and — for composite actions — the list of `uses:` strings of its
// nested steps. Other fields (inputs, outputs, branding, etc.) are
// intentionally not surfaced; consumers that need them should parse
// the YAML themselves.
type ActionMeta struct {
	Name       string
	Execution  ExecutionType
	NestedUses []string
}

// ParseActionMeta parses the contents of an action.yml file into an
// ActionMeta. Composite actions emit their nested step `uses:` strings
// in NestedUses; non-composite actions return an empty NestedUses.
//
// Returns an error only on malformed YAML — unknown `runs.using` values
// resolve to ExecUnknown rather than failing.
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
