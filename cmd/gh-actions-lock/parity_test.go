package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
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

func registerNodeManifests(reg *httpmock.Registry) {
	reg.Register(httpmock.GraphQL(`file: file\(path`), func(req *http.Request) (*http.Response, error) {
		var body struct {
			Variables map[string]string `json:"variables"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		data := map[string]any{}
		for i := 0; ; i++ {
			owner, ok := body.Variables[fmt.Sprintf("owner%d", i)]
			if !ok {
				break
			}
			data[fmt.Sprintf("a%d", i)] = testRepoResponse(
				owner+"/"+body.Variables[fmt.Sprintf("name%d", i)],
				strings.TrimSuffix(body.Variables[fmt.Sprintf("expr%d", i)], "^{commit}"),
				nodeActionYAML,
			)
		}
		return httpmock.JSONResponse(map[string]any{"data": data})(req)
	})
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
	registerNodeManifests(reg)
	path := writeTempWorkflow(t, transferWorkflow, "krzema12/github-actions-typing@v2.2.2=sha1-"+parityTestSHA)

	_, stderr, err := runCommandWithHTTP(t, reg, "--verify", path)
	require.NoError(t, err, stderr)
	assert.Contains(t, stderr, "rewrite it as `uses: typesafegithub/github-actions-typing@v2.2.2`")
	assert.Contains(t, stderr, path)
	assert.Contains(t, stderr, "All 1 workflow valid")
}

// TestParity_TransferredRepoRewrites: in fix mode a recorded same-ID
// redirect moves `uses:` and the lockfile key to the canonical name.
func TestParity_TransferredRepoRewrites(t *testing.T) {
	reg := &httpmock.Registry{}
	registerNodeManifests(reg)
	defer reg.Verify(t)
	reg.Register(httpmock.GraphQL(`commit: object\(oid`), transferParity(1))
	reg.Register(httpmock.REST("GET", `^/repos/typesafegithub/github-actions-typing$`), httpmock.JSONResponse(map[string]any{
		"full_name": "typesafegithub/github-actions-typing", "id": 1, "owner": map[string]any{"id": 42},
	}))
	path := writeTempWorkflow(t, transferWorkflow, "krzema12/github-actions-typing@v2.2.2=sha1-"+parityTestSHA)

	_, stderr, err := runCommandWithHTTP(t, reg, path)
	require.NoError(t, err, stderr)
	assertTransferRewritten(t, path, stderr)
	assert.NotContains(t, stderr, "rewrite it as", "the run already rewrote it")
}

// TestParity_MixedCaseRenameRewrites: the lockfile key is lowercase but
// `uses:` isn't; the rewrite must match the workflow's own text.
func TestParity_MixedCaseRenameRewrites(t *testing.T) {
	reg := &httpmock.Registry{}
	registerNodeManifests(reg)
	reg.Register(httpmock.GraphQL(`commit: object\(oid`), transferParity(1))
	reg.Register(httpmock.REST("GET", `^/repos/typesafegithub/github-actions-typing$`), httpmock.JSONResponse(map[string]any{
		"full_name": "typesafegithub/github-actions-typing", "id": 1, "owner": map[string]any{"id": 42},
	}))
	path := writeTempWorkflow(t, strings.Replace(transferWorkflow, "krzema12/github-actions-typing", "Krzema12/GitHub-Actions-Typing", 1),
		"krzema12/github-actions-typing@v2.2.2=sha1-"+parityTestSHA)

	_, stderr, err := runCommandWithHTTP(t, reg, path)
	require.NoError(t, err, stderr)
	wf, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(wf), "uses: typesafegithub/github-actions-typing@v2.2.2")
	assert.NotContains(t, readTempLockfilePins(t), "krzema12")
}

// TestParity_RenameKeepsLockedCommit: a rename is the same repository, so
// the rewrite keeps the recorded commit and its `uses:` even when the
// mutable ref has since moved.
func TestParity_RenameKeepsLockedCommit(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(httpmock.GraphQL(`commit: object\(oid`), renamedParity)
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

// renamedParity answers like parityOK, with krzema12/github-actions-typing
// redirected to typesafegithub under the same repo ID.
func renamedParity(req *http.Request) (*http.Response, error) {
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
// rerun re-resolves nothing and batches parity plus immutable manifests.
// A mutable ref that moved upstream stays locked and adds one compare call.
func TestParity_UnchangedRerunRequestCount(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	path := writeParityFixture(t)

	for _, args := range [][]string{{path}, {"--verify", path}} {
		registerParity(reg)
		registerNodeManifests(reg)
		ct := &countingTransport{inner: reg}
		p, err := runParity(t, ct, args...)
		require.NoError(t, err)
		assert.True(t, p.Valid, "%+v", p.Findings)
		assert.Equal(t, 2, ct.n, "%v: parity and immutable manifest batches", args)
	}
}

// TestParity_UnchangedRerunRequestCountRESTOnly covers the Dependabot proxy
// path: one repos/{owner}/{repo} call per unique repository, one commits
// call per exact tag, and two per mutable ref (the commit, then the ref).
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
	reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/commits/refs/tags/v4$`), commit)
	reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/commits/refs/heads/v4$`), httpmock.StatusResponse(http.StatusNotFound))
	reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/commits/refs/tags/v4\.2\.1$`), commit)
	reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/commits/refs/heads/v4\.2\.1$`), httpmock.StatusResponse(http.StatusNotFound))
	reg.Register(httpmock.REST("GET", `^/repos/actions/setup-go/commits/`+parityTestSHA+`$`), commit)
	reg.Register(httpmock.REST("GET", `^/repos/actions/setup-go/commits/refs/tags/v5$`), commit)
	reg.Register(httpmock.REST("GET", `^/repos/actions/setup-go/commits/refs/heads/v5$`), httpmock.StatusResponse(http.StatusNotFound))
	for _, repo := range []string{"checkout", "setup-go"} {
		reg.Register(httpmock.REST("GET", `^/repos/actions/`+repo+`/commits/`+parityTestSHA+`$`), commit)
		reg.Register(httpmock.REST("GET", `^/repos/actions/`+repo+`/contents/action\.yml$`),
			httpmock.JSONResponse(map[string]any{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(nodeActionYAML))}))
	}
	reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/commits/`+parityTestSHA+`$`), commit)
	reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/contents/action\.yml$`),
		httpmock.JSONResponse(map[string]any{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(nodeActionYAML))}))
	path := writeParityFixture(t)

	p, err := runParity(t, ct, path)
	require.NoError(t, err, "findings: %+v; requests: %v", p.Findings, ct.urls)
	assert.True(t, p.Valid, "%+v", p.Findings)
	assert.Equal(t, 16, ct.n, "parity plus immutable commit and manifest lookups for three refs: %v", ct.urls)
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
// TestParity_RemoteCompositeRenameKeepsName: a redirect inside a remote
// composite can't be rewritten and the runner follows it while the repo ID
// matches, so the child stays keyed as written and only --verify warns.
func TestParity_RemoteCompositeRenameKeepsName(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(httpmock.GraphQLForRepo("acme", "comp"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": testRepoResponse("acme/comp", parityTestMoved,
			"runs:\n  using: composite\n  steps:\n    - uses: krzema12/github-actions-typing@v2.2.2\n")},
	}))
	reg.Register(httpmock.GraphQLForRepo("krzema12", "github-actions-typing"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": testRepoResponse("typesafegithub/github-actions-typing", parityTestSHA, nodeActionYAML)},
	}))
	reg.Register(httpmock.GraphQL(`commit: object\(oid`), renamedParity)
	reg.Register(httpmock.REST("GET", `^/repos/acme/comp$`), httpmock.JSONResponse(map[string]any{
		"full_name": "acme/comp", "id": 3, "owner": map[string]any{"id": 4},
	}))
	reg.Register(httpmock.REST("GET", `^/repos/krzema12/github-actions-typing$`), httpmock.JSONResponse(map[string]any{
		"full_name": "typesafegithub/github-actions-typing", "id": 1, "owner": map[string]any{"id": 42},
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
	require.NoError(t, err, stderr)
	assert.NotContains(t, stderr, "upgrade acme/comp@v1.0.0", "fix runs stay quiet; --verify reports it")
	lock := readTempLockfilePins(t)
	assert.Contains(t, lock, "'krzema12/github-actions-typing@v2.2.2':")
	assert.NotContains(t, lock, "typesafegithub")

	reg.Register(httpmock.GraphQL(`commit: object\(oid`), renamedParity)
	reg.Register(httpmock.GraphQLForRepo("acme", "comp"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": testRepoResponse("acme/comp", parityTestMoved,
			"runs:\n  using: composite\n  steps:\n    - uses: krzema12/github-actions-typing@v2.2.2\n")},
	}))
	reg.Register(httpmock.GraphQLForRepo("krzema12", "github-actions-typing"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": testRepoResponse("typesafegithub/github-actions-typing", parityTestSHA, nodeActionYAML)},
	}))
	_, stderr, err = runCommandWithHTTP(t, reg, "--verify", path)
	require.NoError(t, err, stderr)
	assert.Contains(t, stderr, "upgrade acme/comp@v1.0.0")
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

// TestParity_VerifyHintNamesVerify: a fixable --verify failure points at
// the flag the user actually passed.
func TestParity_VerifyHintNamesVerify(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(httpmock.GraphQLForRepo("actions", "checkout"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": testRepoResponse("actions/checkout", parityTestSHA, nodeActionYAML)},
	}))
	reg.Register(httpmock.GraphQL(`commit: object\(oid`), parityOK)
	path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
`)

	_, stderr, err := runCommandWithHTTP(t, reg, "--verify", path)
	require.ErrorIs(t, err, errSilent)
	assert.Contains(t, stderr, "Re-run without --verify to apply fixes.")
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

// useDefaultTransport routes anonymous REST calls, which use
// http.DefaultTransport, through rt for the rest of the test.
func useDefaultTransport(t *testing.T, rt http.RoundTripper) {
	t.Helper()
	orig := http.DefaultTransport
	http.DefaultTransport = rt
	t.Cleanup(func() { http.DefaultTransport = orig })
}

// TestParity_DependabotProxyInconclusiveAborts: under the Dependabot proxy
// a rate limit or server error on a recorded pin aborts with exit 2 so
// Dependabot retries, instead of passing on a warning. Outside the proxy
// the same failure stays a warning.
func TestParity_DependabotProxyInconclusiveAborts(t *testing.T) {
	for name, tc := range map[string]struct {
		resp   httpmock.Responder
		status string
	}{
		"rate-limited 403":           {httpmock.StatusResponseWithHeaders(http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "0"}), "HTTP 403"},
		"secondary rate-limited 403": {httpmock.StatusResponseWithHeaders(http.StatusForbidden, map[string]string{"Retry-After": "60"}), "HTTP 403"},
		"503":                        {httpmock.StatusResponse(http.StatusServiceUnavailable), "HTTP 503"},
	} {
		t.Run(name, func(t *testing.T) {
			for _, args := range [][]string{nil, {"--verify"}} {
				t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
				reg := &httpmock.Registry{}
				reg.Register(httpmock.REST("GET", `^/repos/actions/checkout$`), tc.resp)
				useDefaultTransport(t, reg)
				path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4.2.1
`, "actions/checkout@v4.2.1=sha1-"+parityTestSHA)
				lockBefore := readTempLockfilePins(t)
				wfBefore, err := os.ReadFile(path)
				require.NoError(t, err)

				_, stderr, err := runCommandWithHTTP(t, reg, append(args, path)...)
				require.Error(t, err, stderr)
				assert.NotErrorIs(t, err, errSilent, "a tool failure exits 2")
				assert.Contains(t, err.Error(), tc.status)
				assert.Contains(t, err.Error(), "actions/checkout")
				assert.Equal(t, lockBefore, readTempLockfilePins(t))
				wfAfter, err := os.ReadFile(path)
				require.NoError(t, err)
				assert.Equal(t, string(wfBefore), string(wfAfter))
			}
		})
	}
}

func TestParity_InconclusiveWarnsWithoutProxy(t *testing.T) {
	reg := &httpmock.Registry{}
	registerNodeManifests(reg)
	path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4.2.1
`, "actions/checkout@v4.2.1=sha1-"+parityTestSHA)

	_, stderr, err := runCommandWithHTTP(t, parityDown{reg}, "--verify", path)
	require.NoError(t, err, stderr)
	assert.Contains(t, stderr, "could not verify locked actions/checkout")
}

// TestParity_DependabotProxyKeepsRenamedName: Dependabot reads back only
// actions.lock, so under the proxy a same-ID rename must not rewrite
// `uses:` or re-key the pin; the warning still reaches --json.
func TestParity_DependabotProxyKeepsRenamedName(t *testing.T) {
	t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
	reg := &httpmock.Registry{}
	reg.Register(httpmock.REST("GET", `^/repos/krzema12/github-actions-typing$`), httpmock.JSONResponse(map[string]any{
		"full_name": "typesafegithub/github-actions-typing", "id": 1, "owner": map[string]any{"id": 42},
	}))
	reg.Register(httpmock.REST("GET", `^/repos/krzema12/github-actions-typing/commits/refs/tags/v2\.2\.2$`),
		httpmock.JSONResponse(map[string]any{"sha": parityTestSHA}))
	reg.Register(httpmock.REST("GET", `^/repos/krzema12/github-actions-typing/commits/refs/heads/v2\.2\.2$`), httpmock.StatusResponse(http.StatusNotFound))
	reg.Register(httpmock.REST("GET", `^/repos/krzema12/github-actions-typing/commits/`+parityTestSHA+`$`),
		httpmock.JSONResponse(map[string]any{"sha": parityTestSHA}))
	reg.Register(httpmock.REST("GET", `^/repos/krzema12/github-actions-typing/contents/action\.yml$`),
		httpmock.JSONResponse(map[string]any{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(nodeActionYAML))}))
	reg.Register(httpmock.REST("GET", `^/repos/typesafegithub/github-actions-typing/commits/`+parityTestSHA+`$`),
		httpmock.JSONResponse(map[string]any{"sha": parityTestSHA}))
	reg.Register(httpmock.REST("GET", `^/repos/typesafegithub/github-actions-typing/contents/action\.yml$`),
		httpmock.JSONResponse(map[string]any{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(nodeActionYAML))}))
	useDefaultTransport(t, reg)
	path := writeTempWorkflow(t, transferWorkflow, "krzema12/github-actions-typing@v2.2.2=sha1-"+parityTestSHA)
	wfBefore, err := os.ReadFile(path)
	require.NoError(t, err)

	stdout, stderr, err := runCommandWithHTTP(t, reg, "--no-onboard", "--no-narrow", "--no-interactive", "--json=findings", path)
	require.NoError(t, err, stderr)
	var p parityPayload
	require.NoError(t, json.Unmarshal([]byte(stdout), &p), stdout)
	var renamed bool
	for _, f := range p.Findings {
		renamed = renamed || f.Category == "repo-renamed"
	}
	assert.True(t, renamed, "%+v", p.Findings)
	wfAfter, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(wfBefore), string(wfAfter))
	lock := readTempLockfilePins(t)
	assert.Contains(t, lock, "'krzema12/github-actions-typing@v2.2.2':")
	assert.Contains(t, lock, "sha1-"+parityTestSHA)
	assert.NotContains(t, lock, "typesafegithub")
}

// TestParity_DependabotProxyFirstRunKeepsRenamedName: the same holds when
// resolution itself follows the rename on first generation.
func TestParity_DependabotProxyFirstRunKeepsRenamedName(t *testing.T) {
	t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
	reg := &httpmock.Registry{}
	reg.Register(httpmock.REST("GET", `^/repos/krzema12/github-actions-typing$`), httpmock.JSONResponse(map[string]any{
		"full_name": "typesafegithub/github-actions-typing", "id": 1, "owner": map[string]any{"id": 42},
	}))
	commit := httpmock.JSONResponse(map[string]any{"sha": parityTestSHA})
	reg.Register(httpmock.REST("GET", `^/repos/typesafegithub/github-actions-typing/commits/refs/tags/v2\.2\.2$`), commit)
	reg.Register(httpmock.REST("GET", `^/repos/typesafegithub/github-actions-typing/commits/refs/heads/v2\.2\.2$`), httpmock.StatusResponse(http.StatusNotFound))
	reg.Register(httpmock.REST("GET", `^/repos/krzema12/github-actions-typing/commits/refs/tags/v2\.2\.2$`), commit)
	reg.Register(httpmock.REST("GET", `^/repos/krzema12/github-actions-typing/commits/refs/heads/v2\.2\.2$`), httpmock.StatusResponse(http.StatusNotFound))
	reg.Register(httpmock.REST("GET", `^/repos/typesafegithub/github-actions-typing/contents/action\.yml$`), httpmock.StatusResponse(http.StatusNotFound))
	reg.Register(httpmock.REST("GET", `^/repos/typesafegithub/github-actions-typing/contents/action\.yaml$`), httpmock.StatusResponse(http.StatusNotFound))
	useDefaultTransport(t, reg)
	path := writeTempWorkflow(t, transferWorkflow)

	_, stderr, err := runCommandWithHTTP(t, reg, "--no-narrow", "--no-interactive", path)
	require.NoError(t, err, stderr)
	wf, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, strings.TrimSpace(transferWorkflow)+"\n", string(wf))
	lock := readTempLockfilePins(t)
	assert.Contains(t, lock, "'krzema12/github-actions-typing@v2.2.2':")
	assert.NotContains(t, lock, "typesafegithub")
}

// mutableModes are the run modes that check a mutable key's history.
// --relock re-resolves mutable keys, so it repairs rather than blocks.
var mutableModes = map[string][]string{
	"default": {"--no-onboard", "--no-narrow", "--no-interactive"},
	"verify":  {"--verify"},
	"no-fix":  {"--no-fix"},
}

// TestParity_MutableRefHistory: every mode checks that a commit locked
// under a mutable ref is still in that ref's history, since a fork-network
// commit exists under the upstream name (#174 case C). A legitimately moved
// ref stays sticky and silent, and an inconclusive check fails closed.
func TestParity_MutableRefHistory(t *testing.T) {
	const live = "cccccccccccccccccccccccccccccccccccccccc"
	compareURL := `repos/actions/checkout/compare/` + parityTestSHA + `\.\.\.` + live
	tests := []struct {
		name    string
		compare httpmock.Responder
		// want is the finding category, "" for a clean pass.
		want string
	}{
		{"impostor blocks", httpmock.JSONResponse(map[string]any{"status": "diverged", "merge_base_commit": map[string]any{"sha": parityTestMoved}}), "unreachable-pin"},
		{"moved forward passes", httpmock.JSONResponse(map[string]any{"status": "ahead", "merge_base_commit": map[string]any{"sha": parityTestSHA}}), ""},
		{"compare 503 blocks", httpmock.StatusResponse(http.StatusServiceUnavailable), "reachability-unknown"},
	}
	for _, tt := range tests {
		for mode, args := range mutableModes {
			for _, proxy := range []bool{false, true} {
				t.Run(tt.name+"/"+mode+"/"+map[bool]string{false: "graphql", true: "dependabot proxy"}[proxy], func(t *testing.T) {
					reg := &httpmock.Registry{}
					if proxy {
						t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
						useDefaultTransport(t, reg)
						reg.Register(httpmock.REST("GET", `^/repos/actions/checkout$`), httpmock.JSONResponse(map[string]any{
							"full_name": "actions/checkout", "id": 1, "owner": map[string]any{"id": 1},
						}))
						reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/commits/`+parityTestSHA+`$`), httpmock.JSONResponse(map[string]any{"sha": parityTestSHA}))
						reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/commits/refs/tags/v4$`), httpmock.JSONResponse(map[string]any{"sha": live}))
						reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/commits/refs/heads/v4$`), httpmock.StatusResponse(http.StatusNotFound))
					} else {
						reg.Register(httpmock.GraphQL(`head: object\(expression`), httpmock.JSONResponse(map[string]any{
							"data": map[string]any{"a0": map[string]any{
								"nameWithOwner": "actions/checkout",
								"commit":        map[string]any{"oid": parityTestSHA},
								"tag":           map[string]any{"oid": live},
							}},
						}))
					}
					for range 10 {
						reg.Register(httpmock.REST("GET", compareURL), tt.compare)
					}
					if tt.want != "unreachable-pin" {
						if proxy {
							reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/commits/`+parityTestSHA+`$`),
								httpmock.JSONResponse(map[string]any{"sha": parityTestSHA}))
							reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/contents/action\.yml$`),
								httpmock.JSONResponse(map[string]any{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(nodeActionYAML))}))
						} else {
							registerNodeManifests(reg)
						}
					}
					path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
`, "actions/checkout@v4=sha1-"+parityTestSHA)
					lockBefore := readTempLockfilePins(t)

					stdout, stderr, err := runCommandWithHTTP(t, reg, append(args, "--json=valid,findings", path)...)
					if proxy && tt.want == "reachability-unknown" {
						require.Error(t, err, stderr)
						assert.NotErrorIs(t, err, errSilent, "inconclusive under the proxy is a retry")
						assert.Contains(t, err.Error(), "HTTP 503")
						return
					}
					var p parityPayload
					require.NoError(t, json.Unmarshal([]byte(stdout), &p), stdout+stderr)
					if tt.want == "" {
						require.NoError(t, err, stderr)
						assert.Empty(t, p.Findings, "a moved mutable ref stays sticky and silent")
						return
					}
					require.ErrorIs(t, err, errSilent, stderr)
					require.Len(t, p.Findings, 1)
					assert.Equal(t, tt.want, p.Findings[0].Category)
					assert.Equal(t, "error", p.Findings[0].Severity)
					assert.Equal(t, lockBefore, readTempLockfilePins(t), "a blocked pin must not be rewritten")
					if tt.want == "unreachable-pin" {
						assert.Contains(t, p.Findings[0].Detail, "is not in actions/checkout's history")
						assert.Contains(t, p.Findings[0].Detail, "v4 currently points at cccccccccccc")
						assert.NotEmpty(t, p.Findings[0].DocURL, "the impostor keeps its doc link")
					}
				})
			}
		}
	}
}

// TestParity_MissingMutableRefBlocks: a mutable ref that is no longer a
// tag or branch is a definitive answer the runner rejects, so every mode
// blocks rather than passing on the commit alone.
func TestParity_MissingMutableRefBlocks(t *testing.T) {
	for mode, args := range mutableModes {
		for _, proxy := range []bool{false, true} {
			t.Run(mode+"/"+map[bool]string{false: "graphql", true: "dependabot proxy"}[proxy], func(t *testing.T) {
				reg := &httpmock.Registry{}
				if proxy {
					t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
					useDefaultTransport(t, reg)
				}
				// One pass for --json, one for the terminal.
				for range 2 {
					if proxy {
						reg.Register(httpmock.REST("GET", `^/repos/actions/checkout$`), httpmock.JSONResponse(map[string]any{
							"full_name": "actions/checkout", "id": 1, "owner": map[string]any{"id": 1},
						}))
						reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/commits/`+parityTestSHA+`$`), httpmock.JSONResponse(map[string]any{"sha": parityTestSHA}))
						reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/commits/refs/tags/v4$`), httpmock.StatusResponse(http.StatusNotFound))
						reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/commits/refs/heads/v4$`), httpmock.StatusResponse(http.StatusNotFound))
					} else {
						reg.Register(httpmock.GraphQL(`head: object\(expression`), httpmock.JSONResponse(map[string]any{
							"data": map[string]any{"a0": map[string]any{
								"nameWithOwner": "actions/checkout",
								"commit":        map[string]any{"oid": parityTestSHA},
								"head":          nil,
							}},
						}))
					}
				}
				path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
`, "actions/checkout@v4=sha1-"+parityTestSHA)

				stdout, stderr, err := runCommandWithHTTP(t, reg, append(args, "--json=valid,findings", path)...)
				require.ErrorIs(t, err, errSilent, stderr)
				var p parityPayload
				require.NoError(t, json.Unmarshal([]byte(stdout), &p), stdout+stderr)
				require.Len(t, p.Findings, 1)
				assert.Equal(t, "unreachable-pin", p.Findings[0].Category)
				assert.Equal(t, "error", p.Findings[0].Severity)
				assert.Equal(t, "ref v4 is not a tag or branch of actions/checkout; the runner will reject this pin", p.Findings[0].Detail)
				assert.Equal(t, "point `uses:` at a tag or branch that exists, then run `gh actions-lock`", p.Findings[0].Remediation)
				assert.Empty(t, p.Findings[0].DocURL, "the hardening guide is unrelated to a missing ref")

				stdout, stderr, err = runCommandWithHTTP(t, reg, append(args, path)...)
				require.ErrorIs(t, err, errSilent)
				term := stdout + stderr
				assert.Contains(t, term, "Ref no longer exists actions/checkout@v4")
				assert.Contains(t, term, "ref v4 is not a tag or branch of actions/checkout; the runner will reject this pin")
				assert.Contains(t, term, "point `uses:` at a tag or branch that exists, then run `gh actions-lock`")
				assert.NotContains(t, term, "not in upstream history")
				assert.NotContains(t, term, "/releases")
				assert.NotContains(t, term, "see:")
			})
		}
	}
}

// TestParity_VerifyRefCaseSensitive: refs differing only in case are
// distinct refs to the runner, so each gets its own history check.
func TestParity_VerifyRefCaseSensitive(t *testing.T) {
	for _, proxy := range []bool{false, true} {
		t.Run(map[bool]string{false: "graphql", true: "dependabot proxy"}[proxy], func(t *testing.T) {
			reg := &httpmock.Registry{}
			var heads []string
			if proxy {
				t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
				useDefaultTransport(t, reg)
				for range 2 {
					reg.Register(httpmock.REST("GET", `^/repos/actions/checkout$`), httpmock.JSONResponse(map[string]any{
						"full_name": "actions/checkout", "id": 1, "owner": map[string]any{"id": 1},
					}))
					reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/commits/`+parityTestSHA+`$`), httpmock.JSONResponse(map[string]any{"sha": parityTestSHA}))
				}
				for _, ref := range []string{"Main", "main"} {
					reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/commits/refs/tags/`+ref+`$`), func(req *http.Request) (*http.Response, error) {
						heads = append(heads, ref)
						return httpmock.JSONResponse(map[string]any{"sha": parityTestSHA})(req)
					})
					reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/commits/refs/heads/`+ref+`$`), httpmock.StatusResponse(http.StatusNotFound))
				}
			} else {
				reg.Register(httpmock.GraphQL(`head: object\(expression`), func(req *http.Request) (*http.Response, error) {
					body, _ := io.ReadAll(req.Body)
					for _, ref := range []string{"Main", "main"} {
						if strings.Contains(string(body), `"refs/tags/`+ref+`^{commit}"`) {
							heads = append(heads, ref)
						}
					}
					repo := map[string]any{"nameWithOwner": "actions/checkout", "commit": map[string]any{"oid": parityTestSHA}, "head": map[string]any{"oid": parityTestSHA}}
					return httpmock.JSONResponse(map[string]any{"data": map[string]any{"a0": repo, "a1": repo}})(req)
				})
			}
			path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@Main
      - uses: actions/checkout@main
`, "actions/checkout@Main=sha1-"+parityTestSHA, "actions/checkout@main=sha1-"+parityTestSHA)

			if proxy {
				for range 2 {
					reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/commits/`+parityTestSHA+`$`),
						httpmock.JSONResponse(map[string]any{"sha": parityTestSHA}))
					reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/contents/action\.yml$`),
						httpmock.JSONResponse(map[string]any{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(nodeActionYAML))}))
				}
			} else {
				registerNodeManifests(reg)
			}
			_, stderr, err := runCommandWithHTTP(t, reg, "--verify", path)
			require.NoError(t, err, stderr)
			assert.ElementsMatch(t, []string{"Main", "main"}, heads)
		})
	}
}

// TestParity_CancelDuringAncestryAborts: a cancellation while a run
// checks history must not turn into a normal result or, behind the proxy,
// into an inconclusive-lookup error.
func TestParity_CancelDuringAncestryAborts(t *testing.T) {
	const live = "cccccccccccccccccccccccccccccccccccccccc"
	for _, proxy := range []bool{false, true} {
		t.Run(map[bool]string{false: "graphql", true: "dependabot proxy"}[proxy], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reg := &httpmock.Registry{}
			if proxy {
				t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
				useDefaultTransport(t, reg)
				reg.Register(httpmock.REST("GET", `^/repos/actions/checkout$`), httpmock.JSONResponse(map[string]any{
					"full_name": "actions/checkout", "id": 1, "owner": map[string]any{"id": 1},
				}))
				reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/commits/`+parityTestSHA+`$`), httpmock.JSONResponse(map[string]any{"sha": parityTestSHA}))
				reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/commits/refs/tags/v4$`), httpmock.JSONResponse(map[string]any{"sha": live}))
				reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/commits/refs/heads/v4$`), httpmock.StatusResponse(http.StatusNotFound))
			} else {
				reg.Register(httpmock.GraphQL(`head: object\(expression`), httpmock.JSONResponse(map[string]any{
					"data": map[string]any{"a0": map[string]any{
						"nameWithOwner": "actions/checkout",
						"commit":        map[string]any{"oid": parityTestSHA},
						"head":          map[string]any{"oid": live},
					}},
				}))
			}
			for range 10 {
				reg.Register(httpmock.REST("GET", `repos/actions/checkout/compare/`), func(req *http.Request) (*http.Response, error) {
					cancel()
					return nil, ctx.Err()
				})
			}
			path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
`, "actions/checkout@v4=sha1-"+parityTestSHA)
			lockBefore := readTempLockfilePins(t)

			_, stderr, err := runCommandWithHTTPContext(t, ctx, reg, "--verify", path)
			require.ErrorIs(t, err, context.Canceled, stderr)
			assert.NotContains(t, err.Error(), "could not", "the cancellation is reported, not the lookup it interrupted")
			assert.Equal(t, lockBefore, readTempLockfilePins(t))
		})
	}
}

// fullSHAModes are the run modes that must all check a full-SHA key's
// provenance (#174): a fix run that trusts the lock would launder a
// fork-only commit by rewriting `uses:` to the recorded tag.
var fullSHAModes = map[string][]string{
	"default": nil,
	"verify":  {"--verify"},
	"no-fix":  {"--no-fix"},
	"relock":  {"--relock"},
}

// TestParity_FullSHAKeyProvenance is #174: a fork-network commit resolves
// under the upstream name, so every mode checks a locked commit against the
// lock entry's ref, looked up only as a tag, then a branch.
func TestParity_FullSHAKeyProvenance(t *testing.T) {
	const live = "cccccccccccccccccccccccccccccccccccccccc"
	diverged := httpmock.JSONResponse(map[string]any{"status": "diverged", "merge_base_commit": map[string]any{"sha": parityTestMoved}})
	ahead := httpmock.JSONResponse(map[string]any{"status": "ahead", "merge_base_commit": map[string]any{"sha": parityTestSHA}})
	tests := []struct {
		name    string
		ref     string
		tag     any
		head    any
		compare httpmock.Responder
		// want is the blocking finding category, "" for a pass.
		want   string
		detail string
	}{
		{
			name: "fork commit under recorded tag blocks", ref: "v6.1.0",
			head: map[string]any{"oid": live}, compare: diverged,
			want: "unreachable-pin", detail: "v6.1.0 currently points at cccccccccccc",
		},
		{
			name: "upstream commit outside recorded tag blocks", ref: "v4.2.2",
			head: map[string]any{"oid": live}, compare: diverged,
			want: "unreachable-pin", detail: "v4.2.2 currently points at cccccccccccc",
		},
		{
			name: "upstream commit outside recorded branch blocks", ref: "releases/v4",
			head: map[string]any{"oid": live}, compare: diverged,
			want: "unreachable-pin", detail: "releases/v4 currently points at cccccccccccc",
		},
		{
			name: "recorded ref gone blocks", ref: "does-not-exist",
			head: nil,
			want: "unreachable-pin", detail: "ref does-not-exist is not a tag or branch of actions/checkout",
		},
		{
			// Unqualified, pull/1/head would resolve to refs/pull/1/head.
			name: "recorded pull request ref blocks", ref: "pull/1/head",
			head: nil,
			want: "unreachable-pin", detail: "ref pull/1/head is not a tag or branch of actions/checkout",
		},
		{
			name: "recorded tag at commit passes", ref: "v6.1.0",
			tag: map[string]any{"oid": parityTestSHA}, head: nil,
		},
		{
			name: "inconclusive history check blocks", ref: "main",
			head: map[string]any{"oid": live}, compare: httpmock.StatusResponse(http.StatusForbidden),
			want: "reachability-unknown", detail: "checking main history",
		},
		{
			name: "commit in recorded ref passes", ref: "main",
			head: map[string]any{"oid": live}, compare: ahead,
		},
		{
			name: "recorded ref at commit passes", ref: "v6.1.0",
			head: map[string]any{"oid": parityTestSHA},
		},
	}
	for _, tt := range tests {
		for mode, args := range fullSHAModes {
			t.Run(tt.name+"/"+mode, func(t *testing.T) {
				reg := &httpmock.Registry{}
				reg.Register(httpmock.GraphQL(`commit: object\(oid`), func(req *http.Request) (*http.Response, error) {
					var body struct {
						Variables map[string]any `json:"variables"`
					}
					require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
					assert.Equal(t, "refs/tags/"+tt.ref+"^{commit}", body.Variables["tag0"], "the recorded ref is looked up only as a tag or branch")
					assert.Equal(t, "refs/heads/"+tt.ref+"^{commit}", body.Variables["head0"], "the recorded ref is looked up only as a tag or branch")
					return httpmock.JSONResponse(map[string]any{
						"data": map[string]any{"a0": map[string]any{
							"nameWithOwner": "actions/checkout",
							"databaseId":    1,
							"commit":        map[string]any{"oid": parityTestSHA},
							"tag":           tt.tag,
							"head":          tt.head,
						}},
					})(req)
				})
				if tt.compare != nil {
					reg.Register(httpmock.REST("GET", `repos/actions/checkout/compare/`+parityTestSHA+`\.\.\.`+live), tt.compare)
				}
				if tt.want == "" {
					reg.Register(httpmock.REST("GET", `repos/actions/checkout/tags`), httpmock.JSONResponse(httpmock.TagListResponse("v6.1.0", parityTestSHA)))
					registerNodeManifests(reg)
					if mode == "relock" {
						reg.Register(httpmock.REST("GET", `repos/actions/checkout$`),
							httpmock.JSONResponse(map[string]any{"default_branch": "main"}))
						reg.Register(httpmock.REST("GET", `repos/actions/checkout/git/ref/heads/main`),
							httpmock.JSONResponse(map[string]any{
								"ref": "refs/heads/main", "object": map[string]any{"sha": parityTestSHA, "type": "commit"},
							}))
					}
				}
				path := writeFullSHAWorkflow(t, tt.ref)

				assertFullSHARun(t, reg, args, path, tt.want, tt.detail)
			})
		}
	}
}

// TestParity_InvalidEntryRefIsParseError: an entry ref the runner would
// reject (a SHA, a refs/ name, or a ref other than the key's) makes the
// lockfile unreadable in every mode, with exit 2 and nothing rewritten. A
// fix run must never launder it into a `uses:` rewrite.
func TestParity_InvalidEntryRefIsParseError(t *testing.T) {
	tests := []struct {
		name string
		key  string
		ref  string
	}{
		{"full-SHA key records the SHA", parityTestSHA, parityTestSHA},
		{"full-SHA key records a qualified tag", parityTestSHA, "refs/tags/v6.1.0"},
		{"full-SHA key records a qualified branch", parityTestSHA, "refs/heads/main"},
		{"full-SHA key records a pull request ref", parityTestSHA, "refs/pull/1/head"},
		{"branch key records another ref", "main", "v6.1.0"},
	}
	for _, tt := range tests {
		for mode, args := range fullSHAModes {
			t.Run(tt.name+"/"+mode, func(t *testing.T) {
				reg := &httpmock.Registry{}
				defer reg.Verify(t)
				path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@`+tt.key+`
`, "actions/checkout@"+tt.key+"=sha1-"+parityTestSHA)
				lockPath := filepath.Join(".github", "workflows", "actions.lock")
				lock := strings.Replace(readTempLockfilePins(t), "ref: '"+tt.key+"'", "ref: '"+tt.ref+"'", 1)
				require.NoError(t, os.WriteFile(lockPath, []byte(lock), 0o600))
				wfBefore, err := os.ReadFile(path)
				require.NoError(t, err)

				_, stderr, err := runCommandWithHTTP(t, reg, append(args, "--no-interactive", path)...)
				require.Error(t, err, stderr)
				assert.NotErrorIs(t, err, errSilent, "a bad entry ref is a parse error (exit 2), not a finding")
				assert.Contains(t, err.Error(), "actions/checkout@"+tt.key)
				assert.Empty(t, reg.Requests, "nothing is resolved from an unreadable lockfile")
				wfAfter, err := os.ReadFile(path)
				require.NoError(t, err)
				assert.Equal(t, string(wfBefore), string(wfAfter))
				assert.Equal(t, lock, readTempLockfilePins(t))
			})
		}
	}
}

// TestParity_NonBranchRefRefused: a key whose ref is neither a tag nor a
// branch resolves by rev-parse rules but the runner rejects it, so no mode
// may write a lock for it, including --relock, which otherwise drops
// findings that re-resolving would repair. An existing lock entry recording
// a refs/ name is already unreadable.
func TestParity_NonBranchRefRefused(t *testing.T) {
	for _, ref := range []string{"pull/2/head", "refs/pull/2/head"} {
		for _, locked := range []bool{false, true} {
			for mode, args := range map[string][]string{"default": nil, "relock": {"--relock"}} {
				name := ref + "/" + mode + "/" + map[bool]string{false: "first run", true: "locked"}[locked]
				t.Run(name, func(t *testing.T) {
					reg := &httpmock.Registry{}
					// The parity stub goes first: the resolve stub matches any query for the repo.
					reg.Register(httpmock.GraphQL(`commit: object\(oid`), func(req *http.Request) (*http.Response, error) {
						var body struct {
							Variables map[string]any `json:"variables"`
						}
						require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
						assert.Equal(t, "refs/tags/"+ref+"^{commit}", body.Variables["tag0"])
						assert.Equal(t, "refs/heads/"+ref+"^{commit}", body.Variables["head0"])
						return httpmock.JSONResponse(map[string]any{
							"data": map[string]any{"a0": map[string]any{
								"nameWithOwner": "actions/checkout",
								"databaseId":    1,
								"commit":        map[string]any{"oid": parityTestSHA},
							}},
						})(req)
					})
					reg.Register(httpmock.GraphQLForRepo("actions", "checkout"), httpmock.JSONResponse(map[string]any{
						"data": map[string]any{"a0": testRepoResponse("actions/checkout", parityTestSHA, nodeActionYAML)},
					}))
					reg.Register(httpmock.REST("GET", `^/repos/actions/checkout$`), httpmock.JSONResponse(map[string]any{
						"full_name": "actions/checkout", "id": 1, "owner": map[string]any{"id": 1},
					}))
					var pins []string
					if locked {
						pins = []string{"actions/checkout@" + ref + "=sha1-" + parityTestSHA}
					}
					path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@`+ref+`
`, pins...)
					lockPath := filepath.Join(".github", "workflows", "actions.lock")
					lockBefore, _ := os.ReadFile(lockPath)
					wfBefore, err := os.ReadFile(path)
					require.NoError(t, err)

					cmdArgs := append([]string{"--no-narrow", "--no-interactive"}, args...)
					if locked && strings.HasPrefix(ref, "refs/") {
						_, _, err := runCommandWithHTTP(t, reg, append(cmdArgs, path)...)
						require.Error(t, err)
						assert.NotErrorIs(t, err, errSilent, "an unreadable lockfile exits 2")
						assert.Contains(t, err.Error(), "actions/checkout@"+ref)
					} else {
						p, err := runParity(t, reg, append(cmdArgs, path)...)
						require.ErrorIs(t, err, errSilent)
						var unreachable []string
						for _, f := range p.Findings {
							if f.Category == "unreachable-pin" {
								unreachable = append(unreachable, f.Detail)
							}
						}
						require.Len(t, unreachable, 1, "%+v", p.Findings)
						assert.Contains(t, unreachable[0], "ref "+ref+" is not a tag or branch of actions/checkout")
					}
					if locked {
						lockAfter, err := os.ReadFile(lockPath)
						require.NoError(t, err)
						assert.Equal(t, string(lockBefore), string(lockAfter))
					} else {
						assert.NoFileExists(t, lockPath)
					}
					wfAfter, err := os.ReadFile(path)
					require.NoError(t, err)
					assert.Equal(t, string(wfBefore), string(wfAfter))
				})
			}
		}
	}
}

// writeFullSHAWorkflow writes a workflow using actions/checkout at
// parityTestSHA, locked to that commit with the given recorded ref.
func writeFullSHAWorkflow(t *testing.T, ref string) string {
	t.Helper()
	path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@`+parityTestSHA+`
`, "actions/checkout@"+parityTestSHA+"=sha1-"+parityTestSHA)
	lockPath := filepath.Join(".github", "workflows", "actions.lock")
	lock := readTempLockfilePins(t)
	lock = strings.Replace(lock, "ref: '"+parityTestSHA+"'", "ref: '"+ref+"'", 1)
	require.NoError(t, os.WriteFile(lockPath, []byte(lock), 0o600))
	return path
}

// assertFullSHARun runs one mode and checks the verdict. A blocked run must
// leave both files untouched: no rewrite of `uses:` to the recorded ref.
func assertFullSHARun(t *testing.T, reg *httpmock.Registry, args []string, path, want, detail string) {
	t.Helper()
	lockBefore := readTempLockfilePins(t)
	wfBefore, err := os.ReadFile(path)
	require.NoError(t, err)
	stdout, stderr, err := runCommandWithHTTP(t, reg, append(args, "--no-interactive", "--json=valid,findings", path)...)
	var p parityPayload
	require.NoError(t, json.Unmarshal([]byte(stdout), &p), stdout+stderr)
	var blocking []format.Finding
	for _, f := range p.Findings {
		if f.Severity == "error" {
			blocking = append(blocking, f)
		}
	}
	if want == "" {
		require.NoError(t, err, stdout+stderr)
		assert.True(t, p.Valid)
		assert.Empty(t, blocking)
		return
	}
	require.ErrorIs(t, err, errSilent, stderr)
	assert.False(t, p.Valid)
	require.Len(t, blocking, 1, "findings: %+v", p.Findings)
	assert.Equal(t, want, blocking[0].Category)
	assert.Contains(t, blocking[0].Detail, detail)
	wfAfter, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(wfBefore), string(wfAfter), "a blocked pin must not be rewritten")
	assert.Contains(t, string(wfAfter), "actions/checkout@"+parityTestSHA)
	assert.Equal(t, lockBefore, readTempLockfilePins(t), "a blocked pin must not reach the lockfile")
}
