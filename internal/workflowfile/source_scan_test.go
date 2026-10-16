package workflowfile

import (
	"context"
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
}
