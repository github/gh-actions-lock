package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
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

// TestParity_TransferredRepoWarns is the #110 case under --verify: the
// locked repository was transferred and kept its repo ID, so the runner
// follows the redirect. The CLI warns with the replacement `uses:` line.
func TestParity_TransferredRepoWarns(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(httpmock.GraphQL(`commit: object\(oid`), transferParity(1))
	path := writeTempWorkflow(t, transferWorkflow, "krzema12/github-actions-typing@v2.2.2=sha1-"+parityTestSHA)

	_, stderr, err := runCommandWithHTTP(t, reg, "--verify", path)
	require.NoError(t, err, stderr)
	assert.Contains(t, stderr, "rewrite it as `uses: typesafegithub/github-actions-typing@v2.2.2`")
	assert.Contains(t, stderr, path)
}

// TestParity_TransferredRepoRewrites: in fix mode a recorded same-ID
// redirect moves `uses:` and the lockfile key to the canonical name.
func TestParity_TransferredRepoRewrites(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(httpmock.GraphQL(`commit: object\(oid`), transferParity(1))
	reg.Register(httpmock.REST("GET", `^/repos/typesafegithub/github-actions-typing$`), httpmock.JSONResponse(map[string]any{
		"full_name": "typesafegithub/github-actions-typing", "id": 1, "owner": map[string]any{"id": 42},
	}))
	path := writeTempWorkflow(t, transferWorkflow, "krzema12/github-actions-typing@v2.2.2=sha1-"+parityTestSHA)

	_, stderr, err := runCommandWithHTTP(t, reg, path)
	require.NoError(t, err, stderr)
	assertTransferRewritten(t, path, stderr)
}

// TestParity_RenameKeepsLockedCommit: a rename is the same repository, so
// the rewrite keeps the recorded commit and its `uses:` even when the
// mutable ref has since moved.
func TestParity_RenameKeepsLockedCommit(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(httpmock.GraphQL(`commit: object\(oid`), func(req *http.Request) (*http.Response, error) {
		resp, err := parityOK(req)
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		body := strings.Replace(string(b), `"krzema12/github-actions-typing"`, `"typesafegithub/github-actions-typing","databaseId":1`, 1)
		resp.Body = io.NopCloser(strings.NewReader(body))
		return resp, nil
	})
	reg.Register(httpmock.GraphQLForRepo("krzema12", "github-actions-typing"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": testRepoResponse("typesafegithub/github-actions-typing", parityTestMoved, nodeActionYAML)},
	}))
	reg.Register(httpmock.REST("GET", `^/repos/typesafegithub/github-actions-typing$`), httpmock.JSONResponse(map[string]any{
		"full_name": "typesafegithub/github-actions-typing", "id": 1, "owner": map[string]any{"id": 42},
	}))
	path := writeTempWorkflow(t, strings.Replace(transferWorkflow, "@v2.2.2", "@main", 1))
	lock := "version: '" + parserlock.Version + "'\n" + `dependencies:
  'krzema12/github-actions-typing@main':
    ref: 'main'
    commit: 'sha1-` + parityTestSHA + `'
    owner_id: 1
    repo_id: 1
    uses:
      - 'actions/setup-node@v4'
  'actions/setup-node@v4':
    ref: 'v4'
    commit: 'sha1-` + parityTestMoved + `'
    owner_id: 2
    repo_id: 2
workflows:
  '.github/workflows/workflow.yml':
    - 'krzema12/github-actions-typing@main'
`
	require.NoError(t, os.WriteFile(filepath.Join(".github", "workflows", "actions.lock"), []byte(lock), 0o600))

	_, stderr, err := runCommandWithHTTP(t, reg, path)
	require.NoError(t, err, stderr)
	wf, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(wf), "uses: typesafegithub/github-actions-typing@main")
	got := readTempLockfilePins(t)
	assert.NotContains(t, got, "krzema12")
	assert.Contains(t, got, `'typesafegithub/github-actions-typing@main':
        ref: 'main'
        commit: 'sha1-`+parityTestSHA+`'`)
	assert.Contains(t, got, `uses:
            - 'actions/setup-node@v4'`)
}

func registerTransferResolve(reg *httpmock.Registry, repoID int) {
	reg.Register(httpmock.GraphQLForRepo("krzema12", "github-actions-typing"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": testRepoResponse("typesafegithub/github-actions-typing", parityTestSHA, nodeActionYAML)},
	}))
	reg.Register(httpmock.REST("GET", `^/repos/typesafegithub/github-actions-typing$`), httpmock.JSONResponse(map[string]any{
		"full_name": "typesafegithub/github-actions-typing", "id": repoID, "owner": map[string]any{"id": 42},
	}))
	reg.Register(httpmock.REST("GET", `^/repos/typesafegithub/github-actions-typing/tags`),
		httpmock.JSONResponse(httpmock.TagListResponse("v2.2.2", parityTestSHA)))
}

func assertTransferRewritten(t *testing.T, path, stderr string) {
	t.Helper()
	wf, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(wf), "uses: typesafegithub/github-actions-typing@v2.2.2")
	lock := readTempLockfilePins(t)
	assert.Contains(t, lock, "typesafegithub/github-actions-typing@v2.2.2")
	assert.NotContains(t, lock, "krzema12")
	assert.Contains(t, stderr, "Rewrote krzema12/github-actions-typing@v2.2.2 → typesafegithub/github-actions-typing@v2.2.2 in "+path)
}

// TestParity_RepoIDMismatchSameNameBlocks: the name still resolves, but to
// a different repository. Identity is the repo ID alone.
func TestParity_RepoIDMismatchSameNameBlocks(t *testing.T) {
	for name, mode := range map[string][]string{"default": nil, "verify": {"--verify"}} {
		t.Run(name, func(t *testing.T) {
			reg := &httpmock.Registry{}
			defer reg.Verify(t)
			reg.Register(httpmock.GraphQL(`commit: object\(oid`), httpmock.JSONResponse(map[string]any{
				"data": map[string]any{"a0": map[string]any{
					"nameWithOwner": "actions/checkout",
					"databaseId":    99,
					"commit":        map[string]any{"oid": parityTestSHA},
					"tag":           map[string]any{"oid": parityTestSHA},
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

			_, stderr, err := runCommandWithHTTP(t, reg, append(mode, path)...)
			require.ErrorIs(t, err, errSilent)
			assert.Contains(t, stderr, "different repository than the one locked (repo ID 99, locked 1)")
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

// TestParity_FirstRunTransferRewrites is #110 on first generation:
// resolution follows the rename, the workflow and lockfile take the
// canonical name, and the same-run parity check costs one request.
func TestParity_FirstRunTransferRewrites(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(httpmock.GraphQL(`commit: object\(oid`), transferParity(502427408))
	registerTransferResolve(reg, 502427408)
	path := writeTempWorkflow(t, transferWorkflow)

	ct := &countingTransport{inner: reg}
	_, stderr, err := runCommandWithHTTP(t, ct, path)
	require.NoError(t, err, stderr)
	assertTransferRewritten(t, path, stderr)
	assert.NotContains(t, stderr, "rewrite it as")
	assert.Equal(t, 2, countGraphQL(ct), "one resolve + one parity request: %v", ct.urls)
	assert.Contains(t, readTempLockfilePins(t), "repo_id: 502427408")
}

// TestParity_RemoteCompositeTransferBlocks: a redirect inside a composite
// we don't own can't be rewritten here, so nothing is written.
func TestParity_RemoteCompositeTransferBlocks(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(httpmock.GraphQLForRepo("acme", "comp"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": testRepoResponse("acme/comp", parityTestMoved,
			"runs:\n  using: composite\n  steps:\n    - uses: krzema12/github-actions-typing@v2.2.2\n")},
	}))
	reg.Register(httpmock.GraphQLForRepo("krzema12", "github-actions-typing"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": testRepoResponse("typesafegithub/github-actions-typing", parityTestSHA, nodeActionYAML)},
	}))
	path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: acme/comp@v1.0.0
`)

	_, stderr, err := runCommandWithHTTP(t, reg, path)
	require.ErrorIs(t, err, errSilent)
	assert.Contains(t, stderr, "krzema12/github-actions-typing was renamed or transferred to typesafegithub/github-actions-typing; upstream composite acme/comp@v1.0.0 must update")
	assert.NoFileExists(t, ".github/workflows/actions.lock")
}

// TestParity_FirstRunInconclusiveNotWritten: a fresh pin whose identity
// can't be confirmed fails closed.
func TestParity_FirstRunInconclusiveNotWritten(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(httpmock.GraphQLForRepo("actions", "checkout"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": testRepoResponse("actions/checkout", parityTestSHA, nodeActionYAML)},
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

	_, stderr, err := runCommandWithHTTP(t, parityDown{reg}, path)
	require.ErrorIs(t, err, errSilent)
	assert.Contains(t, stderr, "could not verify locked actions/checkout")
	assert.NoFileExists(t, ".github/workflows/actions.lock")
}

// TestParity_FirstRunMixedCaseRefChecked: refs are case-sensitive, so a
// fresh `@Release` must still reach the parity check and fail closed.
func TestParity_FirstRunMixedCaseRefChecked(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(httpmock.GraphQLForRepo("actions", "checkout"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": testRepoResponse("actions/checkout", parityTestSHA, nodeActionYAML)},
	}))
	reg.Register(httpmock.REST("GET", `^/repos/actions/checkout$`), httpmock.JSONResponse(map[string]any{
		"full_name": "actions/checkout", "id": 1, "owner": map[string]any{"id": 2},
	}))
	path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@Release
`)

	_, stderr, err := runCommandWithHTTP(t, parityDown{reg}, "--no-narrow", path)
	require.ErrorIs(t, err, errSilent)
	assert.Contains(t, stderr, "could not verify locked")
	assert.NoFileExists(t, ".github/workflows/actions.lock")
}

// parityDown fails every parity query with a server error.
type parityDown struct{ inner http.RoundTripper }

func (p parityDown) RoundTrip(req *http.Request) (*http.Response, error) {
	if httpmock.GraphQL(`commit: object\(oid`)(req) {
		return httpmock.StatusResponse(http.StatusBadGateway)(req)
	}
	return p.inner.RoundTrip(req)
}

func countGraphQL(ct *countingTransport) int {
	n := 0
	for _, u := range ct.urls {
		if u == "/graphql" {
			n++
		}
	}
	return n
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
	assert.Equal(t, 2, countGraphQL(ct), "resolve + parity: %v", ct.urls)
	assert.Contains(t, readTempLockfilePins(t), "actions/checkout@v4.2.1")
}
