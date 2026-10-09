package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/github/gh-actions-lock/cmd/gh-actions-lock/format"
	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	parityTestSHA   = "9ddf35b71a482be7d8922b28e8d00df16b77e315"
	parityTestMoved = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

type parityPayload struct {
	Valid    bool             `json:"valid"`
	Findings []format.Finding `json:"findings"`
}

func runParity(t *testing.T, rt http.RoundTripper, args ...string) (parityPayload, error) {
	t.Helper()
	stdout, _, err := runCommandWithHTTP(t, rt, append(args, "--json=valid,findings")...)
	var p parityPayload
	require.NoError(t, json.Unmarshal([]byte(stdout), &p), stdout)
	return p, err
}

const transferWorkflow = `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: krzema12/github-actions-typing@v2.2.2
`

func transferParity(repoID int) httpmock.Responder {
	return httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": map[string]any{
			"nameWithOwner": "typesafegithub/github-actions-typing",
			"databaseId":    repoID,
			"owner":         map[string]any{"databaseId": 42},
			"commit":        map[string]any{"oid": parityTestSHA},
			"tag":           map[string]any{"oid": parityTestSHA},
		}},
	})
}

// TestParity_TransferredRepoWarns is the #110 case: the locked repository
// was transferred and kept its repo ID, so the runner follows the redirect.
// The CLI warns with the replacement `uses:` line instead of blocking.
func TestParity_TransferredRepoWarns(t *testing.T) {
	for name, mode := range map[string][]string{"default": nil, "verify": {"--verify"}} {
		t.Run(name, func(t *testing.T) {
			reg := &httpmock.Registry{}
			defer reg.Verify(t)
			reg.Register(httpmock.GraphQL(`commit: object\(oid`), transferParity(1))
			path := writeTempWorkflow(t, transferWorkflow, "krzema12/github-actions-typing@v2.2.2=sha1-"+parityTestSHA)

			_, stderr, err := runCommandWithHTTP(t, reg, append(mode, path)...)
			require.NoError(t, err, stderr)
			assert.Contains(t, stderr, "replace with `uses: typesafegithub/github-actions-typing@v2.2.2`")
			assert.Contains(t, stderr, path)
		})
	}
}

// TestParity_ReclaimedNameBlocks: the name now resolves to a different repo
// ID, so someone else owns it. Both modes block and say why on the terminal.
func TestParity_ReclaimedNameBlocks(t *testing.T) {
	for name, mode := range map[string][]string{"default": nil, "verify": {"--verify"}} {
		t.Run(name, func(t *testing.T) {
			reg := &httpmock.Registry{}
			defer reg.Verify(t)
			reg.Register(httpmock.GraphQL(`commit: object\(oid`), transferParity(99))
			path := writeTempWorkflow(t, transferWorkflow, "krzema12/github-actions-typing@v2.2.2=sha1-"+parityTestSHA)

			_, stderr, err := runCommandWithHTTP(t, reg, append(mode, path)...)
			require.ErrorIs(t, err, errSilent)
			assert.Contains(t, stderr, "different repository than the one locked")
		})
	}
}

// TestParity_RelockReclaimedNameKeepsLock: re-resolving a reclaimed name
// yields an attacker's commit. The recorded repo ID must catch it before
// the new commit reaches the lockfile.
func TestParity_RelockReclaimedNameKeepsLock(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(httpmock.GraphQL(`commit: object\(oid`), transferParity(99))
	reg.Register(httpmock.GraphQLForRepo("krzema12", "github-actions-typing"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": testRepoResponse("krzema12/github-actions-typing", parityTestMoved, nodeActionYAML)},
	}))
	path := writeTempWorkflow(t, transferWorkflow, "krzema12/github-actions-typing@v2.2.2=sha1-"+parityTestSHA)

	// The tag fast-forwarded, so only the repo ID gives the takeover away.
	reg.Register(httpmock.REST("GET", `/compare/`), httpmock.JSONResponse(map[string]any{"status": "ahead", "merge_base_commit": map[string]any{"sha": parityTestSHA}}))
	reg.Register(httpmock.REST("GET", `^/repos/krzema12/github-actions-typing/tags`),
		httpmock.JSONResponse(httpmock.TagListResponse("v2.2.2", parityTestMoved)))

	_, stderr, err := runCommandWithHTTP(t, reg, "--relock", path)
	require.ErrorIs(t, err, errSilent)
	assert.Contains(t, stderr, "different repository than the one locked")
	lock := readTempLockfilePins(t)
	assert.Contains(t, lock, parityTestSHA)
	assert.NotContains(t, lock, parityTestMoved, "the reclaimed repo's commit must not be written")
}

func TestParity_MovedExactTagBlocks(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(httpmock.GraphQL(`commit: object\(oid`), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": map[string]any{
			"nameWithOwner": "actions/checkout",
			"commit":        map[string]any{"oid": parityTestSHA},
			"tag":           map[string]any{"oid": parityTestMoved},
		}},
	}))
	path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4.2.1
`, "actions/checkout@v4.2.1=sha1-"+parityTestSHA)

	p, err := runParity(t, reg, "--verify", path)
	require.ErrorIs(t, err, errSilent)
	require.Len(t, p.Findings, 1)
	assert.Equal(t, "unreachable-pin", p.Findings[0].Category)
}

// TestParity_DefaultRunPrintsParityFinding: parity findings have no pin
// record entry, so the fix-mode summary must still print them.
func TestParity_DefaultRunPrintsParityFinding(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(httpmock.GraphQL(`commit: object\(oid`), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": map[string]any{
			"nameWithOwner": "actions/checkout",
			"commit":        map[string]any{"oid": parityTestSHA},
			"tag":           map[string]any{"oid": parityTestMoved},
		}},
	}))
	path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4.2.1
`, "actions/checkout@v4.2.1=sha1-"+parityTestSHA)

	_, stderr, err := runCommandWithHTTP(t, reg, path)
	require.ErrorIs(t, err, errSilent)
	assert.Contains(t, stderr, "tag v4.2.1 now points at bbbbbbb")
}

// TestParity_UnchangedRerunRequestCount guards API kindness: an unchanged
// rerun re-resolves nothing and costs one GraphQL request for the whole
// closure. A mutable ref that moved upstream stays locked (sticky).
func TestParity_UnchangedRerunRequestCount(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	path := writeParityFixture(t)

	for _, args := range [][]string{{path}, {"--verify", path}} {
		registerParity(reg)
		ct := &countingTransport{inner: reg}
		p, err := runParity(t, ct, args...)
		require.NoError(t, err)
		assert.True(t, p.Valid, "%+v", p.Findings)
		assert.Equal(t, 1, ct.n, "%v: one parity request for every pin", args)
	}
}

// TestParity_UnchangedRerunRequestCountRESTOnly covers the Dependabot proxy
// path: one repos/{owner}/{repo} call per unique repository plus one commits
// call per pin.
func TestParity_UnchangedRerunRequestCountRESTOnly(t *testing.T) {
	t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	// Anonymous REST calls use http.DefaultClient.
	ct := &countingTransport{inner: reg}
	orig := http.DefaultTransport
	http.DefaultTransport = ct
	t.Cleanup(func() { http.DefaultTransport = orig })

	for _, repo := range []string{"checkout", "setup-go"} {
		reg.Register(httpmock.REST("GET", `^/repos/actions/`+repo+`$`), httpmock.JSONResponse(map[string]any{
			"full_name": "actions/" + repo, "id": 1, "owner": map[string]any{"id": 1},
		}))
	}
	commit := httpmock.JSONResponse(map[string]any{"sha": parityTestSHA})
	reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/commits/`+parityTestSHA+`$`), commit)
	reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/commits/v4\.2\.1$`), commit)
	reg.Register(httpmock.REST("GET", `^/repos/actions/setup-go/commits/`+parityTestSHA+`$`), commit)
	path := writeParityFixture(t)

	p, err := runParity(t, ct, path)
	require.NoError(t, err)
	assert.True(t, p.Valid, "%+v", p.Findings)
	assert.Equal(t, 5, ct.n, "2 repos + 3 pins")
}

// countingTransport counts every request, matched or not.
type countingTransport struct {
	inner http.RoundTripper
	n     int
	urls  []string
}

func (c *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.n++
	c.urls = append(c.urls, req.URL.Path)
	return c.inner.RoundTrip(req)
}

func writeParityFixture(t *testing.T) string {
	t.Helper()
	return writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/checkout@v4.2.1
      - uses: actions/setup-go@v5
`,
		"actions/checkout@v4=sha1-"+parityTestSHA,
		"actions/checkout@v4.2.1=sha1-"+parityTestSHA,
		"actions/setup-go@v5=sha1-"+parityTestSHA,
	)
}

// TestParity_FirstRunTransferWarns is #110 on first generation: resolution
// follows the rename, and the same-run parity check names the new location
// while the pin is still written, as the runner would accept it.
func TestParity_FirstRunTransferWarns(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(httpmock.GraphQL(`commit: object\(oid`), transferParity(502427408))
	reg.Register(httpmock.GraphQLForRepo("krzema12", "github-actions-typing"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": testRepoResponse("typesafegithub/github-actions-typing", parityTestSHA, nodeActionYAML)},
	}))
	reg.Register(httpmock.REST("GET", `^/repos/krzema12/github-actions-typing$`), httpmock.JSONResponse(map[string]any{
		"full_name": "typesafegithub/github-actions-typing", "id": 502427408, "owner": map[string]any{"id": 42},
	}))
	reg.Register(httpmock.REST("GET", `^/repos/krzema12/github-actions-typing/tags`),
		httpmock.JSONResponse(httpmock.TagListResponse("v2.2.2", parityTestSHA)))
	path := writeTempWorkflow(t, transferWorkflow)

	ct := &countingTransport{inner: reg}
	_, stderr, err := runCommandWithHTTP(t, ct, path)
	require.NoError(t, err, stderr)
	assert.Contains(t, stderr, "replace with `uses: typesafegithub/github-actions-typing@v2.2.2`")
	graphql := 0
	for _, u := range ct.urls {
		if u == "/graphql" {
			graphql++
		}
	}
	assert.Equal(t, 2, graphql, "one resolve + one parity request: %v", ct.urls)
	assert.Contains(t, readTempLockfilePins(t), "repo_id: 502427408")
}

// TestParity_FirstRunRequestCount: generation adds exactly one GraphQL
// parity request on top of resolution, and the pin is written.
func TestParity_FirstRunRequestCount(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	registerParity(reg)
	reg.Register(httpmock.GraphQLForRepo("actions", "checkout"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": testRepoResponse("actions/checkout", parityTestSHA, nodeActionYAML)},
	}))
	reg.Register(httpmock.REST("GET", `^/repos/actions/checkout$`), httpmock.JSONResponse(map[string]any{
		"full_name": "actions/checkout", "id": 1, "owner": map[string]any{"id": 1},
	}))
	reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/tags`),
		httpmock.JSONResponse(httpmock.TagListResponse("v4.2.1", parityTestSHA)))
	path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4.2.1
`)

	ct := &countingTransport{inner: reg}
	_, _, err := runCommandWithHTTP(t, ct, path)
	require.NoError(t, err)
	graphql := 0
	for _, u := range ct.urls {
		if u == "/graphql" {
			graphql++
		}
	}
	assert.Equal(t, 2, graphql, "resolve + parity: %v", ct.urls)
	assert.Contains(t, readTempLockfilePins(t), "actions/checkout@v4.2.1")
}
