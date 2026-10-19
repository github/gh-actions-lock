package resolve

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/ghapi"
	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/github/gh-actions-lock/internal/pinpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveLockedRecursiveCachesContentWithoutLosingLogicalRefs(t *testing.T) {
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	for _, path := range []string{"root", "sibling"} {
		reg.Register(httpmock.GraphQL(`file: file\(path`), func(req *http.Request) (*http.Response, error) {
			var body struct {
				Variables map[string]string `json:"variables"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				return nil, err
			}
			assert.Equal(t, sha+"^{commit}", body.Variables["expr0"])
			assert.Equal(t, path+"/action.yml", body.Variables["yml0"])
			yml := "runs:\n  using: node20\n"
			if path == "root" {
				yml = "runs:\n  using: composite\n  steps:\n    - uses: $/sibling\n"
			}
			return httpmock.JSONResponse(map[string]any{"data": map[string]any{
				"a0": map[string]any{
					"nameWithOwner": "new/action",
					"object": map[string]any{
						"oid": sha, "file": map[string]any{"object": map[string]any{"text": yml}},
					},
				},
			}})(req)
		})
	}
	r, err := New("github.com", pinpool.New(2, nil), WithTransport(reg))
	require.NoError(t, err)
	for _, ref := range []string{"Main", "main"} {
		d := dep.Dependency{NWO: "old/action", Ref: ref, SHA: sha}
		r.SeedFromLockfile([]dep.Dependency{d})
		deps, parents, err := r.ResolveLockedRecursive(context.Background(),
			[]parserlock.ActionRef{{Owner: "old", Repo: "action", Path: "root", Ref: ref}},
			[]dep.Dependency{d})
		require.NoError(t, err)
		require.Len(t, deps, 1, "same-tarball paths collapse into one dependency")
		assert.Equal(t, "old/action", deps[0].NWO, "locked discovery retains the recorded name across a redirect")
		assert.Equal(t, ref, deps[0].Ref, "content cache must not retain the first logical ref")
		assert.Empty(t, parents, "same-tarball uses must not produce a self-edge")
	}
	assert.Len(t, reg.Requests, 2, "immutable root and sibling content reused across logical refs")
}

func TestResolveLockedRecursiveMissingChildDoesNotResolveLiveRef(t *testing.T) {
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(httpmock.GraphQL(`file: file\(path`), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": map[string]any{
			"nameWithOwner": "o/root",
			"object": map[string]any{
				"oid":  sha,
				"file": map[string]any{"object": map[string]any{"text": "runs:\n  using: composite\n  steps:\n    - uses: o/child@Main\n"}},
			},
		}},
	}))
	r, err := New("github.com", pinpool.New(2, nil), WithTransport(reg))
	require.NoError(t, err)
	_, parents, err := r.ResolveLockedRecursive(context.Background(),
		[]parserlock.ActionRef{{Owner: "o", Repo: "root", Ref: "v1"}},
		[]dep.Dependency{{NWO: "o/root", Ref: "v1", SHA: sha}})
	var missing *UnlockedRefError
	require.ErrorAs(t, err, &missing)
	assert.Equal(t, "Main", missing.Ref.Ref)
	assert.Equal(t, []string{"o/root@v1"}, parents["o/child@Main"])
	assert.Len(t, reg.Requests, 1)
}

func TestResolveRecursiveRejectsMalformedManifest(t *testing.T) {
	for name, locked := range map[string]bool{"live refs": false, "locked commits": true} {
		t.Run(name, func(t *testing.T) {
			const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			reg := &httpmock.Registry{}
			defer reg.Verify(t)
			reg.Register(httpmock.GraphQL(`file: file\(path`), httpmock.JSONResponse(map[string]any{
				"data": map[string]any{"a0": map[string]any{
					"nameWithOwner": "o/root",
					"object":        map[string]any{"oid": sha, "file": map[string]any{"object": map[string]any{"text": "runs: ["}}},
				}},
			}))
			r, err := New("github.com", pinpool.New(2, nil), WithTransport(reg))
			require.NoError(t, err)
			refs := []parserlock.ActionRef{{Owner: "o", Repo: "root", Ref: "v1"}}
			if locked {
				_, _, err = r.ResolveLockedRecursive(context.Background(), refs,
					[]dep.Dependency{{NWO: "o/root", Ref: "v1", SHA: sha}})
			} else {
				_, _, err = r.ResolveAllRecursive(context.Background(), refs)
			}
			require.Error(t, err)
			assert.ErrorContains(t, err, "reading manifest for o/root@v1")
		})
	}
}

func TestResolveLockedRecursiveMissingManifest(t *testing.T) {
	for _, tt := range []struct {
		name    string
		path    string
		wantErr bool
	}{
		{name: "missing action cannot establish coverage", path: "action", wantErr: true},
		{name: "callee workflow owns its own lockfile", path: ".github/workflows/callee.yml"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			reg := &httpmock.Registry{}
			defer reg.Verify(t)
			reg.Register(httpmock.GraphQL(`file: file\(path`), httpmock.JSONResponse(map[string]any{
				"data": map[string]any{"a0": map[string]any{
					"nameWithOwner": "o/root", "object": map[string]any{"oid": sha},
				}},
			}))
			r, err := New("github.com", pinpool.New(2, nil), WithTransport(reg))
			require.NoError(t, err)
			_, parents, err := r.ResolveLockedRecursive(context.Background(),
				[]parserlock.ActionRef{{Owner: "o", Repo: "root", Path: tt.path, Ref: "v1"}},
				[]dep.Dependency{{NWO: "o/root", Ref: "v1", SHA: sha}})
			if tt.wantErr {
				require.ErrorContains(t, err, "manifest is missing")
			} else {
				require.NoError(t, err)
			}
			assert.Empty(t, parents)
		})
	}
}

func TestResolveRecursiveMixedCaseSiblingDoesNotCreateSelfEdge(t *testing.T) {
	for name, locked := range map[string]bool{"live refs": false, "locked commits": true} {
		t.Run(name, func(t *testing.T) {
			const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			entries := map[ghapi.ActionRef]resolvedEntry{
				ghapi.ForActionRef("o", "action", "", "Main"): {
					dep:       dep.Dependency{NWO: "o/action", Ref: "Main", SHA: sha},
					actionYML: "runs:\n  using: composite\n  steps:\n    - uses: O/Action/sibling@Main\n",
				},
				ghapi.ForActionRef("o", "action", "sibling", "Main"): {
					dep:       dep.Dependency{NWO: "o/action", Path: "sibling", Ref: "Main", SHA: sha},
					actionYML: "runs:\n  using: composite\n  steps:\n    - uses: vendor/child@Main\n",
				},
				ghapi.ForActionRef("vendor", "child", "", "Main"): {
					dep:       dep.Dependency{NWO: "vendor/child", Ref: "Main", SHA: sha},
					actionYML: "runs:\n  using: node20\n",
				},
			}
			r := seedCache(&Resolver{MaxRecursionDepth: DefaultMaxRecursionDepth}, entries)
			refs := []parserlock.ActionRef{{Owner: "o", Repo: "action", Ref: "Main"}}
			var deps []dep.Dependency
			var parents dep.ParentMap
			var err error
			if locked {
				for _, entry := range entries {
					owner, repo := entry.dep.OwnerRepo()
					r.lockedContentCache.Put(ghapi.ForActionRef(owner, repo, entry.dep.Path, sha), entry)
				}
				deps, parents, err = r.ResolveLockedRecursive(context.Background(), refs,
					[]dep.Dependency{{NWO: "o/action", Ref: "Main", SHA: sha}, {NWO: "vendor/child", Ref: "Main", SHA: sha}})
			} else {
				deps, parents, err = r.ResolveAllRecursive(context.Background(), refs)
			}
			require.NoError(t, err)
			assert.Len(t, deps, 2)
			assert.Len(t, parents, 1, "same-tarball sibling must not produce a self-edge")
			assert.Equal(t, []string{"o/action@Main"}, parents["vendor/child@Main"])
		})
	}
}

func TestResolveLockedRecursiveDoesNotFoldRefCasing(t *testing.T) {
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	r := &Resolver{MaxRecursionDepth: DefaultMaxRecursionDepth}
	_, _, err := r.ResolveLockedRecursive(context.Background(),
		[]parserlock.ActionRef{{Owner: "O", Repo: "Action", Ref: "Main"}},
		[]dep.Dependency{{NWO: "o/action", Ref: "main", SHA: sha}})
	var missing *UnlockedRefError
	require.ErrorAs(t, err, &missing)
	assert.Equal(t, "Main", missing.Ref.Ref)
}
