package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type collisionTransport struct {
	parityChecks int
	resolutions  int
}

func (c *collisionTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	switch {
	case httpmock.GraphQL(`commit: object\(oid`)(req):
		c.parityChecks++
		return httpmock.JSONResponse(map[string]any{"data": map[string]any{"a0": map[string]any{
			"nameWithOwner": "actions/checkout",
			"commit":        map[string]any{"oid": parityTestSHA},
			"tag":           map[string]any{"oid": parityTestSHA},
			"head":          map[string]any{"oid": parityTestMoved},
		}}})(req)
	case httpmock.GraphQL(`file: file\(path`)(req):
		c.resolutions++
		response := testRepoResponse("actions/checkout", parityTestSHA, nodeActionYAML)
		response["branch"] = map[string]any{"oid": parityTestMoved}
		return httpmock.JSONResponse(map[string]any{"data": map[string]any{"a0": response}})(req)
	case req.URL.Path == "/repos/actions/checkout":
		return httpmock.JSONResponse(map[string]any{"full_name": "actions/checkout", "id": 1, "owner": map[string]any{"id": 1}})(req)
	case req.URL.Path == "/repos/actions/checkout/commits/refs/tags/v4.2.2":
		c.resolutions++
		return httpmock.JSONResponse(map[string]any{"sha": parityTestSHA})(req)
	case req.URL.Path == "/repos/actions/checkout/commits/refs/heads/v4.2.2":
		return httpmock.JSONResponse(map[string]any{"sha": parityTestMoved})(req)
	}
	return nil, fmt.Errorf("unexpected collision fixture request: %s %s", req.Method, req.URL)
}

func TestRefCollisionBlocksWrites(t *testing.T) {
	for _, proxy := range []bool{false, true} {
		for _, recorded := range []bool{false, true} {
			for _, mode := range []string{"default", "--verify", "--no-fix", "--relock", "--accept-moved"} {
				t.Run(fmt.Sprintf("proxy=%t/recorded=%t/%s", proxy, recorded, mode), func(t *testing.T) {
					var pins []string
					if recorded {
						pins = []string{"actions/checkout@v4.2.2=sha1-" + parityTestSHA}
					}
					path := writeTempWorkflow(t, "jobs:\n  build:\n    steps:\n      - uses: actions/checkout@v4.2.2\n", pins...)
					before, err := os.ReadFile(path)
					require.NoError(t, err)
					var lockBefore string
					if recorded {
						lockBefore = readTempLockfilePins(t)
					}
					rt := &collisionTransport{}
					if proxy {
						t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
						useDefaultTransport(t, rt)
					}
					args := []string{"--no-narrow", "--no-interactive"}
					if mode != "default" {
						args = append(args, mode)
					}
					out, err := runParity(t, rt, append(args, path)...)
					require.ErrorIs(t, err, errSilent)
					assert.False(t, out.Valid)
					found := false
					for _, f := range out.Findings {
						if f.Category == "unreachable-pin" {
							found = true
							assert.Equal(t, "error", f.Severity)
							assert.Contains(t, f.Detail, "tag and branch point at different commits")
							assert.Contains(t, f.Remediation, "distinct tag or branch")
							assert.NotContains(t, f.Remediation, "--relock")
						}
					}
					assert.True(t, found, "%+v", out.Findings)
					assert.Positive(t, rt.parityChecks+rt.resolutions, "the collision must be checked, not assumed by the fixture")
					after, err := os.ReadFile(path)
					require.NoError(t, err)
					assert.Equal(t, string(before), string(after))
					if recorded {
						assert.Equal(t, lockBefore, readTempLockfilePins(t))
					} else {
						assert.NoFileExists(t, filepath.Join(filepath.Dir(path), "actions.lock"))
					}
				})
			}
		}
	}
}

func TestRemoteDockerWarningsStayTerminalOnly(t *testing.T) {
	for _, verify := range []bool{false, true} {
		for _, jsonOutput := range []bool{false, true} {
			t.Run(fmt.Sprintf("verify=%t/json=%t", verify, jsonOutput), func(t *testing.T) {
				var pins []string
				if verify {
					pins = []string{"actions/checkout@v4.2.2=sha1-" + parityTestSHA}
				}
				path := writeTempWorkflow(t, "jobs:\n  build:\n    steps:\n      - uses: actions/checkout@v4.2.2\n", pins...)
				rt := proximaTransport(func(req *http.Request) (*http.Response, error) {
					switch {
					case httpmock.GraphQL(`commit: object\(oid`)(req):
						return parityOK(req)
					case httpmock.GraphQL(`file: file\(path`)(req):
						return httpmock.JSONResponse(map[string]any{"data": map[string]any{
							"a0": testRepoResponse("actions/checkout", parityTestSHA, "runs:\n  using: docker\n  image: docker://alpine:3\n"),
						}})(req)
					case req.URL.Path == "/repos/actions/checkout":
						return httpmock.JSONResponse(map[string]any{"id": 1, "owner": map[string]any{"id": 1}})(req)
					case strings.HasSuffix(req.URL.Path, "/releases"):
						return httpmock.JSONResponse([]any{})(req)
					}
					return nil, fmt.Errorf("unexpected Docker fixture request: %s %s", req.Method, req.URL)
				})
				args := []string{"--no-narrow", "--no-interactive"}
				if verify {
					args = append(args, "--verify")
				}
				if jsonOutput {
					args = append(args, "--json=valid,findings")
				}
				stdout, stderr, err := runCommandWithHTTP(t, rt, append(args, path)...)
				require.NoError(t, err, stderr)
				if jsonOutput {
					var out parityPayload
					require.NoError(t, json.Unmarshal([]byte(stdout), &out))
					assert.Equal(t, verify, out.Valid, "JSON describes pre-fix findings; exit status describes the completed fix")
					assert.NotContains(t, stdout+stderr, "container image tags")
					assert.NotContains(t, stdout, "docker://")
				} else {
					assert.Contains(t, stderr, "docker://alpine:3 (pulled by actions/checkout@v4.2.2)")
					assert.Contains(t, stderr, "container image tags are mutable")
				}
			})
		}
	}
}
