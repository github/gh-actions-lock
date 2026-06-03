package lockfile

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSchema_EmbeddedMatchesEnforcement guards against drift between the
// published JSON Schema and the keys Parse actually enforces. The schema file
// is the contract; validateKnownFields is the engine. If they disagree, the
// lockfile would either silently accept fields the schema forbids or reject
// fields it permits.
func TestSchema_EmbeddedMatchesEnforcement(t *testing.T) {
	var doc struct {
		Properties map[string]struct {
			Const string `json:"const"`
		} `json:"properties"`
		Defs struct {
			Action struct {
				Required   []string                   `json:"required"`
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"action"`
		} `json:"$defs"`
	}
	require.NoError(t, json.Unmarshal([]byte(Schema()), &doc), "embedded schema must be valid JSON")

	require.Equal(t, Version, doc.Properties["version"].Const,
		"schema version const must equal the supported Version")

	for key := range doc.Properties {
		_, ok := allowedFileKeys[key]
		assert.Truef(t, ok, "schema declares top-level %q but enforcement does not allow it", key)
	}
	for key := range allowedFileKeys {
		_, ok := doc.Properties[key]
		assert.Truef(t, ok, "enforcement allows top-level %q but schema does not declare it", key)
	}

	for key := range doc.Defs.Action.Properties {
		_, ok := allowedActionKeys[key]
		assert.Truef(t, ok, "schema declares action field %q but enforcement does not allow it", key)
	}
	for key := range allowedActionKeys {
		_, ok := doc.Defs.Action.Properties[key]
		assert.Truef(t, ok, "enforcement allows action field %q but schema does not declare it", key)
	}

	assert.ElementsMatch(t, doc.Defs.Action.Required, requiredActionKeys,
		"schema action.required must match the keys enforcement requires")
}

func TestParse_UnknownTopLevelFieldRejected(t *testing.T) {
	yaml := `version: v0.0.1
dependencies: {}
typo_section: {}
`
	_, err := Parse([]byte(yaml))
	require.Error(t, err)

	var pe *ParseError
	require.True(t, errors.As(err, &pe), "expected a *ParseError, got %T", err)
	assert.Contains(t, pe.Msg, `unknown lockfile field "typo_section"`)
	assert.Equal(t, 3, pe.Line, "unknown key is on line 3")
	assert.Greater(t, pe.Column, 0, "expected a column anchored on the offending key")
}

func TestParse_UnknownActionFieldRejected(t *testing.T) {
	yaml := `version: v0.0.1
dependencies:
  actions/checkout@v4:sha1-34e114876b0b11c390a56381ad16ebd13914f8d5:
    owner_id: 1
    repo_id: 2
    flavor: spicy
`
	_, err := Parse([]byte(yaml))
	require.Error(t, err)

	var pe *ParseError
	require.True(t, errors.As(err, &pe), "expected a *ParseError, got %T", err)
	assert.Contains(t, pe.Msg, `unknown action field "flavor"`)
	assert.Contains(t, pe.Msg, "actions/checkout@v4", "message should name the offending dependency")
	assert.Equal(t, 6, pe.Line, "unknown action key is on line 6")
	assert.Greater(t, pe.Column, 0, "expected a column anchored on the offending key")
}

func TestParse_MissingRequiredActionFieldRejected(t *testing.T) {
	// owner_id/repo_id/commit present, but branch (required) is absent.
	yaml := `version: v0.0.1
dependencies:
  actions/checkout@v4:sha1-34e114876b0b11c390a56381ad16ebd13914f8d5:
    commit: sha1-34e114876b0b11c390a56381ad16ebd13914f8d5
    owner_id: 1
    repo_id: 2
`
	_, err := Parse([]byte(yaml))
	require.Error(t, err)

	var pe *ParseError
	require.True(t, errors.As(err, &pe), "expected a *ParseError, got %T", err)
	assert.Contains(t, pe.Msg, `missing required action field "branch"`)
	assert.Contains(t, pe.Msg, "actions/checkout@v4", "message should name the offending dependency")
	assert.Equal(t, 3, pe.Line, "error anchors on the dependency's pin key")
	assert.Greater(t, pe.Column, 0, "expected a column anchored on the pin key")
}

func TestParse_KnownFieldsAccepted(t *testing.T) {
	yaml := `version: v0.0.1
workflows:
  .github/workflows/ci.yml:
    - actions/checkout@v4:sha1-34e114876b0b11c390a56381ad16ebd13914f8d5
dependencies:
  actions/checkout@v4:sha1-34e114876b0b11c390a56381ad16ebd13914f8d5:
    tag: v4
    branch: main
    commit: sha1-34e114876b0b11c390a56381ad16ebd13914f8d5
    owner_id: 1
    repo_id: 2
    uses:
      - actions/cache@v4:sha1-0000000000000000000000000000000000000000
`
	f, err := Parse([]byte(yaml))
	require.NoError(t, err)
	assert.Len(t, f.Actions, 1)
	assert.Contains(t, f.Workflows, ".github/workflows/ci.yml")
}
