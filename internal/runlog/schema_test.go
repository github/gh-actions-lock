package runlog

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// schemaDoc is a partial view of the embedded JSON Schema, sufficient to
// verify it stays in sync with the Go types it documents.
type schemaDoc struct {
	ID         string                  `json:"$id"`
	Required   []string                `json:"required"`
	Properties map[string]schemaProp   `json:"properties"`
	Defs       map[string]schemaObject `json:"$defs"`
}

type schemaProp struct {
	Const string   `json:"const"`
	Ref   string   `json:"$ref"`
	Type  string   `json:"type"`
	Enum  []string `json:"enum"`
}

type schemaObject struct {
	Type       string                `json:"type"`
	Required   []string              `json:"required"`
	Properties map[string]schemaProp `json:"properties"`
}

func loadSchema(t *testing.T) schemaDoc {
	t.Helper()
	var doc schemaDoc
	require.NoError(t, json.Unmarshal(Schema(), &doc), "embedded schema must be valid JSON")
	return doc
}

// jsonFields returns the JSON field name → omitempty map for a struct type,
// matching Go's `encoding/json` tag handling. Fields tagged `json:"-"` are
// excluded.
func jsonFields(t reflect.Type) map[string]bool {
	out := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		parts := strings.Split(tag, ",")
		name := parts[0]
		if name == "" {
			continue
		}
		omit := false
		for _, p := range parts[1:] {
			if p == "omitempty" {
				omit = true
			}
		}
		out[name] = omit
	}
	return out
}

// assertObjectMatchesStruct cross-checks a schema object against a Go struct:
// every JSON field declared on the struct must be in the schema, every schema
// property must be on the struct, and required-vs-optional must agree with
// `,omitempty`.
func assertObjectMatchesStruct(t *testing.T, label string, schemaProps map[string]schemaProp, schemaRequired []string, goType reflect.Type) {
	t.Helper()

	goFields := jsonFields(goType)
	for name := range goFields {
		_, ok := schemaProps[name]
		assert.Truef(t, ok, "%s: Go struct has %q but schema does not", label, name)
	}
	for name := range schemaProps {
		_, ok := goFields[name]
		assert.Truef(t, ok, "%s: schema has %q but Go struct does not", label, name)
	}

	wantRequired := map[string]bool{}
	for name, omit := range goFields {
		if !omit {
			wantRequired[name] = true
		}
	}
	gotRequired := map[string]bool{}
	for _, name := range schemaRequired {
		gotRequired[name] = true
	}
	assert.Equalf(t, wantRequired, gotRequired,
		"%s: schema required must match Go fields without ,omitempty", label)
}

// TestSchema_TopLevelMatchesReport guards against drift between the embedded
// schema and the Report struct that WriteReport serializes. They are the
// contract; if they disagree, the file we publish stops matching the file we
// promise.
func TestSchema_TopLevelMatchesReport(t *testing.T) {
	doc := loadSchema(t)

	assert.Equal(t, "https://gh.io/gh-actions-pin/provenance-v1.json", doc.ID)
	assert.Equal(t, SchemaVersion, doc.Properties["schema"].Const,
		"schema const must equal the supported SchemaVersion")

	assertObjectMatchesStruct(t, "Report", doc.Properties, doc.Required, reflect.TypeOf(Report{}))
}

func TestSchema_DefsMatchStructs(t *testing.T) {
	doc := loadSchema(t)

	cases := []struct {
		def    string
		goType reflect.Type
	}{
		{"tool", reflect.TypeOf(ToolInfo{})},
		{"repo", reflect.TypeOf(RepoInfo{})},
		{"summary", reflect.TypeOf(Summary{})},
		{"action", reflect.TypeOf(Action{})},
		{"auto_fix", reflect.TypeOf(AutoFix{})},
	}
	for _, c := range cases {
		obj, ok := doc.Defs[c.def]
		require.Truef(t, ok, "schema $defs.%s must exist", c.def)
		assertObjectMatchesStruct(t, "$defs."+c.def, obj.Properties, obj.Required, c.goType)
	}
}

// TestSchema_ResolutionEnumMatchesConstants ensures the documented resolution
// values stay aligned with the Resolution* constants the package exports.
func TestSchema_ResolutionEnumMatchesConstants(t *testing.T) {
	doc := loadSchema(t)
	enum := doc.Defs["action"].Properties["resolution"].Enum
	want := []string{
		ResolutionPinned,
		ResolutionAlreadyPinned,
		ResolutionInvestigate,
		ResolutionSkipped,
		ResolutionUnresolved,
	}
	assert.ElementsMatch(t, want, enum,
		"action.resolution enum must list every Resolution* constant")
}

// TestSchema_RoundTripKnownGoodReport marshals a representative Report and
// checks that every required top-level/$defs key is present in the JSON the
// package would actually write. This is a shape check, not a JSON-Schema
// validator run — pkg/lockfile takes the same approach to avoid pulling in a
// validator dependency.
func TestSchema_RoundTripKnownGoodReport(t *testing.T) {
	canonical := true
	r := Report{
		Schema:      SchemaVersion,
		GeneratedAt: "2025-01-01T00:00:00Z",
		Tool:        ToolInfo{Name: "gh-actions-pin", Version: "v0.0.0-test"},
		Repo:        &RepoInfo{Owner: "github", Name: "gh-actions-pin", Host: "github.com"},
		Summary: Summary{
			Workflows:     1,
			Actions:       1,
			Valid:         true,
			Pinned:        1,
			AlreadyPinned: 0,
			FullScan:      0,
			Investigate:   0,
			Skipped:       0,
			Unresolved:    0,
		},
		Actions: []Action{{
			NWO:             "actions/checkout",
			Ref:             "v4",
			SHA:             "sha1-34e114876b0b11c390a56381ad16ebd13914f8d5",
			HashAlgo:        "sha1",
			Direct:          true,
			Resolution:      ResolutionPinned,
			How:             "locked ref v4 to sha1-34e1...",
			CanonicalBranch: &canonical,
			Workflows:       []string{".github/workflows/ci.yml"},
		}},
	}

	b, err := json.Marshal(r)
	require.NoError(t, err)

	var generic map[string]any
	require.NoError(t, json.Unmarshal(b, &generic))

	doc := loadSchema(t)
	for _, key := range doc.Required {
		_, ok := generic[key]
		assert.Truef(t, ok, "marshalled Report missing required top-level key %q", key)
	}

	action := generic["actions"].([]any)[0].(map[string]any)
	for _, key := range doc.Defs["action"].Required {
		_, ok := action[key]
		assert.Truef(t, ok, "marshalled Action missing required key %q", key)
	}
}
