package workflowfile

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mapSource is an in-memory filesource.Source keyed by repo-relative path.
type mapSource map[string]string

func (m mapSource) ReadFile(_ context.Context, name string) ([]byte, error) {
	if s, ok := m[name]; ok {
		return []byte(s), nil
	}
	return nil, fmt.Errorf("%s: %w", name, fs.ErrNotExist)
}

func (m mapSource) ListFiles(context.Context, string) ([]string, error) { return nil, nil }

func TestScanSelfRepositoryActionsFrom(t *testing.T) {
	ctx := context.Background()

	t.Run("recursive closure resolves against the source", func(t *testing.T) {
		src := mapSource{
			"actions/root/action.yml": `runs:
  using: composite
  steps:
    - uses: actions/checkout@v4
    - uses: $/actions/leaf
    - uses: ./local
`,
			"actions/leaf/action.yaml": `runs:
  using: composite
  steps:
    - uses: actions/setup-node@v4
`,
		}
		scan := ScanSelfRepositoryActionsFrom(ctx, src, []string{"$/actions/root"})

		assert.Empty(t, scan.Errors)
		require.Len(t, scan.Refs, 2)
		assert.ElementsMatch(t, []string{"actions/checkout", "actions/setup-node"}, []string{scan.Refs[0].NWO(), scan.Refs[1].NWO()})
		assert.Equal(t, []string{"actions/root/action.yml", "actions/leaf/action.yaml"}, scan.ActionFiles, "repo-relative paths, action.yaml fallback")
		assert.Equal(t, []string{"$/actions/leaf"}, scan.SelfRepositoryRefs)
		assert.Equal(t, []string{"./local"}, scan.LocalPaths)
	})

	t.Run("missing action is an error, not a crash", func(t *testing.T) {
		scan := ScanSelfRepositoryActionsFrom(ctx, mapSource{}, []string{"$/actions/gone"})

		require.Len(t, scan.Errors, 1)
		assert.Contains(t, scan.Errors[0], "can't inspect self repository action $/actions/gone")
	})

	t.Run("path escaping the repository is refused", func(t *testing.T) {
		scan := ScanSelfRepositoryActionsFrom(ctx, mapSource{}, []string{"$/../sibling"})

		// mapSource does not police paths, so the escape is only caught by
		// real sources; the scan must still terminate with an error.
		require.Len(t, scan.Errors, 1)
	})

	t.Run("version suffix is invalid", func(t *testing.T) {
		scan := ScanSelfRepositoryActionsFrom(ctx, mapSource{}, []string{"$/actions/root@v1"})

		assert.Equal(t, []string{"$/actions/root@v1"}, scan.SelfRepositoryRefErrs)
	})

	t.Run("no refs is a no-op", func(t *testing.T) {
		scan := ScanSelfRepositoryActionsFrom(ctx, mapSource{}, nil)

		assert.Empty(t, scan.Errors)
		assert.Empty(t, scan.Refs)
	})

	t.Run("uses expression in an action is reported", func(t *testing.T) {
		src := mapSource{"actions/x/action.yml": "runs:\n  using: composite\n  steps:\n    - uses: actions/${{ matrix.a }}@v1\n"}
		scan := ScanSelfRepositoryActionsFrom(ctx, src, []string{"$/actions/x"})

		require.Len(t, scan.Errors, 1)
		assert.Contains(t, scan.Errors[0], "parsing action.yml")
	})

	t.Run("action.yaml is not read when action.yml fails for another reason", func(t *testing.T) {
		src := errSource{
			mapSource: mapSource{"actions/x/action.yaml": "runs:\n  using: composite\n  steps:\n    - uses: actions/checkout@v4\n"},
			failPath:  "actions/x/action.yml",
			err:       errors.New("HTTP 403: forbidden"),
		}
		scan := ScanSelfRepositoryActionsFrom(ctx, src, []string{"$/actions/x"})

		require.Len(t, scan.Errors, 1)
		assert.Contains(t, scan.Errors[0], "403")
		assert.Empty(t, scan.Refs, "the fallback manifest must not be used")
	})

	t.Run("a failing action.yaml is reported when action.yml is absent", func(t *testing.T) {
		src := errSource{
			mapSource: mapSource{},
			failPath:  "actions/x/action.yaml",
			err:       errors.New("HTTP 500"),
		}
		scan := ScanSelfRepositoryActionsFrom(ctx, src, []string{"$/actions/x"})

		require.Len(t, scan.Errors, 1)
		assert.Contains(t, scan.Errors[0], "500")
	})
}

// errSource fails reads of failPath with err.
type errSource struct {
	mapSource
	failPath string
	err      error
}

func (s errSource) ReadFile(ctx context.Context, name string) ([]byte, error) {
	if name == s.failPath {
		return nil, s.err
	}
	return s.mapSource.ReadFile(ctx, name)
}

func TestReusableWorkflowPaths(t *testing.T) {
	f, err := Parse(".github/workflows/ci.yml", []byte(`on: push
jobs:
  a:
    uses: ./.github/workflows/build.yml
  b:
    uses: $/.github/workflows/lint.yaml
  c:
    uses: ./.github/workflows/build.yml
  d:
    uses: owner/repo/.github/workflows/x.yml@v1
  e:
    uses: $/.github/workflows/bad.yml@v1
  g:
    runs-on: ubuntu-latest
    steps:
      - uses: ./actions/local
      - uses: $/actions/self
`))
	require.NoError(t, err)

	assert.Equal(t, []string{".github/workflows/build.yml", ".github/workflows/lint.yaml"}, f.ReusableWorkflowPaths(),
		"job-level same-repo calls only, deduplicated; remote, versioned and step-level refs are excluded")
}
