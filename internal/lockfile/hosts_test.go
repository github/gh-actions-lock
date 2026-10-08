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

func TestDotcomSaveOmitsHostname(t *testing.T) {
	for _, version := range []string{"", "v0.0.2", "v0.0.3"} {
		t.Run("input version="+version, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, parserlock.Path)
			sha := strings.Repeat("a", 40)
			if version != "" {
				hostname := ""
				if version == "v0.0.3" {
					hostname = "    hostname: github.com\n"
				}
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				body := fmt.Sprintf("version: %s\nworkflows:\n  .github/workflows/ci.yml:\n    - o/r@v1\ndependencies:\n  o/r@v1:\n%s    ref: v1\n    commit: sha1-%s\n    owner_id: 1\n    repo_id: 2\n", version, hostname, sha)
				require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
			}
			store, err := LoadState(dir, hostMetadata{})
			require.NoError(t, err)
			require.NoError(t, store.SetHostname("github.com"))
			if version == "" {
				require.NoError(t, store.Set(context.Background(), ".github/workflows/ci.yml", []dep.Dependency{
					{Hostname: "github.com", NWO: "o/r", Ref: "v1", SHA: sha},
				}, nil, nil))
			}
			require.NoError(t, store.Save())
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Contains(t, string(raw), "version: 'v0.0.3'")
			assert.NotContains(t, string(raw), "hostname:")
			reloaded, err := LoadState(dir, nil)
			require.NoError(t, err)
			require.Len(t, reloaded.AllDeps(), 1)
			assert.Equal(t, "github.com", reloaded.AllDeps()[0].Hostname)
			assert.Equal(t, sha, reloaded.AllDeps()[0].SHA)
			assert.EqualValues(t, 2, reloaded.File().Dependencies["o/r@v1"].RepoID)
			require.NoError(t, reloaded.Save())
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, raw, after)
		})
	}
}

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
	require.NoError(t, store.SetHostname("tenant.ghe.com"))
	sha := strings.Repeat("a", 40)
	tenant := dep.Dependency{Hostname: "tenant.ghe.com", NWO: "o/r", Ref: "tenant", SHA: sha}
	public := dep.Dependency{Hostname: "github.com", NWO: "o/r", Ref: "public", SHA: sha}
	require.NoError(t, store.Set(context.Background(), ".github/workflows/ci.yml", []dep.Dependency{tenant, public}, nil, nil))
	require.NoError(t, store.Save())
	reloaded, err := LoadState(dir, nil)
	require.NoError(t, err)
	require.NoError(t, reloaded.SetHostname("tenant.ghe.com"))
	file := reloaded.File()
	assert.Empty(t, file.Dependencies["o/r@tenant"].Hostname)
	assert.EqualValues(t, 20, file.Dependencies["o/r@tenant"].RepoID)
	assert.Equal(t, "github.com", file.Dependencies["o/r@public"].Hostname)
	assert.EqualValues(t, 2, file.Dependencies["o/r@public"].RepoID)
	deps, err := reloaded.Get(".github/workflows/ci.yml")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"github.com", "tenant.ghe.com"}, []string{deps[0].Hostname, deps[1].Hostname})
	require.NoError(t, reloaded.Set(context.Background(), ".github/workflows/ci.yml", deps, nil, nil))
	assert.EqualValues(t, 20, reloaded.File().Dependencies["o/r@tenant"].RepoID)
	assert.EqualValues(t, 2, reloaded.File().Dependencies["o/r@public"].RepoID)

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
			host := "github.com"
			ownerID, repoID := 1, 2
			if version == "v0.0.3" {
				host = "tenant.ghe.com"
				ownerID, repoID = 10, 20
			}
			body := fmt.Sprintf("version: %s\nworkflows:\n  .github/workflows/ci.yml:\n    - %s\ndependencies:\n  %s:\n    %s\n    commit: sha1-%s\n    owner_id: %d\n    repo_id: %d\n", version, key, key, ref, strings.Repeat("a", 40), ownerID, repoID)
			require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
			store, err := LoadStateAt(path, nil)
			require.NoError(t, err)
			require.NoError(t, store.SetHostname("tenant.ghe.com"))
			deps := store.AllDeps()
			require.Len(t, deps, 1)
			assert.Equal(t, host, deps[0].Hostname)
			assert.Equal(t, strings.Repeat("a", 40), deps[0].SHA)
			store.SetMetadataResolver(hostMetadata{})
			require.NoError(t, store.VerifyHosts(context.Background()))
			require.NoError(t, store.Save())
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Contains(t, string(raw), "version: 'v0.0.3'")
			if version == "v0.0.3" {
				assert.NotContains(t, string(raw), "hostname:")
			} else {
				assert.Contains(t, string(raw), "hostname: 'github.com'")
			}
			action := store.file.Dependencies["o/r@v1"]
			for _, field := range []string{"owner", "repo"} {
				t.Run("rejects changed "+field+" ID", func(t *testing.T) {
					changed := action
					if field == "owner" {
						changed.OwnerID = 200
					} else {
						changed.RepoID = 200
					}
					store.file.Dependencies["o/r@v1"] = changed
					require.ErrorContains(t, store.VerifyHosts(context.Background()), "regenerate the lockfile")
				})
			}
		})
	}
}

func TestSaveRejectsForeignTenant(t *testing.T) {
	store, err := LoadState(t.TempDir(), hostMetadata{})
	require.NoError(t, err)
	require.NoError(t, store.SetHostname("tenant.ghe.com"))
	require.NoError(t, store.Set(context.Background(), ".github/workflows/ci.yml", []dep.Dependency{
		{Hostname: "other.ghe.com", NWO: "o/r", Ref: "v1", SHA: strings.Repeat("a", 40)},
	}, nil, nil))
	require.ErrorContains(t, store.Save(), "not the home host")
	_, err = os.Stat(store.lockPath)
	assert.True(t, os.IsNotExist(err))
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
