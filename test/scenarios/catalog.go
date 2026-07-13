// Package scenarios provides a shared scenario catalog consumable by
// both Go tests and the Ruby integration harness.
package scenarios

import (
	"os"
	"path/filepath"
	"runtime"

	"gopkg.in/yaml.v3"
)

// Catalog is the top-level structure of catalog.yml.
type Catalog struct {
	Version    int        `yaml:"version"`
	Categories []Category `yaml:"categories"`
	Scenarios  []Scenario `yaml:"scenarios"`
}

// Category groups related scenarios.
type Category struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// Scenario describes a single test scenario.
type Scenario struct {
	Name        string   `yaml:"name"`
	Category    string   `yaml:"category"`
	Description string   `yaml:"description"`
	NeedsToken  bool     `yaml:"needs_token"`
	NeedsStub   bool     `yaml:"needs_stub"`
	Skip        string   `yaml:"skip,omitempty"`
	Tags        []string `yaml:"tags"`
	Flags       []string `yaml:"flags"`
	LiveRepo    string   `yaml:"live_repo"`

	// Env sets or unsets process environment variables for the run. A nil
	// value unsets the variable (YAML null), letting scenarios strip ambient
	// auth or redirect GH_HOST to prove offline behavior.
	Env map[string]*string `yaml:"env,omitempty"`

	Fixtures Fixtures `yaml:"fixtures"`
	Expect   Expect   `yaml:"expect"`
}

// Fixtures describes the file-system setup for a scenario.
type Fixtures struct {
	Workflows        map[string]WorkflowFixture `yaml:"workflows"`
	Lockfile         string                     `yaml:"lockfile"`
	LockfileTemplate string                     `yaml:"lockfile_template"`
}

// WorkflowFixture is either a structured action list or raw YAML.
type WorkflowFixture struct {
	Name    string   `yaml:"name"`
	Actions []string `yaml:"actions"`
	Raw     string   `yaml:"raw"`
}

// JQCheck is a single jq-based assertion on JSON output.
type JQCheck struct {
	Expr        string `yaml:"expr"`
	Equals      string `yaml:"equals,omitempty"`
	Contains    string `yaml:"contains,omitempty"`
	NotEquals   string `yaml:"not_equals,omitempty"`
	Matches     string `yaml:"matches,omitempty"`
	GreaterThan *int   `yaml:"greater_than,omitempty"`
}

// Expect declares assertions on the scenario outcome.
type Expect struct {
	Exit           *int                   `yaml:"exit"`
	ExitAny        []int                  `yaml:"exit_any"`
	OutputContains []string               `yaml:"output_contains"`
	OutputExcludes []string               `yaml:"output_excludes"`
	StdoutContains []string               `yaml:"stdout_contains"`
	StdoutIsJSON   bool                   `yaml:"stdout_is_json"`
	LockfileExists bool                   `yaml:"lockfile_exists"`
	Custom         string                 `yaml:"custom"`
	JQ             []JQCheck              `yaml:"jq,omitempty"`
	GoldenJSON     map[string]interface{} `yaml:"golden_json,omitempty"`
}

// HasTag reports whether the scenario has the given tag.
func (s *Scenario) HasTag(tag string) bool {
	for _, t := range s.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

// Load reads the catalog from the canonical location relative to this
// package's source file. Suitable for use in tests.
func Load() (*Catalog, error) {
	_, thisFile, _, _ := runtime.Caller(0)
	catalogPath := filepath.Join(filepath.Dir(thisFile), "catalog.yml")
	return LoadFrom(catalogPath)
}

// LoadFrom reads and parses a catalog from the given path.
func LoadFrom(path string) (*Catalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Catalog
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// ByCategory returns scenarios matching the given category name.
func (c *Catalog) ByCategory(cat string) []Scenario {
	var out []Scenario
	for _, s := range c.Scenarios {
		if s.Category == cat {
			out = append(out, s)
		}
	}
	return out
}

// ByTag returns scenarios having the given tag.
func (c *Catalog) ByTag(tag string) []Scenario {
	var out []Scenario
	for _, s := range c.Scenarios {
		if s.HasTag(tag) {
			out = append(out, s)
		}
	}
	return out
}

// Names returns all scenario names.
func (c *Catalog) Names() []string {
	names := make([]string, len(c.Scenarios))
	for i, s := range c.Scenarios {
		names[i] = s.Name
	}
	return names
}
