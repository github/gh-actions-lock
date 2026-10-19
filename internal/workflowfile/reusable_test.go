package workflowfile

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReusableWorkflowIsNotAnActionLockRoot(t *testing.T) {
	f, err := Parse("ci.yml", []byte(`jobs:
  call:
    uses: acme/build/.github/workflows/build.yml@main
  steps:
    steps:
      - uses: acme/build@main
`))
	require.NoError(t, err)
	scan := f.ExtractActionRefs()
	require.Len(t, scan.Refs, 1)
	assert.Empty(t, scan.Refs[0].Path)
	require.Len(t, scan.ReusableWorkflowRefs, 1)
	assert.Equal(t, ".github/workflows/build.yml", scan.ReusableWorkflowRefs[0].Path)

	rewritten, n, err := f.RewriteActionRefs(map[string]string{"acme/build@main": "acme/build@v1"})
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Contains(t, string(rewritten), "uses: acme/build/.github/workflows/build.yml@main")
	assert.Contains(t, string(rewritten), "- uses: acme/build@v1")
}
