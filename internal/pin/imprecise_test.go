package pin

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/workflowfile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A composite's imprecise transitive pin is the composite author's choice and
// is never narrowed, so it must not veto narrowing direct uses of that action.
func TestPlan_impreciseTransitivePinDoesNotBlockDirectNarrowing(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755))
	wfPath := filepath.Join(dir, ".github", "workflows", "ci.yml")
	wfKey := workflowfile.KeyFromPath(wfPath)

	store, err := lockfile.LoadState(dir, fakeMeta{})
	require.NoError(t, err)

	deps := []dep.Dependency{
		{NWO: "github/go-linter", Ref: "v2.2.3", Tag: "v2.2.3", Branch: "main", SHA: "1111111111111111111111111111111111111111", HashAlgo: "sha1"},
		// Transitive: pulled in by go-linter, imprecise, not a direct pin.
		{NWO: "actions/checkout", Ref: "v6", Tag: "v6", Branch: "main", SHA: "2222222222222222222222222222222222222222", HashAlgo: "sha1"},
		// Direct and imprecise: this one narrowing must still be allowed to fix.
		{NWO: "actions/setup-go", Ref: "v5", Tag: "v5", Branch: "main", SHA: "3333333333333333333333333333333333333333", HashAlgo: "sha1"},
	}
	parents := map[string][]string{"actions/checkout@v6": {"github/go-linter@v2.2.3"}}
	directKeys := map[string]bool{"github/go-linter@v2.2.3": true, "actions/setup-go@v5": true}
	require.NoError(t, store.Set(context.Background(), wfKey, deps, parents, directKeys))
	require.NoError(t, store.Save())

	imprecise := impreciseDirectNWOs(store)

	assert.False(t, imprecise["actions/checkout"], "transitive-only imprecise pin must not block narrowing")
	assert.True(t, imprecise["actions/setup-go"], "a direct imprecise pin still records the user's precision choice")
}
