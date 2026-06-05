package lockfile

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParse_VersionRequired(t *testing.T) {
	_, err := Parse([]byte(`dependencies: {}` + "\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "version is required")
}

func TestParse_UnsupportedVersion(t *testing.T) {
	// A version that isn't well-formed semver is rejected with the generic
	// "unsupported" message — no upgrade-path hint, since we can't tell if
	// the user is behind or just looking at garbage.
	_, err := Parse([]byte("version: garbage\ndependencies: {}\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported dependency lockfile version")
	assert.False(t, errors.Is(err, ErrFutureVersion))
}

func TestParse_FutureVersion_ReturnsFriendlyError(t *testing.T) {
	_, err := Parse([]byte("version: v0.0.999\ndependencies: {}\n"))
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "v0.0.999", "should name the lockfile version")
	assert.Contains(t, msg, Version, "should name the supported version")
	assert.Contains(t, msg, "gh extension upgrade gh-actions-pin", "should tell the user how to upgrade")
	assert.Contains(t, msg, "github.com/github/gh-actions-pin/releases", "should link to releases")
}

func TestParse_FutureVersion_IsErrFutureVersion(t *testing.T) {
	_, err := Parse([]byte("version: v9.0.0\ndependencies: {}\n"))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrFutureVersion), "future-version error should match ErrFutureVersion sentinel")
}

func TestParse_WrongShapeReportsLine(t *testing.T) {
	// A workflow value shaped as a mapping instead of the expected sequence of
	// pin keys fails yaml type-decoding. Parse must surface the failing line as
	// structured data (ParseError.Line) and strip yaml.v3's "yaml:" prefix from
	// the reason so consumers don't misattribute the position to their own file.
	yaml := `version: v0.0.1
dependencies: {}
workflows:
  .github/workflows/ci.yml:
    dependencies:
      - actions/checkout@v6
`
	_, err := Parse([]byte(yaml))
	require.Error(t, err)

	var pe *ParseError
	require.True(t, errors.As(err, &pe), "expected a *ParseError, got %T", err)
	assert.Greater(t, pe.Line, 0, "expected a lockfile line number")
	assert.NotContains(t, pe.Msg, "yaml:", "yaml package prefix must be stripped from the reason")
}

func TestParse_UnsupportedVersionReportsPosition(t *testing.T) {
	// A semantic failure Parse detects itself must carry both line and column,
	// resolved by walking the retained YAML node tree to the offending value.
	yaml := `version: v9
dependencies: {}
`
	_, err := Parse([]byte(yaml))
	require.Error(t, err)

	var pe *ParseError
	require.True(t, errors.As(err, &pe), "expected a *ParseError, got %T", err)
	assert.Equal(t, 1, pe.Line, "version value is on line 1")
	assert.Greater(t, pe.Column, 0, "expected a column for a positioned semantic error")
}

func TestParse_DuplicateActionKeyReportsPosition(t *testing.T) {
	// The conflicting key must be located in the source tree so the position
	// points at a real offending dependency entry.
	yaml := `version: v0.0.1
dependencies:
  actions/checkout@v6:sha1-8e8c483db84b4bee98b60c0593521ed34d9990e8:
    owner_id: 1234
    repo_id: 5678
  Actions/Checkout@v6:SHA1-8E8C483DB84B4BEE98B60C0593521ED34D9990E8:
    owner_id: 9999
    repo_id: 1
`
	_, err := Parse([]byte(yaml))
	require.Error(t, err)

	var pe *ParseError
	require.True(t, errors.As(err, &pe), "expected a *ParseError, got %T", err)
	assert.Greater(t, pe.Line, 0, "expected a line for the conflicting key")
	assert.Greater(t, pe.Column, 0, "expected a column for the conflicting key")
}

func TestParse_PositionLookup(t *testing.T) {
	// The retained node tree is exposed for consumer diagnostics via
	// Position/KeyPosition.
	yaml := `version: v0.0.1
dependencies: {}
workflows:
  .github/workflows/ci.yml: []
`
	f, err := Parse([]byte(yaml))
	require.NoError(t, err)

	line, col, ok := f.Position("version")
	require.True(t, ok)
	assert.Equal(t, 1, line)
	assert.Greater(t, col, 0)

	kl, kc, ok := f.KeyPosition("workflows", ".github/workflows/ci.yml")
	require.True(t, ok)
	assert.Equal(t, 4, kl, "workflow key is on line 4")
	assert.Greater(t, kc, 0)

	_, _, ok = f.Position("nope")
	assert.False(t, ok, "missing path resolves to ok=false")
}

func TestParse_CanonicalizesActionKeys(t *testing.T) {
	const canonical = "actions/checkout@v6:sha1-8e8c483db84b4bee98b60c0593521ed34d9990e8"
	yaml := `version: v0.0.1
dependencies:
  Actions/Checkout@v6:SHA1-8E8C483DB84B4BEE98B60C0593521ED34D9990E8:
    branch: main
    commit: sha1-8e8c483db84b4bee98b60c0593521ed34d9990e8
    owner_id: 1234
    repo_id: 5678
workflows:
  .github/workflows/ci.yml:
    - Actions/Checkout@v6:SHA1-8E8C483DB84B4BEE98B60C0593521ED34D9990E8
`
	f, err := Parse([]byte(yaml))
	require.NoError(t, err)

	// Action map key is canonicalized so a lookup by Pin.String() hits.
	meta, ok := f.Actions[canonical]
	require.True(t, ok, "expected canonical key %q in actions; got keys: %v", canonical, mapKeys(f.Actions))
	assert.Equal(t, int64(1234), meta.OwnerID)
	assert.Equal(t, int64(5678), meta.RepoID)

	// Workflow dependency entries are canonicalized too.
	wf, ok := f.Workflows[".github/workflows/ci.yml"]
	require.True(t, ok)
	require.Len(t, wf, 1)
	assert.Equal(t, canonical, wf[0])
}

func TestParse_ConflictingActionKeyCasings(t *testing.T) {
	// Two source-casings of the same pin with differing metadata is
	// ambiguous and must be rejected.
	yaml := `version: v0.0.1
dependencies:
  actions/checkout@v6:sha1-8e8c483db84b4bee98b60c0593521ed34d9990e8:
    branch: main
    commit: sha1-8e8c483db84b4bee98b60c0593521ed34d9990e8
    owner_id: 1234
    repo_id: 5678
  Actions/Checkout@v6:SHA1-8E8C483DB84B4BEE98B60C0593521ED34D9990E8:
    branch: main
    commit: sha1-8e8c483db84b4bee98b60c0593521ed34d9990e8
    owner_id: 9999
    repo_id: 1
`
	_, err := Parse([]byte(yaml))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate action key")
}

func TestParse_DuplicateActionKeyCasingsSameMetadataOK(t *testing.T) {
	// Same metadata on two casings collapses to one canonical entry.
	yaml := `version: v0.0.1
dependencies:
  actions/checkout@v6:sha1-8e8c483db84b4bee98b60c0593521ed34d9990e8:
    branch: main
    commit: sha1-8e8c483db84b4bee98b60c0593521ed34d9990e8
    owner_id: 1234
    repo_id: 5678
  Actions/Checkout@v6:SHA1-8E8C483DB84B4BEE98B60C0593521ED34D9990E8:
    branch: main
    commit: sha1-8e8c483db84b4bee98b60c0593521ed34d9990e8
    owner_id: 1234
    repo_id: 5678
`
	f, err := Parse([]byte(yaml))
	require.NoError(t, err)
	assert.Len(t, f.Actions, 1)
}

func TestParse_UnparseableActionKeyPreserved(t *testing.T) {
	// Garbage keys are preserved verbatim so structural diagnostics can
	// surface them; Parse itself is not the validator.
	yaml := `version: v0.0.1
dependencies:
  "not a pin":
    branch: main
    commit: sha1-8e8c483db84b4bee98b60c0593521ed34d9990e8
    owner_id: 1
    repo_id: 2
`
	f, err := Parse([]byte(yaml))
	require.NoError(t, err)
	_, ok := f.Actions["not a pin"]
	assert.True(t, ok)
}

func TestParse_WorkflowPathKeyNotCanonicalized(t *testing.T) {
	// File paths are case-sensitive on Linux; do not normalize them.
	yaml := `version: v0.0.1
dependencies: {}
workflows:
  .github/workflows/CI.yml: []
`
	f, err := Parse([]byte(yaml))
	require.NoError(t, err)
	_, ok := f.Workflows[".github/workflows/CI.yml"]
	assert.True(t, ok)
}

func TestParse_TagAndBranchRoundTrip(t *testing.T) {
	yaml := `version: v0.0.1
dependencies:
  actions/checkout@v6:sha1-8e8c483db84b4bee98b60c0593521ed34d9990e8:
    tag: v6
    branch: main
    commit: sha1-8e8c483db84b4bee98b60c0593521ed34d9990e8
    owner_id: 1234
    repo_id: 5678
  actions/internal@trunk:sha1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa:
    branch: trunk
    commit: sha1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    owner_id: 1
    repo_id: 2
workflows: {}
`
	f, err := Parse([]byte(yaml))
	require.NoError(t, err)

	withTag := f.Actions["actions/checkout@v6:sha1-8e8c483db84b4bee98b60c0593521ed34d9990e8"]
	assert.Equal(t, "v6", withTag.Tag)
	assert.Equal(t, "main", withTag.Branch)

	branchOnly := f.Actions["actions/internal@trunk:sha1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"]
	assert.Equal(t, "", branchOnly.Tag)
	assert.Equal(t, "trunk", branchOnly.Branch)
}

func mapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
