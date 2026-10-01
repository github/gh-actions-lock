package lockfile

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type hostMetadata struct{}

func (hostMetadata) RepoIDs(_ context.Context, host, _, _ string) (int64, int64, error) {
	if host == "tenant.ghe.com" {
		return 10, 20, nil
	}
	return 1, 2, nil
}

func TestHostScopedMetadataAndPinCollisions(t *testing.T) {
	dir := t.TempDir()
	store, err := LoadState(dir, hostMetadata{})
	require.NoError(t, err)
	sha := strings.Repeat("a", 40)
	tenant := dep.Dependency{Hostname: "tenant.ghe.com", NWO: "o/r", Ref: "tenant", SHA: sha}
	public := dep.Dependency{Hostname: "github.com", NWO: "o/r", Ref: "public", SHA: sha}
	require.NoError(t, store.Set(context.Background(), ".github/workflows/ci.yml", []dep.Dependency{tenant, public}, nil, nil))
	require.NoError(t, store.Save())
	reloaded, err := LoadState(dir, nil)
	require.NoError(t, err)
	file := reloaded.File()
	assert.Equal(t, "tenant.ghe.com", file.Dependencies["o/r@tenant"].Hostname)
	assert.EqualValues(t, 20, file.Dependencies["o/r@tenant"].RepoID)
	assert.Equal(t, "github.com", file.Dependencies["o/r@public"].Hostname)
	assert.EqualValues(t, 2, file.Dependencies["o/r@public"].RepoID)
	deps, err := reloaded.Get(".github/workflows/ci.yml")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"github.com", "tenant.ghe.com"}, []string{deps[0].Hostname, deps[1].Hostname})

	public.Ref = tenant.Ref
	require.ErrorContains(t, store.Set(context.Background(), ".github/workflows/other.yml", []dep.Dependency{public}, nil, nil), "conflicting hosts")
	fresh, err := LoadState(t.TempDir(), hostMetadata{})
	require.NoError(t, err)
	require.ErrorContains(t, fresh.Set(context.Background(), ".github/workflows/ci.yml", []dep.Dependency{tenant, public}, nil, nil), "conflicting hosts")
}

func TestLegacyAndOmittedHostnames(t *testing.T) {
	for _, version := range []string{"v0.0.1", "v0.0.2", "v0.0.3"} {
		t.Run(version, func(t *testing.T) {
			key := "o/r@v1"
			ref := "ref: v1"
			if version == "v0.0.1" {
				key += ":sha1-" + strings.Repeat("a", 40)
				ref = "tag: v1"
			}
			path := filepath.Join(t.TempDir(), "actions.lock")
			body := fmt.Sprintf("version: %s\nworkflows:\n  .github/workflows/ci.yml:\n    - %s\ndependencies:\n  %s:\n    %s\n    commit: sha1-%s\n    owner_id: 1\n    repo_id: 2\n", version, key, key, ref, strings.Repeat("a", 40))
			require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
			store, err := LoadStateAt(path, nil)
			require.NoError(t, err)
			require.NoError(t, store.SetHostname("tenant.ghe.com"))
			deps := store.AllDeps()
			require.Len(t, deps, 1)
			assert.Equal(t, "github.com", deps[0].Hostname)
			store.SetMetadataResolver(hostMetadata{})
			require.NoError(t, store.VerifyLegacyHosts(context.Background()))
			if version != "v0.0.3" {
				action := store.file.Dependencies["o/r@v1"]
				action.RepoID = 200
				store.file.Dependencies["o/r@v1"] = action
				require.ErrorContains(t, store.VerifyLegacyHosts(context.Background()), "regenerate the lockfile")
			}
			require.NoError(t, store.Save())
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Contains(t, string(raw), "hostname: 'github.com'")
		})
	}
}

func TestGHESKeepsHostLocalFormat(t *testing.T) {
	dir := t.TempDir()
	store, err := LoadState(dir, hostMetadata{})
	require.NoError(t, err)
	require.NoError(t, store.SetHostname("github.example.com"))
	require.NoError(t, store.Set(context.Background(), ".github/workflows/ci.yml", []dep.Dependency{
		{Hostname: "github.example.com", NWO: "o/r", Ref: "v1", SHA: strings.Repeat("a", 40)},
	}, nil, nil))
	require.NoError(t, store.Save())
	raw, err := os.ReadFile(filepath.Join(dir, parserlock.Path))
	require.NoError(t, err)
	assert.Contains(t, string(raw), "version: 'v0.0.2'")
	assert.NotContains(t, string(raw), "hostname:")
	_, err = parserlock.Parse(raw)
	require.NoError(t, err)

	reloaded, err := LoadState(dir, nil)
	require.NoError(t, err)
	require.NoError(t, reloaded.SetHostname("github.example.com"))
	assert.Equal(t, "github.example.com", reloaded.AllDeps()[0].Hostname)
	require.NoError(t, reloaded.Save())
	after, err := os.ReadFile(filepath.Join(dir, parserlock.Path))
	require.NoError(t, err)
	assert.Equal(t, raw, after)
}
