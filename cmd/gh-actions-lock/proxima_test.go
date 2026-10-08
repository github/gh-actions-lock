package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type proximaTransport func(*http.Request) (*http.Response, error)

func (f proximaTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

const tenantSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const publicSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func proximaFixture(t *testing.T, publicStatus int) http.RoundTripper {
	t.Helper()
	return proximaTransport(func(req *http.Request) (*http.Response, error) {
		tenant := req.URL.Host == "api.tenant.ghe.com"
		if tenant {
			assert.NotEmpty(t, req.Header.Get("Authorization"))
		} else {
			assert.Equal(t, "api.github.com", req.URL.Host)
			assert.Empty(t, req.Header.Get("Authorization"))
			assert.Empty(t, req.Header.Get("Cookie"))
		}
		path := req.URL.Path
		switch {
		case tenant && path == "/repos/tenant/internal":
			return httpmock.JSONResponse(map[string]any{
				"visibility": "internal", "id": 111, "owner": map[string]any{"id": 11},
			})(req)
		case path == "/repos/actions/public":
			if tenant {
				return httpmock.StatusResponse(http.StatusNotFound)(req)
			}
			if publicStatus != 200 {
				return httpmock.StatusResponse(publicStatus)(req)
			}
			return httpmock.JSONResponse(map[string]any{
				"visibility": "public", "id": 222, "owner": map[string]any{"id": 22},
			})(req)
		case tenant && path == "/graphql":
			var body struct {
				Variables map[string]string `json:"variables"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				return nil, err
			}
			assert.Equal(t, "internal", body.Variables["name0"])
			composite := "runs:\n  using: composite\n  steps:\n    - uses: actions/public@v2\n"
			return httpmock.JSONResponse(map[string]any{"data": map[string]any{
				"a0": testRepoResponse("tenant/internal", tenantSHA, composite),
			}})(req)
		case !tenant && path == "/repos/actions/public/commits/v2":
			return httpmock.JSONResponse(map[string]any{"sha": publicSHA})(req)
		case !tenant && path == "/repos/actions/public/contents/action.yml":
			assert.Equal(t, publicSHA, req.URL.Query().Get("ref"))
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(nodeActionYAML)), Request: req}, nil
		case strings.HasSuffix(path, "/releases"):
			return httpmock.JSONResponse([]any{})(req)
		}
		return nil, fmt.Errorf("unexpected request: %s %s", req.Method, req.URL)
	})
}

func TestProximaGenerationPreservesHosts(t *testing.T) {
	path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: tenant/internal@v1
`)
	args := []string{"--hostname", "tenant.ghe.com", "--no-interactive", "--no-narrow", "--no-migrate-local-actions", "--json", path}
	_, _, err := runCommandWithHTTP(t, proximaFixture(t, 200), args...)
	require.NoError(t, err)
	raw := readTempLockfilePins(t)
	file, err := parserlock.Parse([]byte(raw))
	require.NoError(t, err)
	assert.Equal(t, "v0.0.3", file.Version)
	require.Len(t, file.Dependencies, 2)
	local := file.Dependencies["tenant/internal@v1"]
	assert.Equal(t, "tenant.ghe.com", local.Hostname)
	assert.Equal(t, []string{"tenant/internal@v1"}, file.Workflows[path])
	assert.Equal(t, "sha1-"+tenantSHA, local.Commit)
	assert.EqualValues(t, 11, local.OwnerID)
	assert.EqualValues(t, 111, local.RepoID)
	assert.Equal(t, []string{"actions/public@v2"}, local.Uses)
	public := file.Dependencies["actions/public@v2"]
	assert.Equal(t, "github.com", public.Hostname)
	assert.Equal(t, "sha1-"+publicSHA, public.Commit)
	assert.EqualValues(t, 22, public.OwnerID)
	assert.EqualValues(t, 222, public.RepoID)

	_, _, err = runCommandWithHTTP(t, proximaFixture(t, 200), args...)
	require.NoError(t, err)
	assert.Equal(t, raw, readTempLockfilePins(t), "repeat generation must retain hosts and IDs")

	base := proximaFixture(t, 200)
	pinnedTransport := proximaTransport(func(req *http.Request) (*http.Response, error) {
		assert.False(t, req.URL.Host == "api.tenant.ghe.com" && req.URL.Path == "/repos/actions/public",
			"recorded dotcom pins must bypass tenant namesakes")
		return base.RoundTrip(req)
	})
	_, _, err = runCommandWithHTTP(t, pinnedTransport,
		"--hostname", "tenant.ghe.com", "--rescan", "--no-fix", "--json", path)
	require.NoError(t, err)
	assert.Equal(t, raw, readTempLockfilePins(t))

	// Omitted public hostnames retain their dotcom binding.
	omitted := strings.Replace(raw, "        hostname: 'github.com'\n", "", 1)
	require.NotEqual(t, raw, omitted)
	require.NoError(t, os.WriteFile(parserlock.Path, []byte(omitted), 0o600))
	_, _, err = runCommandWithHTTP(t, pinnedTransport,
		"--hostname", "tenant.ghe.com", "--rescan", "--no-fix", "--json", path)
	require.NoError(t, err)
	assert.Equal(t, omitted, readTempLockfilePins(t))
	_, _, err = runCommandWithHTTP(t, proximaFixture(t, 200), args...)
	require.NoError(t, err)
	assert.Equal(t, raw, readTempLockfilePins(t))
}

func TestProximaOmittedPinsCannotBecomeTenantPins(t *testing.T) {
	for _, status := range []int{200, 401, 403, 404} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: tenant/internal@v1
`, "tenant/internal@v1=sha1-"+tenantSHA)
			raw := readTempLockfilePins(t)
			raw = strings.ReplaceAll(raw, "owner_id: 1", "owner_id: 11")
			raw = strings.ReplaceAll(raw, "repo_id: 1", "repo_id: 111")
			require.NoError(t, os.WriteFile(parserlock.Path, []byte(raw), 0o600))
			transport := proximaTransport(func(req *http.Request) (*http.Response, error) {
				assert.Equal(t, "api.github.com", req.URL.Host, "omitted pins must bind to dotcom, never the tenant")
				assert.Equal(t, "/repos/tenant/internal", req.URL.Path)
				assert.Empty(t, req.Header.Get("Authorization"))
				assert.Empty(t, req.Header.Get("Cookie"))
				if status != 200 {
					return httpmock.StatusResponse(status)(req)
				}
				return httpmock.JSONResponse(map[string]any{
					"visibility": "public", "id": 333, "owner": map[string]any{"id": 33},
				})(req)
			})
			_, _, err := runCommandWithHTTP(t, transport,
				"--hostname", "tenant.ghe.com", "--no-narrow", "--no-migrate-local-actions", "--json", path)
			require.Error(t, err)
			if status == 200 {
				assert.ErrorContains(t, err, "does not match its github.com repository IDs")
			} else {
				assert.ErrorContains(t, err, "verifying repository identity")
			}
			assert.Equal(t, raw, readTempLockfilePins(t))
		})
	}
}

func TestProximaIncompleteGenerationDoesNotWrite(t *testing.T) {
	for _, status := range []int{404, 403, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: tenant/internal@v1
`)
			before, err := os.ReadFile(path)
			require.NoError(t, err)
			stdout, _, err := runCommandWithHTTP(t, proximaFixture(t, status),
				"--hostname", "tenant.ghe.com", "--no-narrow", "--no-migrate-local-actions", "--json", path)
			require.ErrorIs(t, err, errSilent)
			var result struct {
				Valid bool `json:"valid"`
			}
			require.NoError(t, json.Unmarshal([]byte(stdout), &result))
			assert.False(t, result.Valid)
			_, err = os.Stat(parserlock.Path)
			assert.True(t, os.IsNotExist(err))
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, before, after)
		})
	}
}

func TestProximaLegacyPinsRequireDotcomIdentity(t *testing.T) {
	for _, matches := range []bool{true, false} {
		t.Run(fmt.Sprintf("matching IDs=%v", matches), func(t *testing.T) {
			path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/public@v2
`, "actions/public@v2=sha1-"+publicSHA)
			raw := strings.ReplaceAll(readTempLockfilePins(t), "v0.0.3", "v0.0.2")
			if matches {
				raw = strings.ReplaceAll(raw, "owner_id: 1", "owner_id: 22")
				raw = strings.ReplaceAll(raw, "repo_id: 1", "repo_id: 222")
			}
			require.NoError(t, os.WriteFile(parserlock.Path, []byte(raw), 0o600))
			_, _, err := runCommandWithHTTP(t, proximaFixture(t, 200),
				"--hostname", "tenant.ghe.com", "--no-narrow", "--no-migrate-local-actions", "--json", path)
			if matches {
				require.NoError(t, err)
				assert.Contains(t, readTempLockfilePins(t), "hostname: 'github.com'")
			} else {
				require.ErrorContains(t, err, "regenerate the lockfile")
				assert.Equal(t, raw, readTempLockfilePins(t))
			}
		})
	}
}
