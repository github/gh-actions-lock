package resolve

import (
	"context"
	"fmt"
	"strings"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/ghapi"
	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/github/gh-actions-lock/internal/pinpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRemoteCompositeRejectsMalformedRemoteRef: a fetched composite's
// malformed remote uses: fails resolution rather than being skipped.
func TestRemoteCompositeRejectsMalformedRemoteRef(t *testing.T) {
	const rejected = "actions/checkout@main:action.yml"
	r := seedCache(&Resolver{MaxRecursionDepth: DefaultMaxRecursionDepth}, map[ghapi.ActionRef]resolvedEntry{
		ghapi.ForActionRef("owner", "composite", "", "v1"): {
			dep:       dep.Dependency{NWO: "owner/composite", Ref: "v1", SHA: strings.Repeat("a", 40)},
			actionYML: "runs:\n  using: composite\n  steps:\n    - uses: " + rejected + "\n",
		},
	})

	_, _, err := r.ResolveAllRecursive(context.Background(), []parserlock.ActionRef{
		{Owner: "owner", Repo: "composite", Ref: "v1"},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), rejected)
}

func TestReadManifests_Batching(t *testing.T) {
	t.Run("small concurrent batches", func(t *testing.T) {
		reg := &httpmock.Registry{}
		reg.RegisterReusable(httpmock.GraphQL(`.`), httpmock.JSONResponse(map[string]any{"data": map[string]any{}}))
		r, err := New("github.com", pinpool.New(0, nil), WithTransport(reg))
		require.NoError(t, err)
		reqs := make([]ghapi.ActionFileRequest, 2*manifestBatchSize+1)
		for i := range reqs {
			reqs[i] = ghapi.ActionFileRequest{Owner: "octo", Repo: fmt.Sprintf("r%d", i), Ref: strings.Repeat("a", 40)}
		}

		got := r.ReadManifests(context.Background(), reqs)

		assert.Len(t, got, len(reqs))
		assert.Len(t, reg.Requests, 3)
		_, pool := manifestBatching(r.Pool)
		assert.Equal(t, manifestConcurrency, pool.Workers)
	})

	t.Run("resolution batches behind the dependabot proxy", func(t *testing.T) {
		t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
		size, _ := manifestBatching(pinpool.New(0, nil))
		assert.Equal(t, batchActionFileSize, size)
	})
}

// TestResolveAllRecursiveSeededChildKeepsWrittenSpelling: a lockfile seed
// stores owner/repo lowercased. When a composite names that child in mixed
// case, the child must come back under the composite's spelling so its
// Key() finds the parent edge; otherwise the child is pruned as an orphan.
func TestResolveAllRecursiveSeededChildKeepsWrittenSpelling(t *testing.T) {
	r := seedCache(&Resolver{MaxRecursionDepth: DefaultMaxRecursionDepth}, map[ghapi.ActionRef]resolvedEntry{
		ghapi.ForActionRef("owner", "composite", "", "v2"): {
			dep:       dep.Dependency{NWO: "owner/composite", Ref: "v2", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			actionYML: "name: C\nruns:\n  using: composite\n  steps:\n    - uses: MixedCase/Child@v1\n",
		},
	})
	r.SeedFromLockfile([]dep.Dependency{{Hostname: "github.com", NWO: "mixedcase/child", Ref: "v1", SHA: "cccccccccccccccccccccccccccccccccccccccc"}})

	deps, parentMap, err := r.ResolveAllRecursive(context.Background(), []parserlock.ActionRef{
		{Owner: "owner", Repo: "composite", Ref: "v2"},
	})
	require.NoError(t, err)

	var child *dep.Dependency
	for i := range deps {
		if strings.EqualFold(deps[i].NWO, "mixedcase/child") {
			child = &deps[i]
		}
	}
	require.NotNil(t, child, "child missing from %+v", deps)
	assert.Equal(t, "MixedCase/Child", child.NWO)
	assert.Equal(t, "cccccccccccccccccccccccccccccccccccccccc", child.SHA)
	assert.Equal(t, []string{"owner/composite@v2"}, parentMap[child.Key()])
}
