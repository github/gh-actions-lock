package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/cmd/gh-actions-lock/format"
	"github.com/github/gh-actions-lock/internal/ghapi"
	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
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
			"tagRef":        map[string]any{"oid": parityTestSHA},
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
	assert.Contains(t, stderr, "All 1 workflow valid")
}

// TestParity_TransferredRepoNamesTheKeyUse: a tag-keyed entry the workflow
// also uses by its commit SHA, earlier in the file, is still checked as
// the tag use, so the rename advice rewrites that use.
func TestParity_TransferredRepoNamesTheKeyUse(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(httpmock.GraphQL(`commit: object\(oid`), transferParity(1))
	path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: krzema12/github-actions-typing@`+parityTestSHA+`
      - uses: krzema12/github-actions-typing@v2.2.2
`, "krzema12/github-actions-typing@v2.2.2=sha1-"+parityTestSHA)

	_, stderr, err := runCommandWithHTTP(t, parityFallback{reg}, "--verify", path)
	require.NoError(t, err, stderr)
	assert.Contains(t, stderr, "rewrite it as `uses: typesafegithub/github-actions-typing@v2.2.2`")
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
	assert.NotContains(t, stderr, "rewrite it as", "the run already rewrote it")
}

// TestParity_MixedCaseRenameRewrites: the lockfile key is lowercase but
// `uses:` isn't; the rewrite must match the workflow's own text.
func TestParity_MixedCaseRenameRewrites(t *testing.T) {
	reg := &httpmock.Registry{}
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
					"tagRef":        map[string]any{"oid": parityTestSHA},
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

// TestParity_MovedExactTagFollowsHistory: an exact release tag names one
// commit, so an entry keyed by v7.0.1 that locks v7.0.0's commit, an older
// commit in the tag's history, blocks every mode (#202). A floating ref
// behind its head is sticky and silent, and so is a branch named like a
// release. A tag that lost the commit blocks as unreachable.
func TestParity_MovedExactTagFollowsHistory(t *testing.T) {
	const lost = "cccccccccccccccccccccccccccccccccccccccc"
	fix := []string{"--no-onboard", "--no-narrow", "--no-interactive"}
	tests := []struct {
		name   string
		ref    string
		status string
		base   string
		args   []string
		want   string
	}{
		{"exact tag behind blocks verify", "v7.0.1", "ahead", parityTestSHA, []string{"--verify"}, "tag-commit-mismatch"},
		{"exact tag behind blocks no-fix", "v7.0.1", "ahead", parityTestSHA, []string{"--no-fix"}, "tag-commit-mismatch"},
		{"exact tag behind blocks a default run", "v7.0.1", "ahead", parityTestSHA, fix, "tag-commit-mismatch"},
		{"floating ref behind passes verify", "v7", "ahead", parityTestSHA, []string{"--verify"}, ""},
		{"floating ref behind passes a default run", "v7", "ahead", parityTestSHA, fix, ""},
		{"lost commit blocks verify", "v7.0.1", "diverged", lost, []string{"--verify"}, "unreachable-pin"},
		{"lost commit blocks a default run", "v7.0.1", "diverged", lost, fix, "unreachable-pin"},
		{"branch named like a release passes verify", "v7.0.1", "ahead", parityTestSHA, []string{"--verify"}, ""},
		{"branch named like a release passes a default run", "v7.0.1", "ahead", parityTestSHA, fix, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := &httpmock.Registry{}
			defer reg.Verify(t)
			head := map[string]any{
				"nameWithOwner": "actions/checkout",
				"commit":        map[string]any{"oid": parityTestSHA},
				"tag":           map[string]any{"oid": parityTestMoved},
				"tagRef":        map[string]any{"oid": parityTestMoved},
			}
			if strings.HasPrefix(tt.name, "branch") {
				delete(head, "tag")
				delete(head, "tagRef")
				head["head"] = map[string]any{"oid": parityTestMoved}
				head["headRef"] = map[string]any{}
			}
			reg.Register(httpmock.GraphQL(`commit: object\(oid`), httpmock.JSONResponse(map[string]any{
				"data": map[string]any{"a0": head},
			}))
			reg.Register(httpmock.REST("GET", `repos/actions/checkout/compare/`+parityTestSHA+`\.\.\.`+parityTestMoved),
				httpmock.JSONResponse(map[string]any{"status": tt.status, "merge_base_commit": map[string]any{"sha": tt.base}}))
			path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@`+tt.ref+`
`, "actions/checkout@"+tt.ref+"=sha1-"+parityTestSHA)
			beforeWorkflow, err := os.ReadFile(path)
			require.NoError(t, err)
			lockPath := filepath.Join(".github", "workflows", "actions.lock")
			beforeLock, err := os.ReadFile(lockPath)
			require.NoError(t, err)
			p, err := runParity(t, reg, append(tt.args, path)...)
			afterWorkflow, readErr := os.ReadFile(path)
			require.NoError(t, readErr)
			assert.Equal(t, beforeWorkflow, afterWorkflow, "the workflow must stay unchanged")
			lock := readTempLockfilePins(t)
			assert.Contains(t, lock, "sha1-"+parityTestSHA)
			assert.NotContains(t, lock, parityTestMoved, "the lock keeps its commit")
			if tt.want == "" {
				require.NoError(t, err)
				assert.True(t, p.Valid)
				assert.Empty(t, p.Findings)
				return
			}
			afterLock, readErr := os.ReadFile(lockPath)
			require.NoError(t, readErr)
			assert.Equal(t, beforeLock, afterLock, "a blocked run must not write the lockfile")
			require.ErrorIs(t, err, errSilent)
			assert.False(t, p.Valid)
			require.Len(t, p.Findings, 1, "%+v", p.Findings)
			assert.Equal(t, tt.want, p.Findings[0].Category)
			assert.Equal(t, "error", p.Findings[0].Severity)
			if tt.want == "tag-commit-mismatch" {
				assert.Equal(t, "high", p.Findings[0].Confidence)
				assert.Equal(t, "tag v7.0.1 points at bbbbbbbbbbbb; the lockfile locks 9ddf35b71a48, an older commit in its history", p.Findings[0].Detail)
			}
		})
	}
}

// TestParity_MovedFloatingPathRefIsSticky: a fix run that bumps one action
// reads a path action's manifest live, so it sees the action's floating ref
// moved ahead. The locked commit is still in the ref's history: the run keeps
// it with a ref-moved warning and writes the bump, and only --relock or
// --accept-moved re-pins it. A ref whose history lost the commit blocks in
// every mode, and an exact tag locked behind its commit still blocks.
func TestParity_MovedFloatingPathRefIsSticky(t *testing.T) {
	const (
		oldCheckout = "8ade135a41bc03ea155e62e844d188df1ea18608"
		newCheckout = "11bd71901bbe5b1630ceea73d27597364c9af683"
		lost        = "cccccccccccccccccccccccccccccccccccccccc"
	)
	fix := []string{"--no-onboard", "--no-narrow", "--no-interactive"}
	tests := []struct {
		name     string
		ref      string
		status   string
		base     string
		args     []string
		want     string // blocking category; "" for a run that writes
		severity string
		repinned bool
	}{
		{name: "fix run keeps the locked commit", ref: "v3", status: "ahead", base: parityTestSHA, args: fix, want: "ref-moved", severity: "warning"},
		{name: "relock re-pins", ref: "v3", status: "ahead", base: parityTestSHA, args: append([]string{"--relock"}, fix...), repinned: true},
		{name: "accept-moved re-pins", ref: "v3", status: "ahead", base: parityTestSHA, args: append([]string{"--accept-moved"}, fix...), repinned: true},
		{name: "force-push blocks a fix run", ref: "v3", status: "diverged", base: lost, args: fix, want: "unreachable-pin", severity: "error"},
		{name: "force-push blocks no-fix", ref: "v3", status: "diverged", base: lost, args: []string{"--no-fix"}, want: "unreachable-pin", severity: "error"},
		{name: "force-push blocks verify", ref: "v3", status: "diverged", base: lost, args: []string{"--verify"}, want: "unreachable-pin", severity: "error"},
		{name: "exact tag behind blocks a fix run", ref: "v3.28.0", status: "ahead", base: parityTestSHA, args: fix, want: "tag-commit-mismatch", severity: "error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := &httpmock.Registry{}
			stubGraph(t, reg,
				map[string]string{"github/codeql-action": parityTestMoved},
				map[string]string{"github/codeql-action": parityTestMoved, "actions/checkout": newCheckout},
				nil)
			for _, nwo := range []string{"actions/checkout", "github/codeql-action"} {
				reg.RegisterReusable(httpmock.REST("GET", `repos/`+nwo+`/(branches|tags)`), httpmock.JSONResponse([]any{}))
			}
			reg.RegisterReusable(httpmock.REST("GET", `repos/github/codeql-action/compare/`+parityTestSHA+`\.\.\.`+parityTestMoved),
				httpmock.JSONResponse(map[string]any{"status": tt.status, "merge_base_commit": map[string]any{"sha": tt.base}}))
			path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4.2.2
      - uses: github/codeql-action/init@`+tt.ref+`
`, "actions/checkout@v4.1.0=sha1-"+oldCheckout, "github/codeql-action@"+tt.ref+"=sha1-"+parityTestSHA)
			lockPath := filepath.Join(".github", "workflows", "actions.lock")
			before, err := os.ReadFile(lockPath)
			require.NoError(t, err)

			p, err := runParity(t, reg, append(tt.args, path)...)
			after, readErr := os.ReadFile(lockPath)
			require.NoError(t, readErr)
			var found *format.Finding
			for i := range p.Findings {
				if p.Findings[i].Category == tt.want {
					found = &p.Findings[i]
				}
			}
			if tt.severity == "error" {
				require.ErrorIs(t, err, errSilent)
				assert.False(t, p.Valid)
				require.NotNil(t, found, "%+v", p.Findings)
				assert.Equal(t, "error", found.Severity)
				assert.Equal(t, string(before), string(after), "a blocked run must not write the lockfile")
				return
			}
			require.NoError(t, err, "%+v\n%s", p.Findings, after)
			if tt.want != "" {
				require.NotNil(t, found, "%+v", p.Findings)
				assert.Equal(t, tt.severity, found.Severity)
			}
			assert.Contains(t, string(after), "sha1-"+newCheckout, "the bump is locked")
			if tt.repinned {
				assert.Contains(t, string(after), "sha1-"+parityTestMoved)
				assert.NotContains(t, string(after), parityTestSHA)
				return
			}
			assert.Contains(t, string(after), "sha1-"+parityTestSHA, "the moved ref keeps its locked commit")
			assert.NotContains(t, string(after), parityTestMoved)
		})
	}
}

// TestParity_RenamedExactTagBehindBlocks: a same-ID rename only warns, and
// must not hide an exact tag locked behind its commit.
func TestParity_RenamedExactTagBehindBlocks(t *testing.T) {
	for name, args := range map[string][]string{
		"verify":  {"--verify"},
		"no-fix":  {"--no-fix"},
		"default": {"--no-onboard", "--no-narrow", "--no-interactive"},
	} {
		t.Run(name, func(t *testing.T) {
			reg := &httpmock.Registry{}
			reg.Register(httpmock.GraphQL(`commit: object\(oid`), httpmock.JSONResponse(map[string]any{
				"data": map[string]any{"a0": map[string]any{
					"nameWithOwner": "typesafegithub/github-actions-typing",
					"databaseId":    1,
					"commit":        map[string]any{"oid": parityTestSHA},
					"tag":           map[string]any{"oid": parityTestMoved},
					"tagRef":        map[string]any{},
				}},
			}))
			reg.Register(httpmock.REST("GET", `compare/`+parityTestSHA+`\.\.\.`+parityTestMoved),
				httpmock.JSONResponse(map[string]any{"status": "ahead", "merge_base_commit": map[string]any{"sha": parityTestSHA}}))
			path := writeTempWorkflow(t, transferWorkflow, "krzema12/github-actions-typing@v2.2.2=sha1-"+parityTestSHA)
			lockPath := filepath.Join(".github", "workflows", "actions.lock")
			before, err := os.ReadFile(lockPath)
			require.NoError(t, err)

			p, err := runParity(t, parityFallback{reg}, append(args, path)...)
			require.ErrorIs(t, err, errSilent)
			assert.False(t, p.Valid)
			sev := map[string]string{}
			for _, f := range p.Findings {
				sev[f.Category] = f.Severity
			}
			assert.Equal(t, "warning", sev["repo-renamed"], "%+v", p.Findings)
			assert.Equal(t, "error", sev["tag-commit-mismatch"], "%+v", p.Findings)
			after, err := os.ReadFile(lockPath)
			require.NoError(t, err)
			assert.Equal(t, string(before), string(after), "nothing is written")
		})
	}
}

// TestParity_TerminalSeverity: the terminal must agree with the exit code.
// A fail-closed history check prints as an error, never a warning, and so
// does an exact tag locked behind its commit.
func TestParity_TerminalSeverity(t *testing.T) {
	workflow := `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4.2.1
`
	t.Run("inconclusive is an error", func(t *testing.T) {
		path := writeTempWorkflow(t, workflow, "actions/checkout@v4.2.1=sha1-"+parityTestSHA)
		_, stderr, err := runCommandWithHTTP(t, parityDown{&httpmock.Registry{}}, "--verify", path)
		require.ErrorIs(t, err, errSilent, stderr)
		assert.Contains(t, stderr, "Couldn't verify 1 pin: GitHub API returned 502 Bad Gateway\n  actions/checkout@v4.2.1\n")
		assert.NotContains(t, stderr, "actions/checkout@v4.2.1: could not verify", "an error must not also print as a warning")
		assert.Contains(t, stderr, "GitHub API returned 502 Bad Gateway")
		assert.NotContains(t, stderr, "api.github.com", "the request URL stays out of the terminal")
	})
	for _, tt := range []struct {
		name   string
		status string
		base   string
		title  string
	}{
		{"exact tag behind blocks", "ahead", parityTestSHA, "✗ Locked commit isn't the tag's commit actions/checkout@v4.2.1"},
		{"unreachable pin blocks", "diverged", "cccccccccccccccccccccccccccccccccccccccc", "✗ Pinned commit not in upstream history actions/checkout@v4.2.1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reg := &httpmock.Registry{}
			defer reg.Verify(t)
			reg.Register(httpmock.GraphQL(`commit: object\(oid`), httpmock.JSONResponse(map[string]any{
				"data": map[string]any{"a0": map[string]any{
					"nameWithOwner": "actions/checkout",
					"commit":        map[string]any{"oid": parityTestSHA},
					"tag":           map[string]any{"oid": parityTestMoved},
					"tagRef":        map[string]any{"oid": parityTestMoved},
				}},
			}))
			reg.Register(httpmock.REST("GET", `repos/actions/checkout/compare/`),
				httpmock.JSONResponse(map[string]any{"status": tt.status, "merge_base_commit": map[string]any{"sha": tt.base}}))
			path := writeTempWorkflow(t, workflow, "actions/checkout@v4.2.1=sha1-"+parityTestSHA)
			_, stderr, err := runCommandWithHTTP(t, reg, "--verify", path)
			require.ErrorIs(t, err, errSilent, stderr)
			assert.Contains(t, stderr, tt.title)
			assert.NotContains(t, stderr, "Tag now points past locked commit")
			assert.NotContains(t, stderr, "All 1 workflow valid")
		})
	}
	t.Run("exact tag behind explains the fix", func(t *testing.T) {
		reg := &httpmock.Registry{}
		reg.Register(httpmock.GraphQL(`commit: object\(oid`), httpmock.JSONResponse(map[string]any{
			"data": map[string]any{"a0": map[string]any{
				"nameWithOwner": "actions/checkout",
				"commit":        map[string]any{"oid": parityTestSHA},
				"tag":           map[string]any{"oid": parityTestMoved},
				"tagRef":        map[string]any{"oid": parityTestMoved},
			}},
		}))
		reg.Register(httpmock.REST("GET", `repos/actions/checkout/compare/`),
			httpmock.JSONResponse(map[string]any{"status": "ahead", "merge_base_commit": map[string]any{"sha": parityTestSHA}}))
		path := writeTempWorkflow(t, workflow, "actions/checkout@v4.2.1=sha1-"+parityTestSHA)
		_, stderr, err := runCommandWithHTTP(t, reg, "--verify", path)
		require.ErrorIs(t, err, errSilent, stderr)
		assert.Contains(t, stderr, "tag v4.2.1 points at bbbbbbbbbbbb; the lockfile locks 9ddf35b71a48, an older commit in its history")
		assert.Contains(t, stderr, "run `gh actions-lock --relock` to lock the tag's commit, or change `uses:` to the release you want")
	})
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
			"tagRef":        map[string]any{"oid": parityTestMoved},
		}},
	}))
	reg.Register(httpmock.REST("GET", `repos/actions/checkout/compare/`),
		httpmock.JSONResponse(map[string]any{"status": "diverged", "merge_base_commit": map[string]any{"sha": parityTestMoved}}))
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
	assert.Contains(t, stderr, "is not in actions/checkout's history")
}

// TestParity_UnchangedRerunRequestCount guards API kindness: an unchanged
// rerun re-resolves nothing and costs one GraphQL request for the whole
// closure, ref lookups included, plus one batch reading the locked
// manifests at their commits. A mutable ref that moved upstream stays
// locked (sticky) and adds one compare call.
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
		assert.Equal(t, 2, ct.n, "%v: one parity request and one manifest batch for every pin", args)
	}
}

// TestParity_UnchangedRerunRequestCountRESTOnly covers the Dependabot proxy
// path: one repos/{owner}/{repo} call per unique repository, one ref call per
// ref that still points at its locked commit, and a commit and a contents
// call per manifest at a distinct commit.
func TestParity_UnchangedRerunRequestCountRESTOnly(t *testing.T) {
	t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
	reg := &httpmock.Registry{}
	registerLockedManifestsREST(reg)
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
	reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/git/ref/tags/v4$`), httpmock.JSONResponse(httpmock.GitRefResponse("refs/tags/v4", parityTestSHA)))
	reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/git/ref/tags/v4\.2\.1$`), httpmock.JSONResponse(httpmock.GitRefResponse("refs/tags/v4.2.1", parityTestSHA)))
	reg.Register(httpmock.REST("GET", `^/repos/actions/setup-go/git/ref/tags/v5$`), httpmock.JSONResponse(httpmock.GitRefResponse("refs/tags/v5", parityTestSHA)))
	path := writeParityFixture(t)

	p, err := runParity(t, ct, path)
	require.NoError(t, err)
	assert.True(t, p.Valid, "%+v", p.Findings)
	assert.Equal(t, 9, ct.n, "2 repos + 3 pins + 2 manifests: %v", ct.urls)
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
	assert.Contains(t, stderr, "Couldn't verify 1 pin")
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
	assert.Contains(t, stderr, "Couldn't verify 1 pin")
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

// outage fails every request with a server error.
type outage struct{}

func (outage) RoundTrip(req *http.Request) (*http.Response, error) {
	return httpmock.StatusResponse(http.StatusServiceUnavailable)(req)
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
		"rate-limited 403":           {httpmock.StatusResponseWithHeaders(http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "0"}), "GitHub returned 403 Forbidden"},
		"secondary rate-limited 403": {httpmock.StatusResponseWithHeaders(http.StatusForbidden, map[string]string{"Retry-After": "60"}), "GitHub returned 403 Forbidden"},
		"503":                        {httpmock.StatusResponse(http.StatusServiceUnavailable), "GitHub returned 503 Service Unavailable"},
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

// TestParity_InconclusiveBlocksWithoutProxy: a ref check that can't finish
// fails closed, exact tags included; only the proxy turns it into a retry.
func TestParity_InconclusiveBlocksWithoutProxy(t *testing.T) {
	reg := &httpmock.Registry{}
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
	require.ErrorIs(t, err, errSilent, stderr)
	assert.Contains(t, stderr, "Couldn't verify 1 pin")
}

// TestParity_UnreachableAPIBlocks is #199's repro: with the API unreachable
// (connection refused on every request), --verify must not report the
// lockfile valid.
func TestParity_UnreachableAPIBlocks(t *testing.T) {
	path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
`, "actions/checkout@v4=sha1-"+parityTestSHA)

	stdout, stderr, err := runCommandWithHTTP(t, unreachable{}, "--verify", "--json=valid,findings", path)
	require.ErrorIs(t, err, errSilent, stderr)
	var p parityPayload
	require.NoError(t, json.Unmarshal([]byte(stdout), &p), stdout+stderr)
	assert.False(t, p.Valid)
	require.NotEmpty(t, p.Findings)
	for _, f := range p.Findings {
		assert.Equal(t, "reachability-unknown", f.Category)
		assert.Equal(t, "error", f.Severity)
		assert.True(t, f.Retryable)
		assert.Contains(t, f.Detail, "connection refused")
		assert.Equal(t, "couldn't verify; re-run this check once the GitHub API is reachable", f.Remediation)
	}

	_, stderr, err = runCommandWithHTTP(t, unreachable{}, "--verify", path)
	require.ErrorIs(t, err, errSilent)
	assert.Contains(t, stderr, "Couldn't verify 1 pin: GitHub API unreachable (connection refused)\n  actions/checkout@v4\n")
	assert.Contains(t, stderr, "↳ re-run this check once the GitHub API is reachable\n")
	assert.NotContains(t, stderr, "workflow failed", "an outage alone is not a failed workflow")
}

// TestParity_ConfigErrorsAbort: an untrusted certificate or a rejected
// token fails every request the same way, so the run stops with one line
// (exit 2) instead of a finding per pin.
func TestParity_ConfigErrorsAbort(t *testing.T) {
	tests := []struct {
		name string
		rt   http.RoundTripper
		want string
	}{
		{"untrusted certificate", untrustedCert{}, "TLS certificate for api.github.com is not trusted; check HTTPS_PROXY or your CA bundle"},
		{"rejected token", rejectedToken{}, "GitHub rejected the token in GH_TOKEN (401); check it, or unset it to use `gh auth login`"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GH_TOKEN", "test-token")
			path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
`, "actions/checkout@v4=sha1-"+parityTestSHA)

			stdout, _, err := runCommandWithHTTP(t, tt.rt, "--verify", "--json=valid,findings", path)
			require.Error(t, err)
			assert.NotErrorIs(t, err, errSilent, "a tool failure exits 2")
			assert.Equal(t, tt.want, err.Error())
			assert.Empty(t, stdout)
		})
	}
}

type untrustedCert struct{}

func (untrustedCert) raw() {}

func (untrustedCert) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}
}

type rejectedToken struct{}

func (rejectedToken) raw() {}

func (rejectedToken) RoundTrip(req *http.Request) (*http.Response, error) {
	return httpmock.StatusResponse(http.StatusUnauthorized)(req)
}

// unreachable fails every request the way a refused connection does.
type unreachable struct{}

func (unreachable) raw() {}

func (unreachable) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
}

// TestParity_DependabotProxyKeepsRenamedName: Dependabot reads back only
// actions.lock, so under the proxy a same-ID rename must not rewrite
// `uses:` or re-key the pin; the warning still reaches --json.
func TestParity_DependabotProxyKeepsRenamedName(t *testing.T) {
	t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
	reg := &httpmock.Registry{}
	registerLockedManifestsREST(reg)
	reg.Register(httpmock.REST("GET", `^/repos/krzema12/github-actions-typing$`), httpmock.JSONResponse(map[string]any{
		"full_name": "typesafegithub/github-actions-typing", "id": 1, "owner": map[string]any{"id": 42},
	}))
	reg.Register(httpmock.REST("GET", `^/repos/krzema12/github-actions-typing/git/ref/tags/v2\.2\.2$`),
		httpmock.JSONResponse(httpmock.GitRefResponse("refs/tags/v2.2.2", parityTestSHA)))
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
	reg.Register(httpmock.REST("GET", `^/repos/typesafegithub/github-actions-typing/commits/v2\.2\.2$`), commit)
	reg.Register(httpmock.REST("GET", `^/repos/krzema12/github-actions-typing/git/ref/tags/v2\.2\.2$`), httpmock.JSONResponse(httpmock.GitRefResponse("refs/tags/v2.2.2", parityTestSHA)))
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

// TestParity_VerifyMutableRefHistory: --verify checks that a commit locked
// under a mutable ref is still in that ref's history, since a fork-network
// commit exists under the upstream name. A legitimately moved ref stays
// sticky and silent, and an inconclusive check fails closed. A plain run
// makes one compare call.
func TestParity_VerifyMutableRefHistory(t *testing.T) {
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
		for _, proxy := range []bool{false, true} {
			t.Run(tt.name+"/"+map[bool]string{false: "graphql", true: "dependabot proxy"}[proxy], func(t *testing.T) {
				reg := &httpmock.Registry{}
				registerLockedManifestsREST(reg)
				if proxy {
					t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
					useDefaultTransport(t, reg)
					reg.Register(httpmock.REST("GET", `^/repos/actions/checkout$`), httpmock.JSONResponse(map[string]any{
						"full_name": "actions/checkout", "id": 1, "owner": map[string]any{"id": 1},
					}))
					reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/commits/`+parityTestSHA+`$`), httpmock.JSONResponse(map[string]any{"sha": parityTestSHA}))
					reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/git/ref/tags/v4$`), httpmock.JSONResponse(httpmock.GitRefResponse("refs/tags/v4", live)))
				} else {
					reg.Register(httpmock.GraphQL(`head: object\(expression`), httpmock.JSONResponse(map[string]any{
						"data": map[string]any{"a0": map[string]any{
							"nameWithOwner": "actions/checkout",
							"commit":        map[string]any{"oid": parityTestSHA},
							"tag":           map[string]any{"oid": live},
							"tagRef":        map[string]any{"oid": live},
						}},
					}))
				}
				for range 10 {
					reg.Register(httpmock.REST("GET", compareURL), tt.compare)
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

				stdout, stderr, err := runCommandWithHTTP(t, reg, "--verify", "--json=valid,findings", path)
				if proxy && tt.want == "reachability-unknown" {
					require.Error(t, err, stderr)
					assert.NotErrorIs(t, err, errSilent, "inconclusive under the proxy is a retry")
					assert.Contains(t, err.Error(), "GitHub returned 503 Service Unavailable")
					return
				}
				var p parityPayload
				require.NoError(t, json.Unmarshal([]byte(stdout), &p), stdout+stderr)
				switch tt.want {
				case "":
					require.NoError(t, err, stderr)
					assert.Empty(t, p.Findings, "a moved mutable ref stays sticky and silent")
				case "unreachable-pin":
					require.ErrorIs(t, err, errSilent)
					require.Len(t, p.Findings, 1)
					assert.Equal(t, "unreachable-pin", p.Findings[0].Category)
					assert.Equal(t, "error", p.Findings[0].Severity)
					assert.Contains(t, p.Findings[0].Detail, "is not in actions/checkout's history")
					assert.Contains(t, p.Findings[0].Detail, "v4 currently points at cccccccccccc")
					assert.NotEmpty(t, p.Findings[0].DocURL, "the impostor keeps its doc link")
				default:
					require.ErrorIs(t, err, errSilent, stderr)
					require.Len(t, p.Findings, 1)
					assert.Equal(t, tt.want, p.Findings[0].Category)
					assert.Equal(t, "error", p.Findings[0].Severity)
				}
			})
		}
	}

	for _, proxy := range []bool{false, true} {
		t.Run("plain run makes one compare call/"+map[bool]string{false: "graphql", true: "dependabot proxy"}[proxy], func(t *testing.T) {
			reg := &httpmock.Registry{}
			registerLockedManifestsREST(reg)
			if proxy {
				t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
				useDefaultTransport(t, reg)
				reg.Register(httpmock.REST("GET", `^/repos/actions/checkout$`), httpmock.JSONResponse(map[string]any{
					"full_name": "actions/checkout", "id": 1, "owner": map[string]any{"id": 1},
				}))
				reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/commits/`+parityTestSHA+`$`), httpmock.JSONResponse(map[string]any{"sha": parityTestSHA}))
				reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/git/ref/tags/v4$`), httpmock.JSONResponse(httpmock.GitRefResponse("refs/tags/v4", live)))
			} else {
				reg.Register(httpmock.GraphQL(`head: object\(expression`), httpmock.JSONResponse(map[string]any{
					"data": map[string]any{"a0": map[string]any{
						"nameWithOwner": "actions/checkout",
						"commit":        map[string]any{"oid": parityTestSHA},
						"tag":           map[string]any{"oid": live},
						"tagRef":        map[string]any{"oid": live},
					}},
				}))
			}
			reg.Register(httpmock.REST("GET", compareURL), httpmock.JSONResponse(map[string]any{"status": "ahead", "merge_base_commit": map[string]any{"sha": parityTestSHA}}))
			path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
`, "actions/checkout@v4=sha1-"+parityTestSHA)

			_, stderr, err := runCommandWithHTTP(t, reg, "--no-onboard", "--no-narrow", "--no-interactive", path)
			require.NoError(t, err, stderr)
			var compares int
			for _, req := range reg.Requests {
				if strings.Contains(req.URL.Path, "/compare/") {
					compares++
				}
			}
			assert.Equal(t, 1, compares)
		})
	}
}

// TestParity_VerifyMissingMutableRefBlocks: under --verify a mutable ref that
// is no longer a tag or branch is a definitive answer the Actions backend
// resolver rejects, so it blocks rather than passing on the commit alone.
func TestParity_VerifyMissingMutableRefBlocks(t *testing.T) {
	for _, proxy := range []bool{false, true} {
		t.Run(map[bool]string{false: "graphql", true: "dependabot proxy"}[proxy], func(t *testing.T) {
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
					reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/git/ref/tags/v4$`), httpmock.StatusResponse(http.StatusNotFound))
					reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/git/ref/heads/v4$`), httpmock.StatusResponse(http.StatusNotFound))
				} else {
					reg.Register(httpmock.GraphQL(`head: object\(expression`), httpmock.JSONResponse(map[string]any{
						"data": map[string]any{"a0": map[string]any{
							"nameWithOwner": "actions/checkout",
							"commit":        map[string]any{"oid": parityTestSHA},
							"head":          nil,
							"headRef":       nil,
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

			stdout, stderr, err := runCommandWithHTTP(t, reg, "--verify", "--json=valid,findings", path)
			require.ErrorIs(t, err, errSilent, stderr)
			var p parityPayload
			require.NoError(t, json.Unmarshal([]byte(stdout), &p), stdout+stderr)
			require.Len(t, p.Findings, 1)
			assert.Equal(t, "unreachable-pin", p.Findings[0].Category)
			assert.Equal(t, "error", p.Findings[0].Severity)
			assert.Equal(t, "ref v4 is not a tag or branch of actions/checkout; the Actions backend will reject this pin", p.Findings[0].Detail)
			assert.Equal(t, "point `uses:` at a tag or branch that exists, then run `gh actions-lock`", p.Findings[0].Remediation)
			assert.Equal(t, checks.UnreachablePin.DocURL(), p.Findings[0].DocURL, "the unreachable-pin section covers a missing ref")

			stdout, stderr, err = runCommandWithHTTP(t, reg, "--verify", path)
			require.ErrorIs(t, err, errSilent)
			term := stdout + stderr
			assert.Contains(t, term, "Ref no longer exists actions/checkout@v4")
			assert.Contains(t, term, "ref v4 is not a tag or branch of actions/checkout; the Actions backend will reject this pin")
			assert.Contains(t, term, "point `uses:` at a tag or branch that exists, then run `gh actions-lock`")
			assert.NotContains(t, term, "not in upstream history")
			assert.NotContains(t, term, "/releases")
			assert.Contains(t, term, "see: "+checks.UnreachablePin.DocURL())
		})
	}
}

// TestParity_DefaultAndNoFixCheckMutableHistory: fix runs check a mutable
// ref's history like --verify (#174), and block without rewriting the lock.
func TestParity_DefaultAndNoFixCheckMutableHistory(t *testing.T) {
	const live = "cccccccccccccccccccccccccccccccccccccccc"
	tests := []struct {
		name    string
		tag     any
		compare httpmock.Responder
		want    string
	}{
		{"impostor blocks", map[string]any{"oid": live}, httpmock.JSONResponse(map[string]any{"status": "diverged", "merge_base_commit": map[string]any{"sha": parityTestMoved}}), "unreachable-pin"},
		{"inconclusive blocks", map[string]any{"oid": live}, httpmock.StatusResponse(http.StatusServiceUnavailable), "reachability-unknown"},
		{"missing ref blocks", nil, nil, "unreachable-pin"},
	}
	modes := map[string][]string{
		"default": {"--no-onboard", "--no-narrow", "--no-interactive"},
		"no-fix":  {"--no-fix"},
	}
	for _, tt := range tests {
		for mode, args := range modes {
			t.Run(tt.name+"/"+mode, func(t *testing.T) {
				reg := &httpmock.Registry{}
				reg.Register(httpmock.GraphQL(`head: object\(expression`), httpmock.JSONResponse(map[string]any{
					"data": map[string]any{"a0": map[string]any{
						"nameWithOwner": "actions/checkout",
						"commit":        map[string]any{"oid": parityTestSHA},
						"tag":           tt.tag,
						"tagRef":        tt.tag,
					}},
				}))
				if tt.compare != nil {
					for range 10 {
						reg.Register(httpmock.REST("GET", `repos/actions/checkout/compare/`+parityTestSHA+`\.\.\.`+live), tt.compare)
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

				p, err := runParity(t, reg, append(args, path)...)
				require.ErrorIs(t, err, errSilent)
				require.Len(t, p.Findings, 1, "%+v", p.Findings)
				assert.Equal(t, tt.want, p.Findings[0].Category)
				assert.Equal(t, "error", p.Findings[0].Severity)
				assert.Equal(t, lockBefore, readTempLockfilePins(t), "a blocked pin must not be rewritten")
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
			registerLockedManifestsREST(reg)
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
					reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/git/ref/tags/`+ref+`$`), func(req *http.Request) (*http.Response, error) {
						heads = append(heads, ref)
						return httpmock.JSONResponse(httpmock.GitRefResponse("refs/tags/"+ref, parityTestSHA))(req)
					})
				}
			} else {
				reg.Register(httpmock.GraphQL(`head: object\(expression`), func(req *http.Request) (*http.Response, error) {
					body, _ := io.ReadAll(req.Body)
					for _, ref := range []string{"Main", "main"} {
						if strings.Contains(string(body), `"refs/tags/`+ref+`^{commit}"`) {
							heads = append(heads, ref)
						}
					}
					repo := map[string]any{"nameWithOwner": "actions/checkout", "commit": map[string]any{"oid": parityTestSHA}, "head": map[string]any{"oid": parityTestSHA}, "headRef": map[string]any{}}
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
				reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/git/ref/tags/v4$`), httpmock.JSONResponse(httpmock.GitRefResponse("refs/tags/v4", live)))
			} else {
				reg.Register(httpmock.GraphQL(`head: object\(expression`), httpmock.JSONResponse(map[string]any{
					"data": map[string]any{"a0": map[string]any{
						"nameWithOwner": "actions/checkout",
						"commit":        map[string]any{"oid": parityTestSHA},
						"head":          map[string]any{"oid": live},
						"headRef":       map[string]any{"oid": live},
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
			// Unqualified, pull/1/head would resolve to refs/pull/1/head.
			name: "recorded pull request ref blocks", ref: "pull/1/head",
			head: nil,
			want: "unreachable-pin", detail: "ref pull/1/head is not a tag or branch of actions/checkout",
		},
		{
			name: "inconclusive history check blocks", ref: "main",
			head: map[string]any{"oid": live}, compare: httpmock.StatusResponse(http.StatusServiceUnavailable),
			want: "reachability-unknown", detail: "checking main history",
		},
		{
			name: "refused history check blocks", ref: "main",
			head: map[string]any{"oid": live}, compare: httpmock.StatusResponse(http.StatusForbidden),
			want: "repo-unavailable", detail: "checking main history",
		},
		{
			name: "commit in recorded ref passes", ref: "main",
			head: map[string]any{"oid": live}, compare: ahead,
		},
	}
	for _, tt := range tests {
		for mode, args := range fullSHAModes {
			t.Run(tt.name+"/"+mode, func(t *testing.T) {
				reg := &httpmock.Registry{}
				reg.RegisterReusable(httpmock.GraphQL(`commit: object\(oid`), func(req *http.Request) (*http.Response, error) {
					var body struct {
						Variables map[string]any `json:"variables"`
					}
					require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
					assert.Equal(t, "refs/tags/"+tt.ref+"^{commit}", body.Variables["tag0"], "the recorded ref is looked up only as a tag or branch")
					assert.Equal(t, "refs/heads/"+tt.ref+"^{commit}", body.Variables["head0"], "the recorded ref is looked up only as a tag or branch")
					// A --relock also checks the commit it resolves afresh.
					data := map[string]any{}
					for k := range body.Variables {
						if i, ok := strings.CutPrefix(k, "oid"); ok {
							data["a"+i] = map[string]any{
								"nameWithOwner": "actions/checkout",
								"databaseId":    1,
								"commit":        map[string]any{"oid": parityTestSHA},
								"tag":           tt.tag,
								"tagRef":        tt.tag,
								"head":          tt.head,
								"headRef":       tt.head,
							}
						}
					}
					return httpmock.JSONResponse(map[string]any{"data": data})(req)
				})
				if tt.compare != nil {
					reg.Register(httpmock.REST("GET", `repos/actions/checkout/compare/`+parityTestSHA+`\.\.\.`+live), tt.compare)
				}
				if tt.want == "" {
					reg.Register(httpmock.REST("GET", `repos/actions/checkout/tags`), httpmock.JSONResponse(httpmock.TagListResponse("v6.1.0", parityTestSHA)))
				}
				reg.RegisterReusable(httpmock.GraphQLForRepo("actions", "checkout"), httpmock.JSONResponse(map[string]any{
					"data": map[string]any{"a0": testRepoResponse("actions/checkout", parityTestSHA, nodeActionYAML)},
				}))
				path := writeFullSHAWorkflow(t, tt.ref)

				assertFullSHARun(t, reg, args, path, tt.want, tt.detail)
			})
		}
	}
}

// TestParity_InvalidEntryRefIsParseError: an entry ref the Actions backend
// resolver would reject makes the lockfile invalid in every mode, with
// exit 2 and nothing rewritten, so a fix run can't launder it into a `uses:`
// rewrite. TestValidEntryRef covers the other rejected forms.
func TestParity_InvalidEntryRefIsParseError(t *testing.T) {
	tests := []struct {
		name string
		key  string
		ref  string
	}{
		{"full-SHA key records a qualified branch", parityTestSHA, "refs/heads/main"},
		{"full-SHA key records revision syntax", parityTestSHA, "main~1"},
		{"full-SHA key records a describe name", parityTestSHA, "v6.1.0-1-g373d742f768e"},
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
				assert.Empty(t, reg.Requests, "nothing is resolved from an invalid lockfile")
				wfAfter, err := os.ReadFile(path)
				require.NoError(t, err)
				assert.Equal(t, string(wfBefore), string(wfAfter))
				assert.Equal(t, lock, readTempLockfilePins(t))
			})
		}
	}
}

// TestSHARecordedRef_ReadOnlyBlocks: a workflow's full-SHA pin whose entry
// records that SHA as its ref is rejected by the Actions backend. Read-only
// modes report it as an error finding (exit 1), not a load error, and write
// nothing.
func TestSHARecordedRef_ReadOnlyBlocks(t *testing.T) {
	for mode, args := range map[string][]string{
		"verify":            {"--verify"},
		"no-fix":            {"--no-fix"},
		"verify-local":      {"--verify-local"},
		"verify no-onboard": {"--verify", "--no-onboard"},
		"no-fix no-onboard": {"--no-fix", "--no-onboard"},
	} {
		t.Run(mode, func(t *testing.T) {
			reg := &httpmock.Registry{}
			if mode != "verify-local" {
				reg.Register(httpmock.GraphQLForRepo("actions", "checkout"), httpmock.JSONResponse(map[string]any{
					"data": map[string]any{"a0": testRepoResponse("actions/checkout", parityTestSHA, nodeActionYAML)},
				}))
			}
			path := writeFullSHAWorkflow(t, parityTestSHA)
			lockBefore := readTempLockfilePins(t)
			wfBefore, err := os.ReadFile(path)
			require.NoError(t, err)

			stdout, stderr, err := runCommandWithHTTP(t, parityFallback{reg}, append(args, "--no-interactive", "--json=valid,findings", path)...)
			require.ErrorIs(t, err, errSilent, stderr)
			var p parityPayload
			require.NoError(t, json.Unmarshal([]byte(stdout), &p), stdout+stderr)
			assert.False(t, p.Valid)
			var blocking []format.Finding
			for _, f := range p.Findings {
				if f.Severity == "error" {
					blocking = append(blocking, f)
				}
			}
			require.Len(t, blocking, 1, "findings: %+v", p.Findings)
			assert.Equal(t, "not-pinned", blocking[0].Category)
			assert.Contains(t, blocking[0].Detail, "actions/checkout@"+parityTestSHA+" records ref "+parserlock.ShortSHA(parityTestSHA)+", a commit SHA")
			assert.Equal(t, "run `gh actions-lock` to record the tag or branch this commit came from", blocking[0].Remediation)
			wfAfter, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, string(wfBefore), string(wfAfter))
			assert.Equal(t, lockBefore, readTempLockfilePins(t))
		})
	}
}

// TestSHARecordedRef_StaleEntryBlocks: an own-SHA entry the workflow no
// longer uses still fails read-only modes, as a stale entry.
func TestSHARecordedRef_StaleEntryBlocks(t *testing.T) {
	for _, mode := range []string{"--verify", "--no-fix", "--verify-local"} {
		t.Run(mode, func(t *testing.T) {
			path := writeFullSHAWorkflow(t, parityTestSHA)
			require.NoError(t, os.WriteFile(path, []byte("name: ci\non: push\njobs:\n  t:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo\n"), 0o600))
			lockBefore := readTempLockfilePins(t)

			p, err := runParity(t, parityFallback{&httpmock.Registry{}}, mode, "--no-interactive", path)
			require.ErrorIs(t, err, errSilent)
			assert.False(t, p.Valid)
			require.NotEmpty(t, p.Findings)
			assert.Equal(t, "stale", p.Findings[0].Category)
			assert.Equal(t, "error", p.Findings[0].Severity)
			assert.Equal(t, lockBefore, readTempLockfilePins(t))
		})
	}
}

// TestSHARecordedRef_FixReRecords: a fix run re-records a pin whose entry
// records its own SHA with the tag at the commit, else a branch containing
// it, keeping the full-SHA `uses:`. A commit no tag or branch contains, such
// as a fork commit served under the upstream name, still blocks.
func TestSHARecordedRef_FixReRecords(t *testing.T) {
	const head = "dddddddddddddddddddddddddddddddddddddddd"
	tests := []struct {
		name    string
		tags    []string
		compare string
		want    string
		// unconfirmed leaves out the repo ID the identity proof needs.
		unconfirmed bool
		refused     string
	}{
		{name: "tag at the commit", tags: []string{"v6.1.0", parityTestSHA}, compare: "behind", want: "v6.1.0"},
		{name: "branch containing the commit", compare: "behind", want: "main"},
		{name: "on no tag or branch", compare: "diverged", refused: "is not on any tag or branch"},
		{name: "in an unconfirmed repository", tags: []string{"v6.1.0", parityTestSHA}, compare: "behind", unconfirmed: true, refused: "could not confirm actions/checkout is the repository locked"},
	}
	for _, tt := range tests {
		for mode, args := range map[string][]string{"default": nil, "relock": {"--relock"}, "no-onboard": {"--no-onboard"}} {
			t.Run(tt.name+"/"+mode, func(t *testing.T) {
				base := parityTestSHA
				if tt.compare == "diverged" {
					base = "cccccccccccccccccccccccccccccccccccccccc"
				}
				reg := &httpmock.Registry{}
				repo := testRepoResponse("actions/checkout", parityTestSHA, nodeActionYAML)
				if !tt.unconfirmed {
					repo = withRepoID(repo, 1)
				}
				for range 4 {
					reg.Register(httpmock.GraphQLForRepo("actions", "checkout"), httpmock.JSONResponse(map[string]any{
						"data": map[string]any{"a0": repo},
					}))
					reg.Register(httpmock.REST("GET", `repos/actions/checkout$`),
						httpmock.JSONResponse(map[string]any{"full_name": "actions/checkout", "default_branch": "main", "id": 1, "owner": map[string]any{"id": 1}}))
					reg.Register(httpmock.REST("GET", `repos/actions/checkout/git/ref/heads/main`),
						httpmock.JSONResponse(map[string]any{"ref": "refs/heads/main", "object": map[string]any{"sha": head, "type": "commit"}}))
					reg.Register(httpmock.REST("GET", `repos/actions/checkout/tags`), httpmock.JSONResponse(httpmock.TagListResponse(tt.tags...)))
					reg.Register(httpmock.REST("GET", `repos/actions/checkout/releases`), httpmock.JSONResponse([]map[string]any{}))
					reg.Register(httpmock.REST("GET", `repos/actions/checkout/branches`), httpmock.JSONResponse(httpmock.BranchListResponse("main", head)))
					reg.Register(httpmock.REST("GET", `repos/actions/checkout/compare/`),
						httpmock.JSONResponse(map[string]any{"status": tt.compare, "merge_base_commit": map[string]any{"sha": base}}))
				}
				httpmock.NoTagsNamed(reg, "actions", "checkout")
				isolateRunLogs(t)
				path := writeFullSHAWorkflow(t, parityTestSHA)
				lockBefore := readTempLockfilePins(t)
				wfBefore, err := os.ReadFile(path)
				require.NoError(t, err)

				_, stderr, err := runCommandWithHTTP(t, parityFallback{reg}, append(args, "--no-interactive", path)...)
				wfAfter, rerr := os.ReadFile(path)
				require.NoError(t, rerr)
				assert.Equal(t, string(wfBefore), string(wfAfter), "`uses:` keeps the full SHA")
				lock := readTempLockfilePins(t)
				if tt.refused != "" {
					require.ErrorIs(t, err, errSilent, stderr)
					assert.Contains(t, stderr, tt.refused)
					if tt.unconfirmed {
						assert.Contains(t, stderr, "Retry if the check was inconclusive, or change to a verified repository and ref")
						assert.NotContains(t, stderr, "--relock` to re-resolve", "--relock re-records the same entry")
					}
					assert.Equal(t, lockBefore, lock)
					return
				}
				require.NoError(t, err, stderr)
				assert.Contains(t, lock, "'actions/checkout@"+parityTestSHA+"':")
				assert.Contains(t, lock, "ref: '"+tt.want+"'")
				assert.Contains(t, lock, "commit: 'sha1-"+parityTestSHA+"'")
				assert.Equal(t, 1, strings.Count(stderr, "actions/checkout@"+parityTestSHA+": recorded ref was the commit SHA; re-recorded as "+tt.want), stderr)
			})
		}
	}
}

// writeOwnSHAChildLock locks workflow.yml on actions/checkout@v6.1.0 plus an
// actions/cache entry recording its own SHA as its ref, reached through the
// checkout entry's `uses:` when transitive, or by nothing.
func writeOwnSHAChildLock(t *testing.T, transitive bool) (wf, lock string) {
	t.Helper()
	wf = writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6.1.0
`)
	uses := ""
	if transitive {
		uses = "        uses:\n            - 'actions/cache@" + parityTestSHA + "'\n"
	}
	lock = "version: 'v0.0.3'\nworkflows:\n" +
		"    '.github/workflows/workflow.yml':\n        - 'actions/checkout@v6.1.0'\n" +
		"dependencies:\n" +
		"    'actions/cache@" + parityTestSHA + "':\n        ref: '" + parityTestSHA + "'\n        commit: 'sha1-" + parityTestSHA + "'\n        owner_id: 1\n        repo_id: 2\n" +
		"    'actions/checkout@v6.1.0':\n        ref: 'v6.1.0'\n        commit: 'sha1-" + parityTestSHA + "'\n        owner_id: 1\n        repo_id: 1\n" + uses
	require.NoError(t, os.WriteFile(filepath.Join(".github", "workflows", "actions.lock"), []byte(lock), 0o600))
	return wf, lock
}

// TestSHARecordedRef_ChildReadOnlyBlocks: an own-SHA entry reached only
// through another entry, or by nothing, fails read-only modes as an error
// and nothing is written.
func TestSHARecordedRef_ChildReadOnlyBlocks(t *testing.T) {
	for _, transitive := range []bool{true, false} {
		for _, mode := range []string{"--verify", "--no-fix", "--verify-local"} {
			t.Run(fmt.Sprintf("transitive=%v/%s", transitive, mode), func(t *testing.T) {
				wf, lock := writeOwnSHAChildLock(t, transitive)
				p, err := runParity(t, parityFallback{&httpmock.Registry{}}, mode, "--no-interactive", wf)
				require.ErrorIs(t, err, errSilent)
				assert.False(t, p.Valid)
				var blocking []format.Finding
				for _, f := range p.Findings {
					if f.Severity == "error" {
						blocking = append(blocking, f)
					}
				}
				require.Len(t, blocking, 1, "findings: %+v", p.Findings)
				where, fix := "used by no workflow", "run `gh actions-lock` to drop it"
				if transitive {
					where, fix = "used by actions/checkout@v6.1.0", "run `gh actions-lock` without workflow paths to record the tag or branch this commit came from; if none contains it, upgrade or replace actions/checkout@v6.1.0"
				}
				assert.Contains(t, blocking[0].Detail, "lock entry for actions/cache@"+parityTestSHA+", "+where+", records ref "+parserlock.ShortSHA(parityTestSHA))
				assert.Equal(t, fix, blocking[0].Remediation)
				if transitive {
					assert.Equal(t, "actions/checkout@v6.1.0", blocking[0].RequiredBy, "JSON names the composite")
				}
				assert.Equal(t, lock, readTempLockfilePins(t))
			})
		}
	}
}

// TestSHARecordedRef_ChildFixRepairs: a fix run drops an orphan own-SHA
// entry and re-records a transitive one with the tag at its commit, else a
// branch containing it, leaving its parent as it was. A commit no tag or
// branch contains still blocks and nothing is written.
func TestSHARecordedRef_ChildFixRepairs(t *testing.T) {
	const head = "dddddddddddddddddddddddddddddddddddddddd"
	tests := []struct {
		name       string
		transitive bool
		tags       []string
		compare    string
		want       string
	}{
		{name: "orphan dropped", compare: "behind"},
		{name: "transitive tag", transitive: true, tags: []string{"v4.2.0", parityTestSHA}, compare: "behind", want: "v4.2.0"},
		{name: "transitive branch", transitive: true, compare: "behind", want: "main"},
		{name: "transitive on no tag or branch", transitive: true, compare: "diverged"},
	}
	for _, tt := range tests {
		for mode, args := range map[string][]string{"default": nil, "relock": {"--relock"}} {
			t.Run(tt.name+"/"+mode, func(t *testing.T) {
				base := parityTestSHA
				if tt.compare == "diverged" {
					base = "cccccccccccccccccccccccccccccccccccccccc"
				}
				reg := &httpmock.Registry{}
				// The parity stub goes first: a repo stub matches any query for the repo.
				for range 2 {
					reg.Register(httpmock.GraphQL(`commit: object\(oid`), func(req *http.Request) (*http.Response, error) {
						body, err := io.ReadAll(req.Body)
						require.NoError(t, err)
						assert.NotContains(t, string(body), "refs/tags/"+parityTestSHA, "a recorded commit SHA is never looked up as a ref")
						req.Body = io.NopCloser(strings.NewReader(string(body)))
						return parityOKWithIDs(map[string]int{"checkout": 1, "cache": 2})(req)
					})
				}
				for _, repo := range []string{"checkout", "cache"} {
					manifest := nodeActionYAML
					if repo == "checkout" && tt.transitive {
						manifest = "name: c\nruns:\n  using: composite\n  steps:\n    - uses: actions/cache@" + parityTestSHA + "\n"
					}
					for range 4 {
						reg.Register(httpmock.GraphQLForRepo("actions", repo), httpmock.JSONResponse(map[string]any{
							"data": map[string]any{"a0": testRepoResponse("actions/"+repo, parityTestSHA, manifest)},
						}))
						reg.Register(httpmock.REST("GET", `repos/actions/`+repo+`$`),
							httpmock.JSONResponse(map[string]any{"full_name": "actions/" + repo, "default_branch": "main", "id": 1, "owner": map[string]any{"id": 1}}))
						reg.Register(httpmock.REST("GET", `repos/actions/`+repo+`/git/ref/heads/main`),
							httpmock.JSONResponse(map[string]any{"ref": "refs/heads/main", "object": map[string]any{"sha": head, "type": "commit"}}))
						reg.Register(httpmock.REST("GET", `repos/actions/`+repo+`/tags`), httpmock.JSONResponse(httpmock.TagListResponse(tt.tags...)))
						reg.Register(httpmock.REST("GET", `repos/actions/`+repo+`/releases`), httpmock.JSONResponse([]map[string]any{}))
						reg.Register(httpmock.REST("GET", `repos/actions/`+repo+`/branches`), httpmock.JSONResponse(httpmock.BranchListResponse("main", head)))
						reg.Register(httpmock.REST("GET", `repos/actions/`+repo+`/compare/`),
							httpmock.JSONResponse(map[string]any{"status": tt.compare, "merge_base_commit": map[string]any{"sha": base}}))
					}
					httpmock.NoTagsNamed(reg, "actions", repo)
				}
				isolateRunLogs(t)
				wf, lockBefore := writeOwnSHAChildLock(t, tt.transitive)
				wfBefore, err := os.ReadFile(wf)
				require.NoError(t, err)

				_, stderr, err := runCommandWithHTTP(t, parityFallback{reg}, append(args, "--no-interactive")...)
				lock := readTempLockfilePins(t)
				if tt.transitive && tt.want == "" {
					require.ErrorIs(t, err, errSilent, stderr)
					assert.Contains(t, stderr, "is not on any tag or branch of actions/cache: it may be a fork or impostor commit served under the upstream name (used by actions/checkout@v6.1.0)")
					assert.Contains(t, stderr, "upgrade or replace actions/checkout@v6.1.0 with a version whose pin of actions/cache can be verified, or stop using it")
					assert.Equal(t, lockBefore, lock)
					return
				}
				require.NoError(t, err, stderr)
				wfAfter, err := os.ReadFile(wf)
				require.NoError(t, err)
				assert.Equal(t, string(wfBefore), string(wfAfter))
				assert.NotContains(t, lock, "ref: '"+parityTestSHA+"'")
				assert.Contains(t, lock, "'actions/checkout@v6.1.0':\n        ref: 'v6.1.0'")
				if !tt.transitive {
					assert.NotContains(t, lock, "actions/cache")
					return
				}
				assert.Contains(t, lock, "'actions/cache@"+parityTestSHA+"':\n        ref: '"+tt.want+"'")
				assert.Contains(t, lock, "            - 'actions/cache@"+parityTestSHA+"'")
				assert.Equal(t, 1, strings.Count(stderr, "actions/cache@"+parityTestSHA+": recorded ref was the commit SHA; re-recorded as "+tt.want), stderr)
			})
		}
	}
}

// TestSHARecordedRef_RootClosureChecked: a workflow pin whose entry records
// its own SHA is re-resolved, which stops at seeded entries, so parity must
// still check its stored closure: a replaced repository two levels down
// blocks.
func TestSHARecordedRef_RootClosureChecked(t *testing.T) {
	for _, args := range [][]string{{"--verify"}, nil} {
		t.Run(fmt.Sprint(args), func(t *testing.T) {
			reg := &httpmock.Registry{}
			for range 3 {
				reg.Register(httpmock.GraphQL(`commit: object\(oid`), func(req *http.Request) (*http.Response, error) {
					var body struct {
						Variables map[string]any `json:"variables"`
					}
					require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
					data := map[string]any{}
					for i := 0; ; i++ {
						owner, ok := body.Variables[fmt.Sprintf("owner%d", i)].(string)
						if !ok {
							break
						}
						name := body.Variables[fmt.Sprintf("name%d", i)].(string)
						id := 1
						if name == "setup-go" {
							id = 999
						}
						oid := map[string]any{"oid": body.Variables[fmt.Sprintf("oid%d", i)]}
						data[fmt.Sprintf("a%d", i)] = map[string]any{
							"nameWithOwner": owner + "/" + name, "databaseId": id,
							"commit": oid, "tag": oid, "tagRef": oid, "head": oid, "headRef": oid,
						}
					}
					return httpmock.JSONResponse(map[string]any{"data": data})(req)
				})
			}
			composite := "runs:\n  using: composite\n  steps:\n    - uses: actions/cache@v1\n"
			reg.Register(httpmock.GraphQLForRepo("actions", "checkout"), httpmock.JSONResponse(map[string]any{
				"data": map[string]any{"a0": testRepoResponse("actions/checkout", parityTestSHA, composite)},
			}))
			isolateRunLogs(t)
			writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@`+parityTestSHA+`
`)
			entry := func(key, ref string, uses ...string) string {
				out := "    '" + key + "':\n        ref: '" + ref + "'\n        commit: 'sha1-" + parityTestSHA + "'\n        owner_id: 1\n        repo_id: 1\n"
				for i, u := range uses {
					if i == 0 {
						out += "        uses:\n"
					}
					out += "            - '" + u + "'\n"
				}
				return out
			}
			lock := "version: 'v0.0.3'\nworkflows:\n    '.github/workflows/workflow.yml':\n        - 'actions/checkout@" + parityTestSHA + "'\n" +
				"dependencies:\n" + entry("actions/cache@v1", "v1", "actions/setup-go@v1") +
				entry("actions/checkout@"+parityTestSHA, parityTestSHA, "actions/cache@v1") + entry("actions/setup-go@v1", "v1")
			require.NoError(t, os.WriteFile(filepath.Join(".github", "workflows", "actions.lock"), []byte(lock), 0o600))

			p, err := runParity(t, parityFallback{reg}, append(args, "--no-interactive")...)
			require.ErrorIs(t, err, errSilent)
			var replaced bool
			for _, f := range p.Findings {
				replaced = replaced || f.Category == "repo-replaced" && strings.Contains(f.Detail, "actions/setup-go")
			}
			assert.True(t, replaced, "findings: %+v", p.Findings)
			assert.Equal(t, lock, readTempLockfilePins(t))
		})
	}
}

// TestParity_RootUsedBySHAClosureChecked: a tag-keyed root the workflow
// uses by its commit SHA (`uses:` rewritten to the commit) is still used,
// so parity checks its stored closure like any other root's: a replaced
// repository or a commit missing from it blocks, and a healthy one passes.
func TestParity_RootUsedBySHAClosureChecked(t *testing.T) {
	for _, deep := range []string{"replaced", "missing commit", "healthy"} {
		t.Run(deep, func(t *testing.T) {
			reg := &httpmock.Registry{}
			reg.Register(httpmock.GraphQL(`commit: object\(oid`), func(req *http.Request) (*http.Response, error) {
				var body struct {
					Variables map[string]any `json:"variables"`
				}
				require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
				data := map[string]any{}
				for i := 0; ; i++ {
					owner, ok := body.Variables[fmt.Sprintf("owner%d", i)].(string)
					if !ok {
						break
					}
					name := body.Variables[fmt.Sprintf("name%d", i)].(string)
					id := 1
					if deep == "replaced" && name == "cache" {
						id = 999
					}
					oid := map[string]any{"oid": body.Variables[fmt.Sprintf("oid%d", i)]}
					var commit any = oid
					if deep == "missing commit" && name == "cache" {
						commit = nil
					}
					data[fmt.Sprintf("a%d", i)] = map[string]any{
						"nameWithOwner": owner + "/" + name, "databaseId": id,
						"commit": commit, "tag": oid, "tagRef": oid, "head": oid, "headRef": oid,
					}
				}
				return httpmock.JSONResponse(map[string]any{"data": data})(req)
			})
			writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@`+parityTestSHA+`
`)
			lock := "version: 'v0.0.3'\nworkflows:\n    '.github/workflows/workflow.yml':\n        - 'actions/checkout@v6.1.0'\n" +
				"dependencies:\n" +
				"    'actions/cache@v1':\n        ref: 'v1'\n        commit: 'sha1-" + parityTestSHA + "'\n        owner_id: 1\n        repo_id: 1\n" +
				"    'actions/checkout@v6.1.0':\n        ref: 'v6.1.0'\n        commit: 'sha1-" + parityTestSHA + "'\n        owner_id: 1\n        repo_id: 1\n        uses:\n            - 'actions/cache@v1'\n"
			require.NoError(t, os.WriteFile(filepath.Join(".github", "workflows", "actions.lock"), []byte(lock), 0o600))

			p, err := runParity(t, reg, "--verify", "--no-interactive")
			assert.Equal(t, lock, readTempLockfilePins(t))
			if deep == "healthy" {
				require.NoError(t, err, "%+v", p.Findings)
				assert.True(t, p.Valid)
				return
			}
			require.ErrorIs(t, err, errSilent)
			want := map[string]string{"replaced": "repo-replaced", "missing commit": "unreachable-pin"}[deep]
			var found bool
			for _, f := range p.Findings {
				found = found || f.Category == want && f.Severity == "error" && strings.Contains(f.Detail, "actions/cache")
			}
			assert.True(t, found, "findings: %+v", p.Findings)
		})
	}
}

// TestSHARecordedRef_StaleRootRepairs: entry repair follows only the roots
// a fix run keeps. An own-SHA child of a stale root is dropped with it, even
// when no tag or branch contains its commit, and a stale own-SHA root that a
// kept entry still uses is repaired as that entry's child.
func TestSHARecordedRef_StaleRootRepairs(t *testing.T) {
	const head = "dddddddddddddddddddddddddddddddddddddddd"
	entry := func(key, ref string, uses ...string) string {
		out := "    '" + key + "':\n        ref: '" + ref + "'\n        commit: 'sha1-" + parityTestSHA + "'\n        owner_id: 1\n        repo_id: 1\n"
		if len(uses) > 0 {
			out += "        uses:\n"
			for _, u := range uses {
				out += "            - '" + u + "'\n"
			}
		}
		return out
	}
	cache := "actions/cache@" + parityTestSHA
	tests := []struct {
		name    string
		roots   []string
		deps    string
		compare string
		want    string
	}{
		{
			name:    "child of a stale root",
			roots:   []string{"actions/checkout@v6.1.0", "actions/setup-go@v1"},
			deps:    entry(cache, parityTestSHA) + entry("actions/checkout@v6.1.0", "v6.1.0") + entry("actions/setup-go@v1", "v1", cache),
			compare: "diverged",
		},
		{
			name:    "stale root a kept entry uses",
			roots:   []string{"actions/cache@" + parityTestSHA, "actions/checkout@v6.1.0"},
			deps:    entry(cache, parityTestSHA) + entry("actions/checkout@v6.1.0", "v6.1.0", cache),
			compare: "behind",
			want:    "v4.2.0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := parityTestSHA
			if tt.compare == "diverged" {
				base = "cccccccccccccccccccccccccccccccccccccccc"
			}
			reg := &httpmock.Registry{}
			for range 3 {
				reg.Register(httpmock.GraphQL(`commit: object\(oid`), parityOKWithIDs(map[string]int{"checkout": 1, "cache": 1, "setup-go": 1}))
			}
			for _, repo := range []string{"checkout", "cache", "setup-go"} {
				for range 4 {
					reg.Register(httpmock.REST("GET", `repos/actions/`+repo+`$`),
						httpmock.JSONResponse(map[string]any{"full_name": "actions/" + repo, "default_branch": "main", "id": 1, "owner": map[string]any{"id": 1}}))
					reg.Register(httpmock.REST("GET", `repos/actions/`+repo+`/git/ref/heads/main`),
						httpmock.JSONResponse(map[string]any{"ref": "refs/heads/main", "object": map[string]any{"sha": head, "type": "commit"}}))
					tags := []string{}
					if tt.want != "" {
						tags = []string{tt.want, parityTestSHA}
					}
					reg.Register(httpmock.REST("GET", `repos/actions/`+repo+`/tags`), httpmock.JSONResponse(httpmock.TagListResponse(tags...)))
					reg.Register(httpmock.REST("GET", `repos/actions/`+repo+`/releases`), httpmock.JSONResponse([]map[string]any{}))
					reg.Register(httpmock.REST("GET", `repos/actions/`+repo+`/branches`), httpmock.JSONResponse(httpmock.BranchListResponse("main", head)))
					reg.Register(httpmock.REST("GET", `repos/actions/`+repo+`/compare/`),
						httpmock.JSONResponse(map[string]any{"status": tt.compare, "merge_base_commit": map[string]any{"sha": base}}))
				}
				httpmock.NoTagsNamed(reg, "actions", repo)
			}
			isolateRunLogs(t)
			writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6.1.0
`)
			lock := "version: 'v0.0.3'\nworkflows:\n    '.github/workflows/workflow.yml':\n"
			for _, r := range tt.roots {
				lock += "        - '" + r + "'\n"
			}
			lock += "dependencies:\n" + tt.deps
			require.NoError(t, os.WriteFile(filepath.Join(".github", "workflows", "actions.lock"), []byte(lock), 0o600))

			_, stderr, err := runCommandWithHTTP(t, parityFallback{reg}, "--no-interactive")
			require.NoError(t, err, stderr)
			after := readTempLockfilePins(t)
			assert.NotContains(t, after, "ref: '"+parityTestSHA+"'")
			assert.NotContains(t, after, "actions/setup-go")
			assert.Contains(t, after, "    '.github/workflows/workflow.yml':\n        - 'actions/checkout@v6.1.0'\n")
			if tt.want == "" {
				assert.NotContains(t, after, "actions/cache")
				return
			}
			assert.Contains(t, after, "    '"+cache+"':\n        ref: '"+tt.want+"'\n")
		})
	}
}

// TestSHARecordedRef_RepairFollowsTheWrite: entry repair follows what the
// run writes, not the selected workflow's old lock roots. A workflow onboarded
// onto, or replacing a root with, a composite another workflow already
// locked writes that composite's stored `uses:`, so its own-SHA child is
// repaired, or blocks the run when no tag or branch contains its commit.
func TestSHARecordedRef_RepairFollowsTheWrite(t *testing.T) {
	const head = "dddddddddddddddddddddddddddddddddddddddd"
	for _, tt := range []struct {
		name       string
		repairable bool
		// replaces gives new.yml an existing lock root its `uses:` no
		// longer names, so the run replaces it with the shared composite.
		replaces bool
		// identity is how the repair candidate's own identity check, which
		// no other check in the run covers, answers: "" confirms it.
		identity string
	}{
		{"onboarding/repairable", true, false, ""},
		{"onboarding/unrepairable", false, false, ""},
		{"root replacement/repairable", true, true, ""},
		{"root replacement/unrepairable", false, true, ""},
		{"onboarding/replaced repository", true, false, "replaced"},
		{"onboarding/unconfirmed identity", true, false, "inconclusive"},
		{"onboarding/missing repository ID", true, false, "no id"},
	} {
		repairable := tt.repairable
		t.Run(tt.name, func(t *testing.T) {
			base, compare, tags := parityTestSHA, "behind", []string{"v4.2.0", parityTestSHA}
			if !repairable {
				base, compare, tags = "cccccccccccccccccccccccccccccccccccccccc", "diverged", nil
			}
			reg := &httpmock.Registry{}
			candidateChecks := 0
			for range 4 {
				reg.Register(httpmock.GraphQL(`commit: object\(oid`), func(req *http.Request) (*http.Response, error) {
					var body struct {
						Variables map[string]any `json:"variables"`
					}
					require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
					data := map[string]any{}
					for i := 0; ; i++ {
						owner, ok := body.Variables[fmt.Sprintf("owner%d", i)].(string)
						if !ok {
							break
						}
						name := body.Variables[fmt.Sprintf("name%d", i)].(string)
						_, hasHead := body.Variables[fmt.Sprintf("head%d", i)]
						var id any = 1
						if name == "cache" && !hasHead {
							candidateChecks++
							switch tt.identity {
							case "inconclusive":
								continue
							case "replaced":
								id = 999
							case "no id":
								id = nil
							}
						}
						oid := map[string]any{"oid": body.Variables[fmt.Sprintf("oid%d", i)]}
						data[fmt.Sprintf("a%d", i)] = map[string]any{
							"nameWithOwner": owner + "/" + name, "databaseId": id,
							"commit": oid, "tag": oid, "tagRef": oid, "head": oid, "headRef": oid,
						}
					}
					return httpmock.JSONResponse(map[string]any{"data": data})(req)
				})
			}
			for _, repo := range []string{"checkout", "cache", "setup-go"} {
				for range 4 {
					reg.Register(httpmock.REST("GET", `repos/actions/`+repo+`$`),
						httpmock.JSONResponse(map[string]any{"full_name": "actions/" + repo, "default_branch": "main", "id": 1, "owner": map[string]any{"id": 1}}))
					reg.Register(httpmock.REST("GET", `repos/actions/`+repo+`/git/ref/heads/main`),
						httpmock.JSONResponse(map[string]any{"ref": "refs/heads/main", "object": map[string]any{"sha": head, "type": "commit"}}))
					reg.Register(httpmock.REST("GET", `repos/actions/`+repo+`/tags`), httpmock.JSONResponse(httpmock.TagListResponse(tags...)))
					reg.Register(httpmock.REST("GET", `repos/actions/`+repo+`/releases`), httpmock.JSONResponse([]map[string]any{}))
					reg.Register(httpmock.REST("GET", `repos/actions/`+repo+`/branches`), httpmock.JSONResponse(httpmock.BranchListResponse("main", head)))
					reg.Register(httpmock.REST("GET", `repos/actions/`+repo+`/compare/`),
						httpmock.JSONResponse(map[string]any{"status": compare, "merge_base_commit": map[string]any{"sha": base}}))
				}
				httpmock.NoTagsNamed(reg, "actions", repo)
			}
			isolateRunLogs(t)
			writeTempWorkflow(t, "on: push\njobs:\n  t:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo\n")
			body := "on: push\njobs:\n  t:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@v6.1.0\n"
			for _, wf := range []string{"old.yml", "new.yml"} {
				require.NoError(t, os.WriteFile(filepath.Join(".github", "workflows", wf), []byte(body), 0o600))
			}
			cache := "actions/cache@" + parityTestSHA
			newRoot, setupGo := "", ""
			if tt.replaces {
				newRoot = "    '.github/workflows/new.yml':\n        - 'actions/setup-go@v1'\n"
				setupGo = "    'actions/setup-go@v1':\n        ref: 'v1'\n        commit: 'sha1-" + parityTestSHA + "'\n        owner_id: 1\n        repo_id: 1\n"
			}
			lock := "version: 'v0.0.3'\nworkflows:\n" + newRoot +
				"    '.github/workflows/old.yml':\n        - 'actions/checkout@v6.1.0'\n" +
				"    '.github/workflows/workflow.yml': []\n" +
				"dependencies:\n" +
				"    '" + cache + "':\n        ref: '" + parityTestSHA + "'\n        commit: 'sha1-" + parityTestSHA + "'\n        owner_id: 1\n        repo_id: 1\n" +
				"    'actions/checkout@v6.1.0':\n        ref: 'v6.1.0'\n        commit: 'sha1-" + parityTestSHA + "'\n        owner_id: 1\n        repo_id: 1\n        uses:\n            - '" + cache + "'\n" + setupGo
			require.NoError(t, os.WriteFile(filepath.Join(".github", "workflows", "actions.lock"), []byte(lock), 0o600))

			_, stderr, err := runCommandWithHTTP(t, parityFallback{reg}, "--no-interactive", filepath.Join(".github", "workflows", "new.yml"))
			after := readTempLockfilePins(t)
			assert.Equal(t, 1, candidateChecks, "the candidate's identity is checked once")
			if tt.identity != "" {
				require.ErrorIs(t, err, errSilent, stderr)
				assert.Contains(t, stderr, "actions/cache@"+parityTestSHA)
				assert.Contains(t, stderr, "Retry if the check was inconclusive, or change to a verified repository and ref; for a pin a composite uses, upgrade or replace actions/checkout@v6.1.0")
				assert.NotContains(t, stderr, "--relock", "--relock re-records the same entry")
				assert.Equal(t, lock, after)
				return
			}
			if !repairable {
				require.ErrorIs(t, err, errSilent, stderr)
				assert.Contains(t, stderr, "is not on any tag or branch of actions/cache")
				assert.Equal(t, lock, after)
				return
			}
			require.NoError(t, err, stderr)
			assert.Contains(t, after, "    '.github/workflows/new.yml':\n        - 'actions/checkout@v6.1.0'\n")
			assert.Contains(t, after, "    '"+cache+"':\n        ref: 'v4.2.0'\n")
			assert.NotContains(t, after, "ref: '"+parityTestSHA+"'")
			assert.NotContains(t, after, "actions/setup-go")
		})
	}
}

// TestSHARecordedRef_SameCommitTagAndSHA: a workflow that locks one commit
// under its tag and under its own SHA, which a composite also uses, still
// gets the SHA entry re-recorded.
func TestSHARecordedRef_SameCommitTagAndSHA(t *testing.T) {
	const head = "dddddddddddddddddddddddddddddddddddddddd"
	reg := &httpmock.Registry{}
	// The parity and manifest stubs go first: a repo stub matches any query
	// for the repo.
	registerLockedManifests(reg, nil)
	for range 3 {
		reg.Register(httpmock.GraphQL(`commit: object\(oid`), parityOKWithIDs(map[string]int{"checkout": 1, "cache": 1}))
	}
	for _, repo := range []string{"checkout", "cache"} {
		for range 4 {
			reg.Register(httpmock.GraphQLForRepo("actions", repo), httpmock.JSONResponse(map[string]any{
				"data": map[string]any{"a0": withRepoID(testRepoResponse("actions/"+repo, parityTestSHA, nodeActionYAML), 1)},
			}))
			reg.Register(httpmock.REST("GET", `repos/actions/`+repo+`$`),
				httpmock.JSONResponse(map[string]any{"full_name": "actions/" + repo, "default_branch": "main", "id": 1, "owner": map[string]any{"id": 1}}))
			reg.Register(httpmock.REST("GET", `repos/actions/`+repo+`/git/ref/heads/main`),
				httpmock.JSONResponse(map[string]any{"ref": "refs/heads/main", "object": map[string]any{"sha": head, "type": "commit"}}))
			reg.Register(httpmock.REST("GET", `repos/actions/`+repo+`/tags`), httpmock.JSONResponse(httpmock.TagListResponse("v4.2.0", parityTestSHA)))
			reg.Register(httpmock.REST("GET", `repos/actions/`+repo+`/releases`), httpmock.JSONResponse([]map[string]any{}))
			reg.Register(httpmock.REST("GET", `repos/actions/`+repo+`/branches`), httpmock.JSONResponse(httpmock.BranchListResponse("main", head)))
			reg.Register(httpmock.REST("GET", `repos/actions/`+repo+`/compare/`),
				httpmock.JSONResponse(map[string]any{"status": "behind", "merge_base_commit": map[string]any{"sha": parityTestSHA}}))
		}
		httpmock.NoTagsNamed(reg, "actions", repo)
	}
	isolateRunLogs(t)
	cache := "actions/cache@" + parityTestSHA
	writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6.1.0
      - uses: actions/cache@v4.2.0
      - uses: `+cache+`
`)
	entry := func(key, ref string, uses ...string) string {
		out := "    '" + key + "':\n        ref: '" + ref + "'\n        commit: 'sha1-" + parityTestSHA + "'\n        owner_id: 1\n        repo_id: 1\n"
		for i, u := range uses {
			if i == 0 {
				out += "        uses:\n"
			}
			out += "            - '" + u + "'\n"
		}
		return out
	}
	lock := "version: 'v0.0.3'\nworkflows:\n    '.github/workflows/workflow.yml':\n" +
		"        - '" + cache + "'\n        - 'actions/cache@v4.2.0'\n        - 'actions/checkout@v6.1.0'\n" +
		"dependencies:\n" + entry(cache, parityTestSHA) + entry("actions/cache@v4.2.0", "v4.2.0") + entry("actions/checkout@v6.1.0", "v6.1.0", cache)
	require.NoError(t, os.WriteFile(filepath.Join(".github", "workflows", "actions.lock"), []byte(lock), 0o600))

	_, stderr, err := runCommandWithHTTP(t, parityFallback{reg}, "--no-interactive", "--no-narrow")
	require.NoError(t, err, stderr)
	after := readTempLockfilePins(t)
	assert.NotContains(t, after, "ref: '"+parityTestSHA+"'")
	assert.Contains(t, after, "            - '"+cache+"'\n")
}

// TestSHARecordedRef_InconclusiveIdentityBlocks: an own-SHA entry's parity
// check confirms only its repository and commit, so if that check can't
// finish the run fails closed, as for a pin it resolved itself, even when
// a tag for the commit is found.
func TestSHARecordedRef_InconclusiveIdentityBlocks(t *testing.T) {
	const head = "dddddddddddddddddddddddddddddddddddddddd"
	reg := &httpmock.Registry{}
	// Parity answers for checkout but leaves the own-SHA cache entry out.
	for range 3 {
		reg.Register(httpmock.GraphQL(`commit: object\(oid`), func(req *http.Request) (*http.Response, error) {
			var body struct {
				Variables map[string]any `json:"variables"`
			}
			require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
			data := map[string]any{}
			for i := 0; ; i++ {
				owner, ok := body.Variables[fmt.Sprintf("owner%d", i)].(string)
				if !ok {
					break
				}
				name := body.Variables[fmt.Sprintf("name%d", i)].(string)
				if name == "cache" {
					continue
				}
				oid := map[string]any{"oid": body.Variables[fmt.Sprintf("oid%d", i)]}
				data[fmt.Sprintf("a%d", i)] = map[string]any{
					"nameWithOwner": owner + "/" + name, "databaseId": 1,
					"commit": oid, "tag": oid, "tagRef": oid, "head": oid, "headRef": oid,
				}
			}
			return httpmock.JSONResponse(map[string]any{"data": data})(req)
		})
	}
	for range 4 {
		reg.Register(httpmock.REST("GET", `repos/actions/cache$`),
			httpmock.JSONResponse(map[string]any{"full_name": "actions/cache", "default_branch": "main", "id": 1, "owner": map[string]any{"id": 1}}))
		reg.Register(httpmock.REST("GET", `repos/actions/cache/git/ref/heads/main`),
			httpmock.JSONResponse(map[string]any{"ref": "refs/heads/main", "object": map[string]any{"sha": head, "type": "commit"}}))
		reg.Register(httpmock.REST("GET", `repos/actions/cache/tags`), httpmock.JSONResponse(httpmock.TagListResponse("v4.2.0", parityTestSHA)))
		reg.Register(httpmock.REST("GET", `repos/actions/cache/releases`), httpmock.JSONResponse([]map[string]any{}))
		reg.Register(httpmock.REST("GET", `repos/actions/cache/branches`), httpmock.JSONResponse(httpmock.BranchListResponse("main", head)))
		reg.Register(httpmock.REST("GET", `repos/actions/cache/compare/`),
			httpmock.JSONResponse(map[string]any{"status": "behind", "merge_base_commit": map[string]any{"sha": parityTestSHA}}))
	}
	httpmock.NoTagsNamed(reg, "actions", "cache")
	isolateRunLogs(t)
	wf, lock := writeOwnSHAChildLock(t, true)

	_, stderr, err := runCommandWithHTTP(t, reg, "--no-interactive", wf)
	require.ErrorIs(t, err, errSilent, stderr)
	assert.Contains(t, stderr, "could not verify locked actions/cache")
	assert.Equal(t, lock, readTempLockfilePins(t))
}

// TestSHARecordedRef_CoveredByAnyUseBlocks: an own-SHA entry the workflow
// uses by its SHA in other letter case still fails read-only modes. (A
// full-SHA key whose commit differs from it never loads, so no other use
// can cover such an entry.)
func TestSHARecordedRef_CoveredByAnyUseBlocks(t *testing.T) {
	for _, tt := range []struct{ name, key, commit, use string }{
		{"uppercase use", parityTestSHA, parityTestSHA, strings.ToUpper(parityTestSHA)},
	} {
		for _, mode := range []string{"--verify-local", "--verify", "--no-fix"} {
			t.Run(tt.name+"/"+mode, func(t *testing.T) {
				wf := writeTempWorkflow(t, "on: push\njobs:\n  t:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@"+tt.use+"\n")
				key := "actions/checkout@" + tt.key
				lock := "version: 'v0.0.3'\nworkflows:\n    '.github/workflows/workflow.yml':\n        - '" + key + "'\n" +
					"dependencies:\n    '" + key + "':\n        ref: '" + tt.key + "'\n        commit: 'sha1-" + tt.commit + "'\n        owner_id: 1\n        repo_id: 1\n"
				require.NoError(t, os.WriteFile(filepath.Join(".github", "workflows", "actions.lock"), []byte(lock), 0o600))

				p, err := runParity(t, parityFallback{&httpmock.Registry{}}, mode, "--no-interactive", wf)
				require.ErrorIs(t, err, errSilent, "%+v", p.Findings)
				assert.False(t, p.Valid)
				var found bool
				for _, f := range p.Findings {
					found = found || f.Severity == "error" && strings.Contains(f.Detail, key) && strings.Contains(f.Detail, "a commit SHA rather than the tag or branch")
				}
				assert.True(t, found, "findings: %+v", p.Findings)
				assert.Equal(t, lock, readTempLockfilePins(t))
			})
		}
	}
}

// TestSHARecordedRef_ScopedFixWarns: a fix run given workflow paths warns
// about an own-SHA entry of a workflow it didn't select, names the entry and
// the full-scan repair, never claims every workflow is valid, still writes
// the selected workflow and exits 0, and leaves the unselected workflow and
// its entry untouched.
func TestSHARecordedRef_ScopedFixWarns(t *testing.T) {
	const head = "dddddddddddddddddddddddddddddddddddddddd"
	for _, locked := range []bool{true, false} {
		for mode, args := range map[string][]string{"default": nil, "relock": {"--relock"}} {
			t.Run(fmt.Sprintf("selected locked=%v/%s", locked, mode), func(t *testing.T) {
				reg := &httpmock.Registry{}
				for range 4 {
					reg.Register(httpmock.GraphQLForRepo("actions", "checkout"), httpmock.JSONResponse(map[string]any{
						"data": map[string]any{"a0": testRepoResponse("actions/checkout", parityTestSHA, nodeActionYAML)},
					}))
					reg.Register(httpmock.REST("GET", `repos/actions/checkout$`),
						httpmock.JSONResponse(map[string]any{"full_name": "actions/checkout", "default_branch": "main", "id": 1, "owner": map[string]any{"id": 1}}))
					reg.Register(httpmock.REST("GET", `repos/actions/checkout/git/ref/heads/main`),
						httpmock.JSONResponse(map[string]any{"ref": "refs/heads/main", "object": map[string]any{"sha": head, "type": "commit"}}))
					reg.Register(httpmock.REST("GET", `repos/actions/checkout/tags`), httpmock.JSONResponse(httpmock.TagListResponse("v6.1.0", parityTestSHA)))
					reg.Register(httpmock.REST("GET", `repos/actions/checkout/releases`), httpmock.JSONResponse([]map[string]any{}))
					reg.Register(httpmock.REST("GET", `repos/actions/checkout/branches`), httpmock.JSONResponse(httpmock.BranchListResponse("main", head)))
				}
				httpmock.NoTagsNamed(reg, "actions", "checkout")
				isolateRunLogs(t)
				ci := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6.1.0
`)
				other := filepath.Join(".github", "workflows", "other.yml")
				otherBody := "on: push\njobs:\n  t:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@" + parityTestSHA + "\n"
				require.NoError(t, os.WriteFile(other, []byte(otherBody), 0o600))
				otherLock := "    '.github/workflows/other.yml':\n        - 'actions/checkout@" + parityTestSHA + "'\n"
				legacy := "    'actions/checkout@" + parityTestSHA + "':\n        ref: '" + parityTestSHA + "'\n        commit: 'sha1-" + parityTestSHA + "'\n        owner_id: 1\n        repo_id: 1\n"
				lock := "version: 'v0.0.3'\nworkflows:\n" + otherLock + "dependencies:\n" + legacy
				if locked {
					lock = "version: 'v0.0.3'\nworkflows:\n" + otherLock +
						"    '.github/workflows/workflow.yml':\n        - 'actions/checkout@v6.1.0'\n" +
						"dependencies:\n" + legacy +
						"    'actions/checkout@v6.1.0':\n        ref: 'v6.1.0'\n        commit: 'sha1-" + parityTestSHA + "'\n        owner_id: 1\n        repo_id: 1\n"
				}
				require.NoError(t, os.WriteFile(filepath.Join(".github", "workflows", "actions.lock"), []byte(lock), 0o600))

				// An unselected workflow's entry is no repair candidate: the
				// run spends no identity check on it.
				headless := 0
				counting := proximaTransport(func(req *http.Request) (*http.Response, error) {
					if req.Body != nil && httpmock.GraphQL(`commit: object\(oid`)(req) {
						b, err := io.ReadAll(req.Body)
						require.NoError(t, err)
						var body struct {
							Variables map[string]any `json:"variables"`
						}
						require.NoError(t, json.Unmarshal(b, &body))
						for i := 0; ; i++ {
							if _, ok := body.Variables[fmt.Sprintf("owner%d", i)]; !ok {
								break
							}
							if _, ok := body.Variables[fmt.Sprintf("head%d", i)]; !ok {
								headless++
							}
						}
						req.Body = io.NopCloser(bytes.NewReader(b))
					}
					return parityFallback{reg}.RoundTrip(req)
				})
				_, stderr, err := runCommandWithHTTP(t, counting, append(args, "--no-interactive", ci)...)
				assert.Zero(t, headless, "no identity check for an entry the run doesn't rewrite")
				require.NoError(t, err, stderr)
				assert.Contains(t, stderr, "actions/checkout@"+parityTestSHA+", in .github/workflows/other.yml, records ref "+parserlock.ShortSHA(parityTestSHA))
				assert.Contains(t, stderr, "run `gh actions-lock` without a path to repair it")
				assert.NotRegexp(t, `All \d+ workflows? valid`, stderr)
				gotOther, err := os.ReadFile(other)
				require.NoError(t, err)
				assert.Equal(t, otherBody, string(gotOther))
				after := readTempLockfilePins(t)
				assert.Contains(t, after, otherLock)
				assert.Contains(t, after, legacy)
				assert.Contains(t, after, "    'actions/checkout@v6.1.0':\n        ref: 'v6.1.0'\n")
				assert.Contains(t, after, "    '.github/workflows/workflow.yml':\n        - 'actions/checkout@v6.1.0'\n")
			})
		}
	}
}

// TestParity_NonBranchRefRefused: a key whose ref is neither a tag nor a
// branch resolves by rev-parse rules but the Actions backend resolver rejects
// it, so no mode may write a lock for it, including --relock, which otherwise
// drops findings that re-resolving would repair. An existing lock entry
// recording a refs/ name is already invalid.
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
						assert.NotErrorIs(t, err, errSilent, "an invalid lockfile exits 2")
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

// TestParity_RevisionSyntaxRefRefused: a `uses:` ref the Actions backend
// doesn't accept as a tag or branch name, such as `main~1` or a
// `git describe` name, may still resolve by rev-parse rules. Parity never
// builds a lookup expression from it, so nothing is locked, even when a tag
// by that name exists.
func TestParity_RevisionSyntaxRefRefused(t *testing.T) {
	for _, ref := range []string{"main~1", "v6.1.0-1-g373d742f768e"} {
		t.Run(ref, func(t *testing.T) { testRefusedRefName(t, ref) })
		t.Run(ref+"/dependabot proxy", func(t *testing.T) {
			t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
			reg := &httpmock.Registry{}
			reg.Register(httpmock.REST("GET", `^/repos/actions/checkout$`), httpmock.JSONResponse(map[string]any{
				"full_name": "actions/checkout", "id": 1, "owner": map[string]any{"id": 1},
			}))
			// Resolution still evaluates the name; parity must refuse it.
			for range 3 {
				reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/commits/`), httpmock.JSONResponse(map[string]any{"sha": parityTestSHA}))
			}
			reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/contents/action\.yml$`), func(req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(nodeActionYAML)), Request: req}, nil
			})
			reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/git/ref/`), func(req *http.Request) (*http.Response, error) {
				t.Errorf("a refused name must not be looked up: %s", req.URL.Path)
				return httpmock.StatusResponse(http.StatusNotFound)(req)
			})
			useDefaultTransport(t, reg)
			path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@`+ref+`
`)

			_, stderr, err := runCommandWithHTTP(t, reg, "--no-narrow", "--no-interactive", path)
			require.ErrorIs(t, err, errSilent, stderr)
			assert.Contains(t, stderr, refusalDetail(ref))
			assert.NoFileExists(t, filepath.Join(".github", "workflows", "actions.lock"))
		})
	}
}

func testRefusedRefName(t *testing.T, ref string) {
	reg := &httpmock.Registry{}
	// One run for --json, one for the terminal.
	for range 2 {
		reg.Register(httpmock.GraphQL(`commit: object\(oid`), func(req *http.Request) (*http.Response, error) {
			var body struct {
				Variables map[string]any `json:"variables"`
			}
			require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
			assert.NotContains(t, body.Variables, "tag0", "revision syntax must not reach a lookup expression")
			assert.NotContains(t, body.Variables, "head0", "revision syntax must not reach a lookup expression")
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
	}
	path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@`+ref+`
`)

	p, err := runParity(t, reg, "--no-narrow", "--no-interactive", path)
	require.ErrorIs(t, err, errSilent)
	var unreachable []string
	for _, f := range p.Findings {
		if f.Category == "unreachable-pin" {
			unreachable = append(unreachable, f.Detail)
		}
	}
	require.Len(t, unreachable, 1, "%+v", p.Findings)
	assert.Contains(t, unreachable[0], refusalDetail(ref))
	assert.NoFileExists(t, filepath.Join(".github", "workflows", "actions.lock"))

	_, stderr, err := runCommandWithHTTP(t, reg, "--no-narrow", "--no-interactive", path)
	require.ErrorIs(t, err, errSilent)
	title := "Not a tag or branch"
	if ghapi.DescribeName(ref) {
		title = "Ref name refused"
	}
	assert.Contains(t, stderr, title+" actions/checkout@"+ref, "the lockfile never recorded it")
	assert.NotContains(t, stderr, "Ref no longer exists")
}

// TestParity_RelockKeepsFreshPinFailures: --relock ignores recorded pins'
// findings because it replaces them, but a pin it just resolved is what it
// would write, so its failures still block.
func TestParity_RelockKeepsFreshPinFailures(t *testing.T) {
	reg := &httpmock.Registry{}
	// The parity stub goes first: the resolve stub matches any query for the repo.
	reg.Register(httpmock.GraphQL(`commit: object\(oid`), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": map[string]any{
			"nameWithOwner": "actions/checkout",
			"databaseId":    1,
			"commit":        map[string]any{"oid": parityTestSHA},
			"tag":           map[string]any{"oid": parityTestMoved},
			"tagRef":        map[string]any{"oid": parityTestMoved},
		}},
	}))
	reg.Register(httpmock.GraphQLForRepo("actions", "checkout"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": testRepoResponse("actions/checkout", parityTestSHA, nodeActionYAML)},
	}))
	reg.Register(httpmock.REST("GET", `^/repos/actions/checkout$`), httpmock.JSONResponse(map[string]any{
		"full_name": "actions/checkout", "id": 1, "owner": map[string]any{"id": 1},
	}))
	reg.Register(httpmock.REST("GET", `compare/`), httpmock.JSONResponse(map[string]any{
		"status": "diverged", "merge_base_commit": map[string]any{"sha": "cccccccccccccccccccccccccccccccccccccccc"},
	}))
	path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v1.0.0
`)

	p, err := runParity(t, reg, "--relock", "--no-narrow", "--no-interactive", path)
	require.ErrorIs(t, err, errSilent)
	var categories []string
	for _, f := range p.Findings {
		categories = append(categories, f.Category)
	}
	assert.Contains(t, categories, "unreachable-pin")
	assert.NoFileExists(t, filepath.Join(".github", "workflows", "actions.lock"))
}

// TestParity_RelockOutageKeepsRecordedPinBlocking: --relock ignores a
// recorded pin's findings only because a fresh pin replaces it. When
// resolution fails too, nothing replaces the entry, so a parity check that
// couldn't finish must still block.
func TestParity_RelockOutageKeepsRecordedPinBlocking(t *testing.T) {
	rt := outage{}
	useDefaultTransport(t, rt)
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

	p, err := runParity(t, rt, "--relock", "--no-interactive", path)
	require.ErrorIs(t, err, errSilent, "%+v", p.Findings)
	assert.False(t, p.Valid)
	var blocking []string
	for _, f := range p.Findings {
		if f.Severity == "error" {
			blocking = append(blocking, f.Detail)
		}
	}
	require.Len(t, blocking, 1, "%+v", p.Findings)
	assert.Contains(t, blocking[0], "could not verify locked actions/checkout")
	assert.Equal(t, lockBefore, readTempLockfilePins(t))
}

// TestParity_RelockKeepsUnresolvedChildBlocking: a composite that resolves
// doesn't vouch for a child that didn't. If the child's resolution and its
// parity check both fail, nothing replaces its recorded entry, so --relock
// must block rather than keep it unverified.
func TestParity_RelockKeepsUnresolvedChildBlocking(t *testing.T) {
	const parentSHA = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	composite := "name: c\nruns:\n  using: composite\n  steps:\n    - uses: actions/checkout@v4\n"
	reg := &httpmock.Registry{}
	reg.RegisterReusable(httpmock.GraphQL(`commit: object\(oid`), func(req *http.Request) (*http.Response, error) {
		var body struct {
			Variables map[string]any `json:"variables"`
		}
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		data := map[string]any{}
		for k, v := range body.Variables {
			i, ok := strings.CutPrefix(k, "oid")
			if !ok {
				continue
			}
			nwo := body.Variables["owner"+i].(string) + "/" + body.Variables["name"+i].(string)
			// The child's tag moved, so its history check needs a compare.
			head := v
			if nwo == "actions/checkout" {
				head = parityTestMoved
			}
			data["a"+i] = map[string]any{
				"nameWithOwner": nwo,
				"databaseId":    1,
				"commit":        map[string]any{"oid": v},
				"tag":           map[string]any{"oid": head},
				"tagRef":        map[string]any{"oid": head},
			}
		}
		return httpmock.JSONResponse(map[string]any{"data": data})(req)
	})
	reg.RegisterReusable(httpmock.GraphQLForRepo("octo", "composite"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": testRepoResponse("octo/composite", parentSHA, composite)},
	}))
	rt := reposDown{reg, []string{"actions/checkout"}}
	useDefaultTransport(t, rt)
	path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: octo/composite@v1
`)
	lock := "version: '" + parserlock.Version + "'\ndependencies:\n" +
		"  'octo/composite@v1':\n    ref: 'v1'\n    commit: 'sha1-" + parentSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"    uses:\n      - 'actions/checkout@v4'\n" +
		"  'actions/checkout@v4':\n    ref: 'v4'\n    commit: 'sha1-" + parityTestSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"workflows:\n  '.github/workflows/workflow.yml':\n    - 'octo/composite@v1'\n"
	lockPath := filepath.Join(".github", "workflows", "actions.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(lock), 0o600))

	_, stderr, err := runCommandWithHTTP(t, rt, "--relock", "--no-narrow", "--no-interactive", path)
	require.ErrorIs(t, err, errSilent, stderr)
	assert.Contains(t, stderr, "could not be resolved")
	after, rerr := os.ReadFile(lockPath)
	require.NoError(t, rerr)
	assert.Equal(t, lock, string(after), "nothing is written")
}

// TestParity_RelockDropsChildTheCompositeDropped: the composite moved to a
// commit that no longer uses a recorded child. --relock rebuilds the
// composite's edges from its manifest, so the child's entry goes, and its
// failed check can't block a lock that no longer records it.
func TestParity_RelockDropsChildTheCompositeDropped(t *testing.T) {
	const rootSHA = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	const liveRoot = "ffffffffffffffffffffffffffffffffffffffff"
	reg := &httpmock.Registry{}
	reg.RegisterReusable(httpmock.GraphQL(`commit: object\(oid`), func(req *http.Request) (*http.Response, error) {
		var body struct {
			Variables map[string]any `json:"variables"`
		}
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		data := map[string]any{}
		for k, v := range body.Variables {
			i, ok := strings.CutPrefix(k, "oid")
			if !ok {
				continue
			}
			nwo := body.Variables["owner"+i].(string) + "/" + body.Variables["name"+i].(string)
			repo := map[string]any{"nameWithOwner": nwo, "databaseId": 1, "commit": map[string]any{"oid": v}}
			// Both tags moved ahead; the child's history can't be checked.
			head := liveRoot
			if nwo == "actions/checkout" {
				head = parityTestMoved
			}
			repo["tag"], repo["tagRef"] = map[string]any{"oid": head}, map[string]any{"oid": head}
			data["a"+i] = repo
		}
		return httpmock.JSONResponse(map[string]any{"data": data})(req)
	})
	reg.RegisterReusable(httpmock.GraphQLForRepo("octo", "root"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": testRepoResponse("octo/root", liveRoot, nodeActionYAML)},
	}))
	reg.RegisterReusable(httpmock.REST("GET", `repos/octo/root/compare/`),
		httpmock.JSONResponse(map[string]any{"status": "ahead", "merge_base_commit": map[string]any{"sha": rootSHA}}))
	rt := reposDown{reg, []string{"actions/checkout"}}
	useDefaultTransport(t, rt)
	path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: octo/root@v1
`)
	lock := "version: '" + parserlock.Version + "'\ndependencies:\n" +
		"  'octo/root@v1':\n    ref: 'v1'\n    commit: 'sha1-" + rootSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"    uses:\n      - 'actions/checkout@v4'\n" +
		"  'actions/checkout@v4':\n    ref: 'v4'\n    commit: 'sha1-" + parityTestSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"workflows:\n  '.github/workflows/workflow.yml':\n    - 'octo/root@v1'\n"
	lockPath := filepath.Join(".github", "workflows", "actions.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(lock), 0o600))

	_, stderr, err := runCommandWithHTTP(t, rt, "--relock", "--no-narrow", "--no-interactive", path)
	require.NoError(t, err, stderr)
	after, rerr := os.ReadFile(lockPath)
	require.NoError(t, rerr)
	assert.Contains(t, string(after), "commit: 'sha1-"+liveRoot+"'")
	assert.NotContains(t, string(after), "actions/checkout", "the dropped child's entry goes")
}

// TestParity_RelockRepinsMovedChild: the composite is unchanged, but its
// child's tag moved and the old commit's history can't be checked. --relock
// rebuilds the composite's closure, so the child is written at the commit
// its tag points at now, and the old, unchecked commit is not.
func TestParity_RelockRepinsMovedChild(t *testing.T) {
	const rootSHA = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	root := "name: r\nruns:\n  using: composite\n  steps:\n    - uses: actions/checkout@v4\n"
	reg := &httpmock.Registry{}
	reg.RegisterReusable(httpmock.GraphQL(`commit: object\(oid`), func(req *http.Request) (*http.Response, error) {
		var body struct {
			Variables map[string]any `json:"variables"`
		}
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		data := map[string]any{}
		for k, v := range body.Variables {
			i, ok := strings.CutPrefix(k, "oid")
			if !ok {
				continue
			}
			nwo := body.Variables["owner"+i].(string) + "/" + body.Variables["name"+i].(string)
			head := v
			if nwo == "actions/checkout" {
				head = parityTestMoved
			}
			data["a"+i] = map[string]any{
				"nameWithOwner": nwo,
				"databaseId":    1,
				"commit":        map[string]any{"oid": v},
				"tag":           map[string]any{"oid": head},
				"tagRef":        map[string]any{"oid": head},
			}
		}
		return httpmock.JSONResponse(map[string]any{"data": data})(req)
	})
	reg.RegisterReusable(httpmock.GraphQLForRepo("octo", "root"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": testRepoResponse("octo/root", rootSHA, root)},
	}))
	reg.RegisterReusable(httpmock.GraphQLForRepo("actions", "checkout"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": testRepoResponse("actions/checkout", parityTestMoved, nodeActionYAML)},
	}))
	// Only the old commit's history check fails.
	reg.RegisterReusable(httpmock.REST("GET", `repos/actions/checkout/compare/`), httpmock.StatusResponse(http.StatusServiceUnavailable))
	useDefaultTransport(t, reg)
	path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: octo/root@v1
`)
	lock := "version: '" + parserlock.Version + "'\ndependencies:\n" +
		"  'octo/root@v1':\n    ref: 'v1'\n    commit: 'sha1-" + rootSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"    uses:\n      - 'actions/checkout@v4'\n" +
		"  'actions/checkout@v4':\n    ref: 'v4'\n    commit: 'sha1-" + parityTestSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"workflows:\n  '.github/workflows/workflow.yml':\n    - 'octo/root@v1'\n"
	lockPath := filepath.Join(".github", "workflows", "actions.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(lock), 0o600))

	_, stderr, err := runCommandWithHTTP(t, reg, "--relock", "--no-narrow", "--no-interactive", path)
	require.NoError(t, err, stderr)
	after, rerr := os.ReadFile(lockPath)
	require.NoError(t, rerr)
	assert.Contains(t, string(after), "commit: 'sha1-"+parityTestMoved+"'")
	assert.NotContains(t, string(after), parityTestSHA)
}

// TestParity_RelockMovedTag: actions/checkout@v4 is used directly, alone
// or also through an unchanged composite, and its tag moved. --relock
// rebuilds the closure whether or not the history comparison classifies the
// move, so the new commit is written and verifies, and the old commit,
// whose check may not have finished, is not.
func TestParity_RelockMovedTag(t *testing.T) {
	for _, shared := range []bool{false, true} {
		for _, compareOK := range []bool{false, true} {
			for _, narrow := range []bool{false, true} {
				name := fmt.Sprintf("shared=%v compare=%v narrow=%v", shared, compareOK, narrow)
				t.Run(name, func(t *testing.T) { testRelockMovedTag(t, shared, compareOK, narrow) })
			}
		}
	}
}

func testRelockMovedTag(t *testing.T, shared, compareOK, narrow bool) {
	const rootSHA = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	root := "name: r\nruns:\n  using: composite\n  steps:\n    - uses: actions/checkout@v4\n"
	reg := &httpmock.Registry{}
	reg.RegisterReusable(httpmock.GraphQL(`commit: object\(oid`), func(req *http.Request) (*http.Response, error) {
		var body struct {
			Variables map[string]any `json:"variables"`
		}
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		data := map[string]any{}
		for k, v := range body.Variables {
			i, ok := strings.CutPrefix(k, "oid")
			if !ok {
				continue
			}
			nwo := body.Variables["owner"+i].(string) + "/" + body.Variables["name"+i].(string)
			head := v
			if nwo == "actions/checkout" {
				head = parityTestMoved
			}
			data["a"+i] = map[string]any{
				"nameWithOwner": nwo,
				"databaseId":    1,
				"commit":        map[string]any{"oid": v},
				"tag":           map[string]any{"oid": head},
				"tagRef":        map[string]any{"oid": head},
			}
		}
		return httpmock.JSONResponse(map[string]any{"data": data})(req)
	})
	repos := map[string]map[string]any{
		"octo/root":        testRepoResponse("octo/root", rootSHA, root),
		"actions/checkout": testRepoResponse("actions/checkout", parityTestMoved, nodeActionYAML),
	}
	reg.RegisterReusable(httpmock.GraphQL(`.`), func(req *http.Request) (*http.Response, error) {
		var body struct {
			Variables map[string]any `json:"variables"`
		}
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		data := map[string]any{}
		for k, v := range body.Variables {
			if i, ok := strings.CutPrefix(k, "owner"); ok {
				data["a"+i] = repos[v.(string)+"/"+body.Variables["name"+i].(string)]
			}
		}
		return httpmock.JSONResponse(map[string]any{"data": data})(req)
	})
	compare := httpmock.StatusResponse(http.StatusServiceUnavailable)
	if compareOK {
		compare = httpmock.JSONResponse(map[string]any{"status": "ahead", "merge_base_commit": map[string]any{"sha": parityTestSHA}})
	}
	reg.RegisterReusable(httpmock.REST("GET", `repos/actions/checkout/compare/`), compare)
	reg.RegisterReusable(httpmock.REST("GET", `repos/actions/checkout/tags`), httpmock.JSONResponse(httpmock.TagListResponse("v4.2.1", parityTestMoved, "v4", parityTestMoved)))
	reg.RegisterReusable(httpmock.REST("GET", `repos/octo/root/tags`), httpmock.JSONResponse(httpmock.TagListResponse("v1.0.0", rootSHA, "v1", rootSHA)))
	useDefaultTransport(t, reg)
	steps, entries, roots := "      - uses: actions/checkout@v4\n", "", "    - 'actions/checkout@v4'\n"
	if shared {
		steps = "      - uses: octo/root@v1\n" + steps
		entries = "  'octo/root@v1':\n    ref: 'v1'\n    commit: 'sha1-" + rootSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
			"    uses:\n      - 'actions/checkout@v4'\n"
		roots = "    - 'octo/root@v1'\n" + roots
	}
	path := writeTempWorkflow(t, "name: ci\non: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n"+steps)
	lock := "version: '" + parserlock.Version + "'\ndependencies:\n" + entries +
		"  'actions/checkout@v4':\n    ref: 'v4'\n    commit: 'sha1-" + parityTestSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"workflows:\n  '.github/workflows/workflow.yml':\n" + roots
	lockPath := filepath.Join(".github", "workflows", "actions.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(lock), 0o600))

	args := []string{"--relock", "--no-interactive", path}
	if !narrow {
		args = append([]string{"--no-narrow"}, args...)
	}
	_, stderr, err := runCommandWithHTTP(t, reg, args...)
	after, rerr := os.ReadFile(lockPath)
	require.NoError(t, rerr)
	require.NoError(t, err, "%s\n%s", stderr, after)
	assert.Contains(t, string(after), "commit: 'sha1-"+parityTestMoved+"'", "the new commit is written")
	assert.NotContains(t, string(after), parityTestSHA, "the old commit is not")
	_, stderr, err = runCommandWithHTTP(t, reg, "--verify", path)
	require.NoError(t, err, "the relocked lock verifies: %s", stderr)
}

// TestParity_RelockRepairsExactTagBehind: --relock and --accept-moved
// excuse an exact tag locked behind its commit, re-resolve it to the tag's
// commit, and the result verifies.
func TestParity_RelockRepairsExactTagBehind(t *testing.T) {
	for _, flag := range []string{"--relock", "--accept-moved"} {
		t.Run(flag, func(t *testing.T) {
			reg := exactTagBehindRegistry(t, "")
			path := writeTempWorkflow(t, "name: ci\non: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@v7.0.1\n")
			lock := "version: '" + parserlock.Version + "'\ndependencies:\n" +
				"  'actions/checkout@v7.0.1':\n    ref: 'v7.0.1'\n    commit: 'sha1-" + parityTestSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
				"workflows:\n  '.github/workflows/workflow.yml':\n    - 'actions/checkout@v7.0.1'\n"
			lockPath := filepath.Join(".github", "workflows", "actions.lock")
			require.NoError(t, os.WriteFile(lockPath, []byte(lock), 0o600))

			_, stderr, err := runCommandWithHTTP(t, reg, flag, "--no-interactive", path)
			after, rerr := os.ReadFile(lockPath)
			require.NoError(t, rerr)
			require.NoError(t, err, "%s\n%s", stderr, after)
			assert.Contains(t, string(after), "commit: 'sha1-"+parityTestMoved+"'", "the tag's commit is written")
			assert.NotContains(t, string(after), parityTestSHA, "the older commit is not")
			p, err := runParity(t, reg, "--verify", path)
			require.NoError(t, err, "the relocked lock verifies")
			assert.True(t, p.Valid, "%+v", p.Findings)
		})
	}
}

// TestParity_ExactTagBehindInCompositeRelocks: a composite's exact-tag
// entry locked behind its tag blocks. --relock rebuilds the composite's
// closure from its manifest, so it locks the tag's commit, and the result
// verifies.
func TestParity_ExactTagBehindInCompositeRelocks(t *testing.T) {
	const rootSHA = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	reg := exactTagBehindRegistry(t, rootSHA)
	path := writeTempWorkflow(t, "name: ci\non: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: octo/root@v1\n")
	lock := "version: '" + parserlock.Version + "'\ndependencies:\n" +
		"  'octo/root@v1':\n    ref: 'v1'\n    commit: 'sha1-" + rootSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"    uses:\n      - 'actions/checkout@v7.0.1'\n" +
		"  'actions/checkout@v7.0.1':\n    ref: 'v7.0.1'\n    commit: 'sha1-" + parityTestSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"workflows:\n  '.github/workflows/workflow.yml':\n    - 'octo/root@v1'\n"
	lockPath := filepath.Join(".github", "workflows", "actions.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(lock), 0o600))

	p, err := runParity(t, reg, "--verify", path)
	require.ErrorIs(t, err, errSilent)
	require.Len(t, p.Findings, 1, "%+v", p.Findings)
	assert.Equal(t, "tag-commit-mismatch", p.Findings[0].Category)
	assert.Equal(t, "run `gh actions-lock --relock` to lock the tag's commit", p.Findings[0].Remediation)

	_, stderr, err := runCommandWithHTTP(t, reg, "--relock", "--no-interactive", path)
	require.NoError(t, err, stderr)
	after, rerr := os.ReadFile(lockPath)
	require.NoError(t, rerr)
	assert.Contains(t, string(after), "'actions/checkout@v7.0.1':\n        ref: 'v7.0.1'\n        commit: 'sha1-"+parityTestMoved+"'", "the tag's commit is written")
	assert.NotContains(t, string(after), parityTestSHA, "the older commit is not")
	assert.Contains(t, string(after), "sha1-"+rootSHA, "the composite keeps its commit")

	p, err = runParity(t, reg, "--verify", path)
	require.NoError(t, err, "the relocked lock verifies")
	assert.True(t, p.Valid, "%+v", p.Findings)
}

// TestParity_ExactTagBehindSeededCompositeChildBlocks: a new composite
// whose child is the workflow's recorded exact tag resolves that child from
// the lockfile, at the older commit. That fresh pin must not hide the
// mismatch: the run blocks and writes nothing.
func TestParity_ExactTagBehindSeededCompositeChildBlocks(t *testing.T) {
	const rootSHA = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	reg := exactTagBehindRegistry(t, rootSHA)
	path := writeTempWorkflow(t, "name: ci\non: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@v7.0.1\n      - uses: octo/root@v1\n")
	lock := "version: '" + parserlock.Version + "'\ndependencies:\n" +
		"  'actions/checkout@v7.0.1':\n    ref: 'v7.0.1'\n    commit: 'sha1-" + parityTestSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"workflows:\n  '.github/workflows/workflow.yml':\n    - 'actions/checkout@v7.0.1'\n"
	lockPath := filepath.Join(".github", "workflows", "actions.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(lock), 0o600))

	p, err := runParity(t, reg, "--no-narrow", "--no-interactive", path)
	require.ErrorIs(t, err, errSilent)
	assert.False(t, p.Valid)
	var cats []string
	for _, f := range p.Findings {
		cats = append(cats, f.Category)
	}
	assert.Contains(t, cats, "tag-commit-mismatch")
	after, rerr := os.ReadFile(lockPath)
	require.NoError(t, rerr)
	assert.Equal(t, lock, string(after), "nothing is written")
}

// exactTagBehindRegistry answers parity with actions/checkout@v7.0.1 at
// parityTestMoved, a descendant of the locked parityTestSHA, and resolves
// octo/root (a composite using actions/checkout@v7.0.1) at rootSHA.
func exactTagBehindRegistry(t *testing.T, rootSHA string) *httpmock.Registry {
	t.Helper()
	reg := &httpmock.Registry{}
	reg.RegisterReusable(httpmock.GraphQL(`commit: object\(oid`), func(req *http.Request) (*http.Response, error) {
		var body struct {
			Variables map[string]any `json:"variables"`
		}
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		data := map[string]any{}
		for k, v := range body.Variables {
			i, ok := strings.CutPrefix(k, "oid")
			if !ok {
				continue
			}
			nwo := body.Variables["owner"+i].(string) + "/" + body.Variables["name"+i].(string)
			head := v
			if nwo == "actions/checkout" {
				head = parityTestMoved
			}
			data["a"+i] = map[string]any{
				"nameWithOwner": nwo,
				"databaseId":    1,
				"commit":        map[string]any{"oid": v},
				"tag":           map[string]any{"oid": head},
				"tagRef":        map[string]any{"oid": head},
			}
		}
		return httpmock.JSONResponse(map[string]any{"data": data})(req)
	})
	repos := map[string]map[string]any{
		"actions/checkout": testRepoResponse("actions/checkout", parityTestMoved, nodeActionYAML),
	}
	if rootSHA != "" {
		repos["octo/root"] = testRepoResponse("octo/root", rootSHA, "name: r\nruns:\n  using: composite\n  steps:\n    - uses: actions/checkout@v7.0.1\n")
		reg.RegisterReusable(httpmock.REST("GET", `repos/octo/root/tags`), httpmock.JSONResponse(httpmock.TagListResponse("v1.0.0", rootSHA, "v1", rootSHA)))
	}
	reg.RegisterReusable(httpmock.GraphQL(`.`), func(req *http.Request) (*http.Response, error) {
		var body struct {
			Variables map[string]any `json:"variables"`
		}
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		data := map[string]any{}
		for k, v := range body.Variables {
			if i, ok := strings.CutPrefix(k, "owner"); ok {
				data["a"+i] = repos[v.(string)+"/"+body.Variables["name"+i].(string)]
			}
		}
		return httpmock.JSONResponse(map[string]any{"data": data})(req)
	})
	reg.RegisterReusable(httpmock.REST("GET", `repos/actions/checkout/compare/`),
		httpmock.JSONResponse(map[string]any{"status": "ahead", "merge_base_commit": map[string]any{"sha": parityTestSHA}}))
	reg.RegisterReusable(httpmock.REST("GET", `repos/actions/checkout/tags`), httpmock.JSONResponse(httpmock.TagListResponse("v7.0.1", parityTestMoved, "v7.0.0", parityTestSHA)))
	useDefaultTransport(t, reg)
	return reg
}

// TestParity_RelockUnreachableKeepsWorkflow: one pin's tag moved cleanly,
// so its entry is pruned and its findings excused; another pin is
// unreachable. Planning then leaves the whole workflow as recorded, old
// commit included, so the run must fail and write nothing.
func TestParity_RelockUnreachableKeepsWorkflow(t *testing.T) {
	const cacheSHA = "cccccccccccccccccccccccccccccccccccccccc"
	reg := &httpmock.Registry{}
	reg.RegisterReusable(httpmock.GraphQL(`commit: object\(oid`), func(req *http.Request) (*http.Response, error) {
		var body struct {
			Variables map[string]any `json:"variables"`
		}
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		data := map[string]any{}
		for k, v := range body.Variables {
			i, ok := strings.CutPrefix(k, "oid")
			if !ok {
				continue
			}
			nwo := body.Variables["owner"+i].(string) + "/" + body.Variables["name"+i].(string)
			repo := map[string]any{"nameWithOwner": nwo, "databaseId": 1, "commit": map[string]any{"oid": v}}
			// checkout's tag moved; cache's recorded ref is gone.
			if nwo == "actions/checkout" {
				repo["tag"], repo["tagRef"] = map[string]any{"oid": parityTestMoved}, map[string]any{"oid": parityTestMoved}
			}
			data["a"+i] = repo
		}
		return httpmock.JSONResponse(map[string]any{"data": data})(req)
	})
	repos := map[string]map[string]any{
		"actions/checkout": testRepoResponse("actions/checkout", parityTestMoved, nodeActionYAML),
		"actions/cache":    testRepoResponse("actions/cache", cacheSHA, nodeActionYAML),
	}
	reg.RegisterReusable(httpmock.GraphQL(`.`), func(req *http.Request) (*http.Response, error) {
		var body struct {
			Variables map[string]any `json:"variables"`
		}
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		data := map[string]any{}
		for k, v := range body.Variables {
			if i, ok := strings.CutPrefix(k, "owner"); ok {
				data["a"+i] = repos[v.(string)+"/"+body.Variables["name"+i].(string)]
			}
		}
		return httpmock.JSONResponse(map[string]any{"data": data})(req)
	})
	reg.RegisterReusable(httpmock.REST("GET", `repos/actions/checkout/compare/`),
		httpmock.JSONResponse(map[string]any{"status": "ahead", "merge_base_commit": map[string]any{"sha": parityTestSHA}}))
	useDefaultTransport(t, reg)
	path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/cache@`+cacheSHA+`
`)
	lock := "version: '" + parserlock.Version + "'\ndependencies:\n" +
		"  'actions/cache@" + cacheSHA + "':\n    ref: 'v3-gone'\n    commit: 'sha1-" + cacheSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"  'actions/checkout@v4':\n    ref: 'v4'\n    commit: 'sha1-" + parityTestSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"workflows:\n  '.github/workflows/workflow.yml':\n    - 'actions/checkout@v4'\n    - 'actions/cache@" + cacheSHA + "'\n"
	lockPath := filepath.Join(".github", "workflows", "actions.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(lock), 0o600))

	_, stderr, err := runCommandWithHTTP(t, reg, "--relock", "--no-narrow", "--no-interactive", path)
	require.ErrorIs(t, err, errSilent, stderr)
	assert.Contains(t, stderr, "ref v3-gone is not a tag or branch of actions/cache")
	after, rerr := os.ReadFile(lockPath)
	require.NoError(t, rerr)
	assert.Equal(t, lock, string(after), "nothing is written")
}

// TestParity_AcceptMovedUnresolvableBlocks: --accept-moved accepts a direct
// full-SHA pin whose recorded ref is gone, which makes planning re-resolve
// it. If that resolution fails, nothing replaces the entry: the run must
// fail and write nothing.
func TestParity_AcceptMovedUnresolvableBlocks(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.RegisterReusable(httpmock.GraphQL(`commit: object\(oid`), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": map[string]any{
			"nameWithOwner": "actions/checkout",
			"databaseId":    1,
			"commit":        map[string]any{"oid": parityTestSHA},
		}},
	}))
	rt := reposDown{reg, []string{"actions/checkout"}}
	useDefaultTransport(t, rt)
	path := writeFullSHAWorkflow(t, "v6.1.0")
	lockBefore := readTempLockfilePins(t)
	wfBefore, err := os.ReadFile(path)
	require.NoError(t, err)

	_, stderr, err := runCommandWithHTTP(t, rt, "--accept-moved", "--no-interactive", path)
	require.Error(t, err, stderr)
	assert.Equal(t, lockBefore, readTempLockfilePins(t), "nothing is written")
	wfAfter, werr := os.ReadFile(path)
	require.NoError(t, werr)
	assert.Equal(t, string(wfBefore), string(wfAfter))
}

// TestParity_UnreachableChildNamesComposite: a composite's mutable child
// whose recorded commit left its tag's history blocks a plain run and
// --relock, pointing at the composite, never at a flag that can't fix it.
// --accept-moved rebuilds the composite's closure, so it locks the child's
// tag at its current commit.
func TestParity_UnreachableChildNamesComposite(t *testing.T) {
	for name, args := range map[string][]string{
		"default":      nil,
		"relock":       {"--relock"},
		"accept-moved": {"--accept-moved"},
	} {
		t.Run(name, func(t *testing.T) { testUnreachableChild(t, args, false) })
	}
	// Onboarding another workflow onto the locked composite reuses its
	// recorded children, so they are checked too.
	t.Run("scoped onboarding", func(t *testing.T) { testUnreachableChild(t, nil, true) })
}

func testUnreachableChild(t *testing.T, args []string, onboard bool) {
	const rootSHA = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	root := "name: r\nruns:\n  using: composite\n  steps:\n    - uses: actions/checkout@v4\n"
	reg := &httpmock.Registry{}
	reg.RegisterReusable(httpmock.GraphQL(`commit: object\(oid`), func(req *http.Request) (*http.Response, error) {
		var body struct {
			Variables map[string]any `json:"variables"`
		}
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		data := map[string]any{}
		for k, v := range body.Variables {
			i, ok := strings.CutPrefix(k, "oid")
			if !ok {
				continue
			}
			nwo := body.Variables["owner"+i].(string) + "/" + body.Variables["name"+i].(string)
			head := v
			if nwo == "actions/checkout" {
				head = parityTestMoved
			}
			data["a"+i] = map[string]any{
				"nameWithOwner": nwo,
				"databaseId":    1,
				"commit":        map[string]any{"oid": v},
				"tag":           map[string]any{"oid": head},
				"tagRef":        map[string]any{"oid": head},
			}
		}
		return httpmock.JSONResponse(map[string]any{"data": data})(req)
	})
	repos := map[string]map[string]any{
		"octo/root":        testRepoResponse("octo/root", rootSHA, root),
		"actions/checkout": testRepoResponse("actions/checkout", parityTestMoved, nodeActionYAML),
	}
	reg.RegisterReusable(httpmock.GraphQL(`.`), func(req *http.Request) (*http.Response, error) {
		var body struct {
			Variables map[string]any `json:"variables"`
		}
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		data := map[string]any{}
		for k, v := range body.Variables {
			if i, ok := strings.CutPrefix(k, "owner"); ok {
				data["a"+i] = repos[v.(string)+"/"+body.Variables["name"+i].(string)]
			}
		}
		return httpmock.JSONResponse(map[string]any{"data": data})(req)
	})
	reg.RegisterReusable(httpmock.REST("GET", `repos/actions/checkout/compare/`),
		httpmock.JSONResponse(map[string]any{"status": "diverged", "merge_base_commit": map[string]any{"sha": "cccccccccccccccccccccccccccccccccccccccc"}}))
	useDefaultTransport(t, reg)
	path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: octo/root@v1
`)
	lock := "version: '" + parserlock.Version + "'\ndependencies:\n" +
		"  'octo/root@v1':\n    ref: 'v1'\n    commit: 'sha1-" + rootSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"    uses:\n      - 'actions/checkout@v4'\n" +
		"  'actions/checkout@v4':\n    ref: 'v4'\n    commit: 'sha1-" + parityTestSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"workflows:\n  '.github/workflows/workflow.yml':\n    - 'octo/root@v1'\n"
	lockPath := filepath.Join(".github", "workflows", "actions.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(lock), 0o600))
	if onboard {
		wf, err := os.ReadFile(path)
		require.NoError(t, err)
		path = filepath.Join(".github", "workflows", "other.yml")
		require.NoError(t, os.WriteFile(path, wf, 0o600))
	}

	p, err := runParity(t, reg, append(args, "--no-narrow", "--no-interactive", path)...)
	if slices.Contains(args, "--accept-moved") {
		require.NoError(t, err, "%+v", p.Findings)
		after, rerr := os.ReadFile(lockPath)
		require.NoError(t, rerr)
		assert.Contains(t, string(after), "commit: 'sha1-"+parityTestMoved+"'")
		assert.NotContains(t, string(after), parityTestSHA)
		return
	}
	require.ErrorIs(t, err, errSilent, "%+v", p.Findings)
	var child *format.Finding
	for i, f := range p.Findings {
		if f.Category == "unreachable-pin" && f.Severity == "error" {
			child = &p.Findings[i]
		}
	}
	require.NotNil(t, child, "%+v", p.Findings)
	assert.Contains(t, child.Detail, "used by octo/root@v1")
	assert.Equal(t, "upgrade or replace octo/root@v1 with a version whose pin of actions/checkout can be verified, or stop using it", child.Remediation)
	after, rerr := os.ReadFile(lockPath)
	require.NoError(t, rerr)
	assert.Equal(t, lock, string(after), "nothing is written")
}

// TestParity_OnboardingSiblingSubActionSkipsReplacedChildren: onboarding
// org/actions/b@v1 while an unselected workflow locks org/actions/a@v1 at
// the same commit reads b's manifest, so the onboarded workflow doesn't
// keep a's recorded children. a's unavailable child doesn't block it.
func TestParity_OnboardingSiblingSubActionSkipsUnusedChildren(t *testing.T) {
	const rootSHA = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	reg := &httpmock.Registry{}
	reg.RegisterReusable(httpmock.GraphQL(`commit: object\(oid`), func(req *http.Request) (*http.Response, error) {
		var body struct {
			Variables map[string]any `json:"variables"`
		}
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		data := map[string]any{}
		for k, v := range body.Variables {
			i, ok := strings.CutPrefix(k, "oid")
			if !ok {
				continue
			}
			nwo := body.Variables["owner"+i].(string) + "/" + body.Variables["name"+i].(string)
			if nwo == "octo/gone" {
				data["a"+i] = nil
				continue
			}
			data["a"+i] = map[string]any{
				"nameWithOwner": nwo,
				"databaseId":    1,
				"commit":        map[string]any{"oid": v},
				"tag":           map[string]any{"oid": v},
				"tagRef":        map[string]any{"oid": v},
			}
		}
		return httpmock.JSONResponse(map[string]any{"data": data})(req)
	})
	reg.RegisterReusable(httpmock.GraphQLForRepo("org", "actions"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": testRepoResponse("org/actions", rootSHA, nodeActionYAML)},
	}))
	useDefaultTransport(t, reg)
	writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: org/actions/a@v1
`)
	lock := "version: '" + parserlock.Version + "'\ndependencies:\n" +
		"  'org/actions@v1':\n    ref: 'v1'\n    commit: 'sha1-" + rootSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"    uses:\n      - 'octo/gone@v1'\n" +
		"  'octo/gone@v1':\n    ref: 'v1'\n    commit: 'sha1-" + parityTestSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"workflows:\n  '.github/workflows/workflow.yml':\n    - 'org/actions@v1'\n"
	lockPath := filepath.Join(".github", "workflows", "actions.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(lock), 0o600))
	path := filepath.Join(".github", "workflows", "other.yml")
	require.NoError(t, os.WriteFile(path, []byte("name: other\non: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: org/actions/b@v1\n"), 0o600))

	p, err := runParity(t, reg, "--no-narrow", "--no-interactive", path)
	require.NoError(t, err, "%+v", p.Findings)
	for _, f := range p.Findings {
		assert.NotContains(t, f.Detail, "octo/gone", "a's replaced child is not checked")
	}
	after, rerr := os.ReadFile(lockPath)
	require.NoError(t, rerr)
	assert.Contains(t, string(after), "'.github/workflows/other.yml'")
	assert.Contains(t, string(after), "'.github/workflows/workflow.yml':\n        - 'org/actions@v1'", "the unselected workflow keeps its pins")
}

// TestParity_SharedMutablePinAcceptsMoved: actions/checkout@v4 is used
// directly and through an unchanged composite, and its recorded commit left
// v4's history. Both uses share the one v4 entry, which --accept-moved
// re-resolves in place, so the advice is --accept-moved, and following it
// writes a lock that verifies.
func TestParity_SharedMutablePinAcceptsMoved(t *testing.T) {
	const rootSHA = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	root := "name: r\nruns:\n  using: composite\n  steps:\n    - uses: actions/checkout@v4\n"
	reg := &httpmock.Registry{}
	reg.RegisterReusable(httpmock.GraphQL(`commit: object\(oid`), func(req *http.Request) (*http.Response, error) {
		var body struct {
			Variables map[string]any `json:"variables"`
		}
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		data := map[string]any{}
		for k, v := range body.Variables {
			i, ok := strings.CutPrefix(k, "oid")
			if !ok {
				continue
			}
			nwo := body.Variables["owner"+i].(string) + "/" + body.Variables["name"+i].(string)
			head := v
			if nwo == "actions/checkout" {
				head = parityTestMoved
			}
			data["a"+i] = map[string]any{
				"nameWithOwner": nwo,
				"databaseId":    1,
				"commit":        map[string]any{"oid": v},
				"tag":           map[string]any{"oid": head},
				"tagRef":        map[string]any{"oid": head},
			}
		}
		return httpmock.JSONResponse(map[string]any{"data": data})(req)
	})
	repos := map[string]map[string]any{
		"octo/root":        testRepoResponse("octo/root", rootSHA, root),
		"actions/checkout": testRepoResponse("actions/checkout", parityTestMoved, nodeActionYAML),
	}
	reg.RegisterReusable(httpmock.GraphQL(`.`), func(req *http.Request) (*http.Response, error) {
		var body struct {
			Variables map[string]any `json:"variables"`
		}
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		data := map[string]any{}
		for k, v := range body.Variables {
			if i, ok := strings.CutPrefix(k, "owner"); ok {
				data["a"+i] = repos[v.(string)+"/"+body.Variables["name"+i].(string)]
			}
		}
		return httpmock.JSONResponse(map[string]any{"data": data})(req)
	})
	reg.RegisterReusable(httpmock.REST("GET", `repos/actions/checkout/compare/`+parityTestSHA),
		httpmock.JSONResponse(map[string]any{"status": "diverged", "merge_base_commit": map[string]any{"sha": "cccccccccccccccccccccccccccccccccccccccc"}}))
	reg.RegisterReusable(httpmock.REST("GET", `repos/actions/checkout/tags`), httpmock.JSONResponse(httpmock.TagListResponse("v4.2.1", parityTestMoved, "v4", parityTestMoved)))
	useDefaultTransport(t, reg)
	path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: octo/root@v1
      - uses: actions/checkout@v4
`)
	lock := "version: '" + parserlock.Version + "'\ndependencies:\n" +
		"  'octo/root@v1':\n    ref: 'v1'\n    commit: 'sha1-" + rootSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"    uses:\n      - 'actions/checkout@v4'\n" +
		"  'actions/checkout@v4':\n    ref: 'v4'\n    commit: 'sha1-" + parityTestSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"workflows:\n  '.github/workflows/workflow.yml':\n    - 'octo/root@v1'\n    - 'actions/checkout@v4'\n"
	lockPath := filepath.Join(".github", "workflows", "actions.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(lock), 0o600))

	p, err := runParity(t, reg, "--no-interactive", path)
	require.ErrorIs(t, err, errSilent)
	var advice []string
	for _, f := range p.Findings {
		if f.Category == "unreachable-pin" {
			advice = append(advice, f.Remediation)
		}
	}
	assert.Equal(t, []string{"verify it, then run `gh actions-lock --accept-moved`"}, advice)

	_, stderr, err := runCommandWithHTTP(t, reg, "--accept-moved", "--no-interactive", path)
	after, rerr := os.ReadFile(lockPath)
	require.NoError(t, rerr)
	require.NoError(t, err, "%s\n%s", stderr, after)
	assert.Contains(t, string(after), "commit: 'sha1-"+parityTestMoved+"'")
	assert.NotContains(t, string(after), parityTestSHA)
	_, stderr, err = runCommandWithHTTP(t, reg, "--verify", path)
	require.NoError(t, err, "the repaired lock verifies: %s", stderr)
}

// TestParity_StaleSharedChild: checkout@v4 was a direct pin and also a
// composite's child. The workflow dropped the direct use, so its stale
// finding prunes the direct entry, but the composite still uses v4, so the
// entry stays. A rebuild re-resolves v4 under the composite: if v4 can't be
// resolved, or --relock meets a commit gone from its history, nothing is
// written; --accept-moved locks v4 at its current commit.
func TestParity_StaleSharedChild(t *testing.T) {
	for _, tc := range []struct {
		name     string
		diverged bool
		args     []string
		want     string
	}{
		{"relock, history unknown", false, []string{"--relock"}, "could not be resolved"},
		{"relock, commit gone", true, []string{"--relock"}, "is not in actions/checkout's history"},
		{"accept-moved, history unknown", false, []string{"--accept-moved"}, "could not be resolved"},
		{"accept-moved, commit gone", true, []string{"--accept-moved"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) { testStaleSharedChild(t, tc.diverged, tc.args, tc.want) })
	}
}

func testStaleSharedChild(t *testing.T, diverged bool, args []string, want string) {
	const rootSHA = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	const liveRoot = "ffffffffffffffffffffffffffffffffffffffff"
	reg := &httpmock.Registry{}
	reg.RegisterReusable(httpmock.GraphQL(`commit: object\(oid`), func(req *http.Request) (*http.Response, error) {
		var body struct {
			Variables map[string]any `json:"variables"`
		}
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		data := map[string]any{}
		for k, v := range body.Variables {
			i, ok := strings.CutPrefix(k, "oid")
			if !ok {
				continue
			}
			nwo := body.Variables["owner"+i].(string) + "/" + body.Variables["name"+i].(string)
			head := liveRoot
			if nwo == "actions/checkout" {
				head = parityTestMoved
			}
			data["a"+i] = map[string]any{"nameWithOwner": nwo, "databaseId": 1, "commit": map[string]any{"oid": v},
				"tag": map[string]any{"oid": head}, "tagRef": map[string]any{"oid": head}}
		}
		return httpmock.JSONResponse(map[string]any{"data": data})(req)
	})
	root := "name: r\nruns:\n  using: composite\n  steps:\n    - uses: actions/checkout@v4\n"
	reg.RegisterReusable(httpmock.GraphQLForRepo("octo", "root"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": testRepoResponse("octo/root", liveRoot, root)},
	}))
	reg.RegisterReusable(httpmock.REST("GET", `repos/octo/root/compare/`),
		httpmock.JSONResponse(map[string]any{"status": "ahead", "merge_base_commit": map[string]any{"sha": rootSHA}}))
	var rt http.RoundTripper = reposDown{reg, []string{"actions/checkout"}}
	if diverged {
		reg.RegisterReusable(httpmock.REST("GET", `repos/actions/checkout/compare/`),
			httpmock.JSONResponse(map[string]any{"status": "diverged", "merge_base_commit": map[string]any{"sha": "cccccccccccccccccccccccccccccccccccccccc"}}))
		reg.RegisterReusable(httpmock.GraphQLForRepo("actions", "checkout"), httpmock.JSONResponse(map[string]any{
			"data": map[string]any{"a0": testRepoResponse("actions/checkout", parityTestMoved, nodeActionYAML)},
		}))
		rt = reg
	}
	useDefaultTransport(t, rt)
	path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: octo/root@v1
`)
	lock := "version: '" + parserlock.Version + "'\ndependencies:\n" +
		"  'octo/root@v1':\n    ref: 'v1'\n    commit: 'sha1-" + rootSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"    uses:\n      - 'actions/checkout@v4'\n" +
		"  'actions/checkout@v4':\n    ref: 'v4'\n    commit: 'sha1-" + parityTestSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"workflows:\n  '.github/workflows/workflow.yml':\n    - 'octo/root@v1'\n    - 'actions/checkout@v4'\n"
	lockPath := filepath.Join(".github", "workflows", "actions.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(lock), 0o600))

	_, stderr, err := runCommandWithHTTP(t, rt, append(args, "--no-narrow", "--no-interactive", path)...)
	after, rerr := os.ReadFile(lockPath)
	require.NoError(t, rerr)
	if want == "" {
		require.NoError(t, err, "%s\n%s", stderr, after)
		assert.Contains(t, string(after), "commit: 'sha1-"+parityTestMoved+"'")
		assert.NotContains(t, string(after), parityTestSHA)
		return
	}
	require.ErrorIs(t, err, errSilent, "%s\n%s", stderr, after)
	assert.Contains(t, stderr, want)
	assert.Equal(t, lock, string(after), "nothing is written")
}

// TestParity_RelockRepinsDifferentlyCasedChild: the workflow uses
// actions/checkout@Release and a composite uses actions/checkout@release,
// both locked at one commit. Release moved cleanly; release's history check
// is inconclusive. Refs are case-sensitive, so each is re-resolved under its
// own key, and neither old commit is written.
func TestParity_RelockRepinsDifferentlyCasedChild(t *testing.T) {
	const (
		rootSHA     = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
		releaseHead = "cccccccccccccccccccccccccccccccccccccccc"
	)
	root := "name: r\nruns:\n  using: composite\n  steps:\n    - uses: actions/checkout@release\n"
	heads := map[string]string{"Release": parityTestMoved, "release": releaseHead}
	reg := &httpmock.Registry{}
	reg.RegisterReusable(httpmock.GraphQL(`commit: object\(oid`), func(req *http.Request) (*http.Response, error) {
		var body struct {
			Variables map[string]any `json:"variables"`
		}
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		data := map[string]any{}
		for k, v := range body.Variables {
			i, ok := strings.CutPrefix(k, "oid")
			if !ok {
				continue
			}
			nwo := body.Variables["owner"+i].(string) + "/" + body.Variables["name"+i].(string)
			head := v
			if ref, _ := body.Variables["tagRef"+i].(string); nwo == "actions/checkout" {
				head = heads[strings.TrimPrefix(ref, "refs/tags/")]
			}
			data["a"+i] = map[string]any{"nameWithOwner": nwo, "databaseId": 1, "commit": map[string]any{"oid": v},
				"tag": map[string]any{"oid": head}, "tagRef": map[string]any{"oid": head}}
		}
		return httpmock.JSONResponse(map[string]any{"data": data})(req)
	})
	reg.RegisterReusable(httpmock.GraphQL(`.`), func(req *http.Request) (*http.Response, error) {
		var body struct {
			Variables map[string]any `json:"variables"`
		}
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		data := map[string]any{}
		for k, v := range body.Variables {
			i, ok := strings.CutPrefix(k, "owner")
			if !ok {
				continue
			}
			nwo := v.(string) + "/" + body.Variables["name"+i].(string)
			expr, _ := body.Variables["expr"+i].(string)
			switch nwo {
			case "octo/root":
				data["a"+i] = testRepoResponse(nwo, rootSHA, root)
			default:
				data["a"+i] = testRepoResponse(nwo, heads[strings.TrimSuffix(expr, "^{commit}")], nodeActionYAML)
			}
		}
		return httpmock.JSONResponse(map[string]any{"data": data})(req)
	})
	reg.RegisterReusable(httpmock.REST("GET", `repos/actions/checkout/compare/`+parityTestSHA+`\.\.\.`+parityTestMoved),
		httpmock.JSONResponse(map[string]any{"status": "ahead", "merge_base_commit": map[string]any{"sha": parityTestSHA}}))
	reg.RegisterReusable(httpmock.REST("GET", `repos/actions/checkout/compare/`+parityTestSHA+`\.\.\.`+releaseHead),
		httpmock.StatusResponse(http.StatusServiceUnavailable))
	for ref, head := range heads {
		reg.RegisterReusable(httpmock.REST("GET", `repos/actions/checkout/git/ref/tags/`+ref+`$`),
			httpmock.JSONResponse(httpmock.GitRefResponse("refs/tags/"+ref, head)))
	}
	useDefaultTransport(t, reg)
	path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: octo/root@v1
      - uses: actions/checkout@Release
`)
	lock := "version: '" + parserlock.Version + "'\ndependencies:\n" +
		"  'actions/checkout@Release':\n    ref: 'Release'\n    commit: 'sha1-" + parityTestSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"  'actions/checkout@release':\n    ref: 'release'\n    commit: 'sha1-" + parityTestSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"  'octo/root@v1':\n    ref: 'v1'\n    commit: 'sha1-" + rootSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"    uses:\n      - 'actions/checkout@release'\n" +
		"workflows:\n  '.github/workflows/workflow.yml':\n    - 'actions/checkout@Release'\n    - 'octo/root@v1'\n"
	lockPath := filepath.Join(".github", "workflows", "actions.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(lock), 0o600))

	_, stderr, err := runCommandWithHTTP(t, reg, "--relock", "--no-narrow", "--no-interactive", path)
	after, rerr := os.ReadFile(lockPath)
	require.NoError(t, rerr)
	require.NoError(t, err, "%s\n%s", stderr, after)
	assert.Contains(t, string(after), "'actions/checkout@Release':\n        ref: 'Release'\n        commit: 'sha1-"+parityTestMoved+"'")
	assert.Contains(t, string(after), "'actions/checkout@release':\n        ref: 'release'\n        commit: 'sha1-"+releaseHead+"'")
	assert.NotContains(t, string(after), parityTestSHA)
}

// TestParity_AcceptMovedRepairsParityOnlyUnreachablePin: diagnosis
// couldn't classify actions/checkout@v4's move (its history check failed),
// but parity then found the recorded commit gone from v4's history. The pin
// is the workflow's alone, so --accept-moved re-resolves it in place: it
// must repair it, not block on the finding only parity raised.
func TestParity_AcceptMovedRepairsParityOnlyUnreachablePin(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.RegisterReusable(httpmock.GraphQL(`commit: object\(oid`), func(req *http.Request) (*http.Response, error) {
		var body struct {
			Variables map[string]any `json:"variables"`
		}
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		data := map[string]any{}
		for k, v := range body.Variables {
			if i, ok := strings.CutPrefix(k, "oid"); ok {
				data["a"+i] = map[string]any{"nameWithOwner": "actions/checkout", "databaseId": 1, "commit": map[string]any{"oid": v},
					"tag": map[string]any{"oid": parityTestMoved}, "tagRef": map[string]any{"oid": parityTestMoved}}
			}
		}
		return httpmock.JSONResponse(map[string]any{"data": data})(req)
	})
	reg.RegisterReusable(httpmock.GraphQL(`.`), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": testRepoResponse("actions/checkout", parityTestMoved, nodeActionYAML)},
	}))
	var compares atomic.Int32
	diverged := httpmock.JSONResponse(map[string]any{"status": "diverged", "merge_base_commit": map[string]any{"sha": "cccccccccccccccccccccccccccccccccccccccc"}})
	reg.RegisterReusable(httpmock.REST("GET", `repos/actions/checkout/compare/`), func(req *http.Request) (*http.Response, error) {
		// Diagnosis asks first and gets no answer; parity then does.
		if compares.Add(1) <= 1 {
			return httpmock.StatusResponse(http.StatusServiceUnavailable)(req)
		}
		return diverged(req)
	})
	useDefaultTransport(t, reg)
	path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
`, "actions/checkout@v4=sha1-"+parityTestSHA)

	stdout, stderr, err := runCommandWithHTTP(t, reg, "--accept-moved", "--no-narrow", "--no-interactive", "--json=valid,findings", path)
	require.Equal(t, int32(2), compares.Load(), "diagnosis, then parity")
	require.NoError(t, err, "%s\n%s", stdout, stderr)
	lock := readTempLockfilePins(t)
	assert.Contains(t, lock, "sha1-"+parityTestMoved)
	assert.NotContains(t, lock, parityTestSHA)
	_, stderr, err = runCommandWithHTTP(t, reg, "--verify", path)
	require.NoError(t, err, "the repaired lock verifies: %s", stderr)
}

// stubGraph answers parity and resolution for a set of repositories:
// heads maps an owner/repo to the commit its tags now point at (absent: the
// queried commit), manifests maps it to its action.yml.
func stubGraph(t *testing.T, reg *httpmock.Registry, heads, commits map[string]string, manifests map[string]string) {
	t.Helper()
	reg.RegisterReusable(httpmock.GraphQL(`commit: object\(oid`), func(req *http.Request) (*http.Response, error) {
		var body struct {
			Variables map[string]any `json:"variables"`
		}
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		data := map[string]any{}
		for k, v := range body.Variables {
			i, ok := strings.CutPrefix(k, "oid")
			if !ok {
				continue
			}
			nwo := body.Variables["owner"+i].(string) + "/" + body.Variables["name"+i].(string)
			head := v
			if h, ok := heads[nwo]; ok {
				head = h
			}
			data["a"+i] = map[string]any{"nameWithOwner": nwo, "databaseId": 1, "commit": map[string]any{"oid": v},
				"tag": map[string]any{"oid": head}, "tagRef": map[string]any{"oid": head}}
		}
		return httpmock.JSONResponse(map[string]any{"data": data})(req)
	})
	reg.RegisterReusable(httpmock.GraphQL(`.`), func(req *http.Request) (*http.Response, error) {
		var body struct {
			Variables map[string]any `json:"variables"`
		}
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		data := map[string]any{}
		for k, v := range body.Variables {
			if i, ok := strings.CutPrefix(k, "owner"); ok {
				nwo := v.(string) + "/" + body.Variables["name"+i].(string)
				manifest, ok := manifests[nwo]
				if !ok {
					manifest = nodeActionYAML
				}
				data["a"+i] = testRepoResponse(nwo, commits[nwo], manifest)
			}
		}
		return httpmock.JSONResponse(map[string]any{"data": data})(req)
	})
}

// TestParity_AcceptMovedRepairsChildWithNewDirectUse: a composite's child
// left its tag's history, and a workflow now uses the child directly too.
// --accept-moved resolves that direct use and writes the child's own key
// afresh, so the repair goes through, whether the direct use is in the
// composite's workflow or another one.
func TestParity_AcceptMovedRepairsChildWithNewDirectUse(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		cross, blocked, narrow bool
	}{
		{name: "same workflow"},
		{name: "same workflow, narrowing", narrow: true},
		{name: "another workflow", cross: true},
		// The repairing workflow can't be written, so neither can the repair.
		{name: "another workflow that is blocked", cross: true, blocked: true},
	} {
		t.Run(tc.name, func(t *testing.T) { testChildWithNewDirectUse(t, tc.cross, tc.blocked, tc.narrow) })
	}
}

func testChildWithNewDirectUse(t *testing.T, cross, blocked, narrow bool) {
	const rootSHA = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	reg := &httpmock.Registry{}
	stubGraph(t, reg,
		map[string]string{"actions/checkout": parityTestMoved},
		map[string]string{"octo/root": rootSHA, "actions/checkout": parityTestMoved},
		map[string]string{"octo/root": "name: r\nruns:\n  using: composite\n  steps:\n    - uses: actions/checkout@v4.2.1\n"})
	reg.RegisterReusable(httpmock.REST("GET", `repos/actions/checkout/compare/`),
		httpmock.JSONResponse(map[string]any{"status": "diverged", "merge_base_commit": map[string]any{"sha": "cccccccccccccccccccccccccccccccccccccccc"}}))
	useDefaultTransport(t, reg)
	steps := "      - uses: octo/root@v1\n"
	if !cross {
		steps += "      - uses: actions/checkout@v4.2.1\n"
	}
	path := writeTempWorkflow(t, "name: ci\non: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n"+steps)
	paths := []string{path}
	if cross {
		other := filepath.Join(".github", "workflows", "other.yml")
		otherSteps := "      - uses: actions/checkout@v4.2.1\n"
		if blocked {
			otherSteps += "      - uses: actions/cache@v9-missing\n"
		}
		require.NoError(t, os.WriteFile(other, []byte("name: other\non: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n"+otherSteps), 0o600))
		paths = append(paths, filepath.ToSlash(other))
	}
	lock := "version: '" + parserlock.Version + "'\ndependencies:\n" +
		"  'octo/root@v1':\n    ref: 'v1'\n    commit: 'sha1-" + rootSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"    uses:\n      - 'actions/checkout@v4.2.1'\n" +
		"  'actions/checkout@v4.2.1':\n    ref: 'v4.2.1'\n    commit: 'sha1-" + parityTestSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"workflows:\n  '.github/workflows/workflow.yml':\n    - 'octo/root@v1'\n"
	lockPath := filepath.Join(".github", "workflows", "actions.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(lock), 0o600))

	args := []string{"--accept-moved", "--no-interactive"}
	if !narrow {
		args = append(args, "--no-narrow")
	}
	_, stderr, err := runCommandWithHTTP(t, reg, append(args, paths...)...)
	after, rerr := os.ReadFile(lockPath)
	require.NoError(t, rerr)
	if blocked {
		require.ErrorIs(t, err, errSilent, "%s\n%s", stderr, after)
		assert.Equal(t, lock, string(after), "nothing is written")
		return
	}
	require.NoError(t, err, "%s\n%s", stderr, after)
	assert.Contains(t, string(after), "commit: 'sha1-"+parityTestMoved+"'")
	assert.NotContains(t, string(after), parityTestSHA)
	_, stderr, err = runCommandWithHTTP(t, reg, append([]string{"--verify"}, paths...)...)
	require.NoError(t, err, "the repaired lock verifies: %s", stderr)
}

// TestParity_AcceptMovedKeepsDeclaredChildBesideDirectUse: a composite's
// child actions/checkout@v4 left v4's history, and the workflow now uses
// checkout@v4 directly too. Narrowing leaves the shared use as the
// composite declares it, so with or without narrowing --accept-moved
// re-locks v4 under the composite's edge, and the lock verifies.
func TestParity_AcceptMovedKeepsDeclaredChildBesideDirectUse(t *testing.T) {
	for _, narrow := range []bool{true, false} {
		t.Run(fmt.Sprintf("narrow=%v", narrow), func(t *testing.T) {
			const rootSHA = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
			reg := &httpmock.Registry{}
			stubGraph(t, reg,
				map[string]string{"actions/checkout": parityTestMoved},
				map[string]string{"octo/root": rootSHA, "actions/checkout": parityTestMoved},
				map[string]string{"octo/root": "name: r\nruns:\n  using: composite\n  steps:\n    - uses: actions/checkout@v4\n"})
			reg.RegisterReusable(httpmock.REST("GET", `repos/actions/checkout/compare/`),
				httpmock.JSONResponse(map[string]any{"status": "diverged", "merge_base_commit": map[string]any{"sha": "cccccccccccccccccccccccccccccccccccccccc"}}))
			reg.RegisterReusable(httpmock.REST("GET", `repos/actions/checkout/tags`),
				httpmock.JSONResponse(httpmock.TagListResponse("v4.2.1", parityTestMoved, "v4", parityTestMoved)))
			reg.RegisterReusable(httpmock.REST("GET", `repos/octo/root/tags`),
				httpmock.JSONResponse(httpmock.TagListResponse("v1.0.0", rootSHA, "v1", rootSHA)))
			useDefaultTransport(t, reg)
			path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: octo/root@v1
      - uses: actions/checkout@v4
`)
			lock := "version: '" + parserlock.Version + "'\ndependencies:\n" +
				"  'actions/checkout@v4':\n    ref: 'v4'\n    commit: 'sha1-" + parityTestSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
				"  'octo/root@v1':\n    ref: 'v1'\n    commit: 'sha1-" + rootSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
				"    uses:\n      - 'actions/checkout@v4'\n" +
				"workflows:\n  '.github/workflows/workflow.yml':\n    - 'octo/root@v1'\n"
			lockPath := filepath.Join(".github", "workflows", "actions.lock")
			require.NoError(t, os.WriteFile(lockPath, []byte(lock), 0o600))

			args := []string{"--accept-moved", "--no-interactive", path}
			if !narrow {
				args = append([]string{"--no-narrow"}, args...)
			}
			_, stderr, err := runCommandWithHTTP(t, reg, args...)
			after, rerr := os.ReadFile(lockPath)
			require.NoError(t, rerr)
			require.NoError(t, err, "%s\n%s", stderr, after)
			assert.Contains(t, string(after), "'actions/checkout@v4':\n        ref: 'v4'\n        commit: 'sha1-"+parityTestMoved+"'")
			assert.Contains(t, string(after), "'octo/root@v1':\n        ref: 'v1'\n        commit: 'sha1-"+rootSHA+"'\n        owner_id: 1\n        repo_id: 1\n        uses:\n            - 'actions/checkout@v4'\n")
			assert.NotContains(t, string(after), "v4.2.1")
			assert.NotContains(t, string(after), parityTestSHA)
			wf, werr := os.ReadFile(path)
			require.NoError(t, werr)
			assert.Contains(t, string(wf), "uses: actions/checkout@v4\n")
			_, stderr, err = runCommandWithHTTP(t, reg, "--verify", path)
			require.NoError(t, err, "the repaired lock verifies: %s", stderr)
		})
	}
}

// TestParity_UnwrittenRunReportsDeferredFindings: --relock excused
// checkout@v4's failed history check on the strength of a write, but the
// run writes nothing: another workflow's pin can't be planned, or can't
// be resolved at all. The recorded entry stays as it was, so its finding
// must be reported.
func TestParity_UnwrittenRunReportsDeferredFindings(t *testing.T) {
	const orphan = "1111111111111111111111111111111111111111"
	for name, other := range map[string]string{
		// No tag or branch contains the commit: planning can't lock it.
		"planning fails": "actions/cache@" + orphan,
		// The repository is down: nothing resolves the pin.
		"resolution fails": "actions/cache@v9",
	} {
		t.Run(name, func(t *testing.T) {
			reg := &httpmock.Registry{}
			stubGraph(t, reg,
				map[string]string{"actions/checkout": parityTestMoved},
				map[string]string{"actions/checkout": parityTestMoved, "actions/cache": orphan},
				nil)
			reg.RegisterReusable(httpmock.REST("GET", `repos/actions/checkout/compare/`), httpmock.StatusResponse(http.StatusServiceUnavailable))
			reg.RegisterReusable(httpmock.REST("GET", `repos/actions/cache$`),
				httpmock.JSONResponse(map[string]any{"full_name": "actions/cache", "default_branch": "main", "id": 1, "owner": map[string]any{"id": 1}}))
			reg.RegisterReusable(httpmock.REST("GET", `repos/actions/cache/tags`), httpmock.JSONResponse(httpmock.TagListResponse()))
			reg.RegisterReusable(httpmock.REST("GET", `repos/actions/cache/releases`), httpmock.JSONResponse([]map[string]any{}))
			reg.RegisterReusable(httpmock.REST("GET", `repos/actions/cache/branches`), httpmock.JSONResponse(httpmock.BranchListResponse("main", parityTestMoved)))
			reg.RegisterReusable(httpmock.REST("GET", `repos/actions/cache/compare/`),
				httpmock.JSONResponse(map[string]any{"status": "diverged", "merge_base_commit": map[string]any{"sha": "cccccccccccccccccccccccccccccccccccccccc"}}))
			httpmock.NoTagsNamed(reg, "actions", "cache")
			var rt http.RoundTripper = reg
			if name == "resolution fails" {
				rt = reposDown{reg, []string{"actions/cache"}}
			}
			useDefaultTransport(t, rt)
			path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
`, "actions/checkout@v4=sha1-"+parityTestSHA)
			otherPath := filepath.ToSlash(filepath.Join(".github", "workflows", "other.yml"))
			require.NoError(t, os.WriteFile(otherPath, []byte("name: other\non: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: "+other+"\n"), 0o600))
			lockBefore := readTempLockfilePins(t)

			p, err := runParity(t, rt, "--relock", "--no-narrow", "--no-interactive", path, otherPath)
			require.ErrorIs(t, err, errSilent, "%+v", p.Findings)
			var checkout []string
			for _, f := range p.Findings {
				if f.Dependency == "actions/checkout@v4" && f.Severity == "error" {
					checkout = append(checkout, f.Detail)
				}
			}
			require.NotEmpty(t, checkout, "%+v", p.Findings)
			assert.Contains(t, checkout[0], "checking v4 history")
			assert.Equal(t, lockBefore, readTempLockfilePins(t), "nothing is written")

			_, stderr, err := runCommandWithHTTP(t, rt, "--relock", "--no-narrow", "--no-interactive", path, otherPath)
			require.ErrorIs(t, err, errSilent)
			if name == "planning fails" {
				assert.Contains(t, stderr, "Couldn't verify 1 pin: GitHub API returned 503 Service Unavailable\n  actions/checkout@v4\n", "the terminal reports it too")
			}
		})
	}
}

// TestParity_ReadOnlyRelockKeepsDeferredFindings: a read-only run never
// writes, so a finding --relock would excuse on the strength of a write
// must stand.
func TestParity_ReadOnlyRelockKeepsDeferredFindings(t *testing.T) {
	for _, args := range [][]string{{"--relock", "--no-fix"}, {"--relock", "--verify"}, {"--accept-moved", "--no-fix"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			reg := &httpmock.Registry{}
			stubGraph(t, reg,
				map[string]string{"actions/checkout": parityTestMoved},
				map[string]string{"actions/checkout": parityTestMoved},
				nil)
			reg.RegisterReusable(httpmock.REST("GET", `repos/actions/checkout/compare/`), httpmock.StatusResponse(http.StatusServiceUnavailable))
			useDefaultTransport(t, reg)
			path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
`, "actions/checkout@v4=sha1-"+parityTestSHA)

			p, err := runParity(t, reg, append(args, "--no-narrow", "--no-interactive", path)...)
			require.ErrorIs(t, err, errSilent, "%+v", p.Findings)
			assert.False(t, p.Valid)
			var checkout []string
			for _, f := range p.Findings {
				if f.Dependency == "actions/checkout@v4" && f.Severity == "error" {
					checkout = append(checkout, f.Detail)
				}
			}
			require.NotEmpty(t, checkout, "%+v", p.Findings)
			assert.Contains(t, checkout[0], "checking v4 history")
		})
	}
}

// TestParity_AcceptMovedCarriesProofThroughNarrowing: the composite's
// child o/child@v4.2.1 left its tag's history, and the composite moved to a
// version that no longer uses it, though its recorded edge stays. The
// workflow's new direct o/child@v4, checked at B, narrows to v4.2.1, the tag
// at B, so the write rewrites the child's own key at a verified commit.
func TestParity_AcceptMovedCarriesProofThroughNarrowing(t *testing.T) {
	const rootSHA = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	const liveRoot = "ffffffffffffffffffffffffffffffffffffffff"
	reg := &httpmock.Registry{}
	stubGraph(t, reg,
		map[string]string{"o/child": parityTestMoved, "octo/root": liveRoot},
		map[string]string{"octo/root": liveRoot, "o/child": parityTestMoved},
		nil)
	reg.RegisterReusable(httpmock.REST("GET", `repos/o/child/compare/`),
		httpmock.JSONResponse(map[string]any{"status": "diverged", "merge_base_commit": map[string]any{"sha": "cccccccccccccccccccccccccccccccccccccccc"}}))
	reg.RegisterReusable(httpmock.REST("GET", `repos/octo/root/compare/`),
		httpmock.JSONResponse(map[string]any{"status": "ahead", "merge_base_commit": map[string]any{"sha": rootSHA}}))
	reg.RegisterReusable(httpmock.REST("GET", `repos/o/child/tags`),
		httpmock.JSONResponse(httpmock.TagListResponse("v4.2.1", parityTestMoved, "v4", parityTestMoved)))
	reg.RegisterReusable(httpmock.REST("GET", `repos/octo/root/tags`),
		httpmock.JSONResponse(httpmock.TagListResponse("v1.0.0", liveRoot, "v1", liveRoot)))
	useDefaultTransport(t, reg)
	path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: octo/root@v1
      - uses: o/child@v4
`)
	lock := "version: '" + parserlock.Version + "'\ndependencies:\n" +
		"  'o/child@v4.2.1':\n    ref: 'v4.2.1'\n    commit: 'sha1-" + parityTestSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"  'octo/root@v1':\n    ref: 'v1'\n    commit: 'sha1-" + rootSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"    uses:\n      - 'o/child@v4.2.1'\n" +
		"workflows:\n  '.github/workflows/workflow.yml':\n    - 'octo/root@v1'\n"
	lockPath := filepath.Join(".github", "workflows", "actions.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(lock), 0o600))

	_, stderr, err := runCommandWithHTTP(t, reg, "--accept-moved", "--no-interactive", path)
	after, rerr := os.ReadFile(lockPath)
	require.NoError(t, rerr)
	require.NoError(t, err, "%s\n%s", stderr, after)
	assert.Contains(t, string(after), "'o/child@v4.2.1':\n        ref: 'v4.2.1'\n        commit: 'sha1-"+parityTestMoved+"'")
	assert.NotContains(t, string(after), parityTestSHA)
	_, stderr, err = runCommandWithHTTP(t, reg, "--verify", path)
	require.NoError(t, err, "the repaired lock verifies: %s", stderr)
}

// TestParity_AcceptMovedRepairsEachWorkflowsChild: each workflow's
// composite has a mutable child that left its tag's history, and other.yml
// also uses one of them directly. --accept-moved rebuilds every workflow's
// closure, so each child is locked at its current commit whichever
// workflow's write would otherwise have to repair it.
func TestParity_AcceptMovedRepairsEachWorkflowsChild(t *testing.T) {
	const (
		rootSHA  = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
		root2SHA = "dddddddddddddddddddddddddddddddddddddddd"
		cacheOld = "1111111111111111111111111111111111111111"
		cacheNew = "2222222222222222222222222222222222222222"
	)
	reg := &httpmock.Registry{}
	stubGraph(t, reg,
		map[string]string{"actions/checkout": parityTestMoved, "actions/cache": cacheNew},
		map[string]string{"octo/root": rootSHA, "octo/root2": root2SHA, "actions/checkout": parityTestMoved, "actions/cache": cacheNew},
		map[string]string{
			"octo/root":  "name: r\nruns:\n  using: composite\n  steps:\n    - uses: actions/checkout@v4.2.1\n",
			"octo/root2": "name: r2\nruns:\n  using: composite\n  steps:\n    - uses: actions/cache@v1\n",
		})
	for _, nwo := range []string{"actions/checkout", "actions/cache"} {
		reg.RegisterReusable(httpmock.REST("GET", `repos/`+nwo+`/compare/`),
			httpmock.JSONResponse(map[string]any{"status": "diverged", "merge_base_commit": map[string]any{"sha": "cccccccccccccccccccccccccccccccccccccccc"}}))
	}
	useDefaultTransport(t, reg)
	path := writeTempWorkflow(t, "name: ci\non: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: octo/root@v1\n")
	other := filepath.ToSlash(filepath.Join(".github", "workflows", "other.yml"))
	require.NoError(t, os.WriteFile(other, []byte("name: other\non: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n"+
		"      - uses: actions/checkout@v4.2.1\n      - uses: octo/root2@v1\n"), 0o600))
	lock := "version: '" + parserlock.Version + "'\ndependencies:\n" +
		"  'actions/cache@v1':\n    ref: 'v1'\n    commit: 'sha1-" + cacheOld + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"  'actions/checkout@v4.2.1':\n    ref: 'v4.2.1'\n    commit: 'sha1-" + parityTestSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"  'octo/root2@v1':\n    ref: 'v1'\n    commit: 'sha1-" + root2SHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"    uses:\n      - 'actions/cache@v1'\n" +
		"  'octo/root@v1':\n    ref: 'v1'\n    commit: 'sha1-" + rootSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"    uses:\n      - 'actions/checkout@v4.2.1'\n" +
		"workflows:\n  '.github/workflows/other.yml':\n    - 'octo/root2@v1'\n  '.github/workflows/workflow.yml':\n    - 'octo/root@v1'\n"
	lockPath := filepath.Join(".github", "workflows", "actions.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(lock), 0o600))

	p, err := runParity(t, reg, "--accept-moved", "--no-narrow", "--no-interactive", path, other)
	require.NoError(t, err, "%+v", p.Findings)
	after, rerr := os.ReadFile(lockPath)
	require.NoError(t, rerr)
	assert.Contains(t, string(after), "commit: 'sha1-"+parityTestMoved+"'")
	assert.Contains(t, string(after), "commit: 'sha1-"+cacheNew+"'")
	assert.NotContains(t, string(after), parityTestSHA)
	assert.NotContains(t, string(after), cacheOld)
}

// TestParity_RelockNeverExcusesUnreachablePin: a composite's child left its
// tag's history, and a new direct use would rewrite its key. That repair is
// --accept-moved's to make: plain --relock leaves an unreachable pin an
// error, whatever the write would do.
func TestParity_RelockNeverExcusesUnreachablePin(t *testing.T) {
	const rootSHA = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	reg := &httpmock.Registry{}
	stubGraph(t, reg,
		map[string]string{"actions/checkout": parityTestMoved},
		map[string]string{"octo/root": rootSHA, "actions/checkout": parityTestMoved},
		map[string]string{"octo/root": "name: r\nruns:\n  using: composite\n  steps:\n    - uses: actions/checkout@v4.2.1\n"})
	reg.RegisterReusable(httpmock.REST("GET", `repos/actions/checkout/compare/`),
		httpmock.JSONResponse(map[string]any{"status": "diverged", "merge_base_commit": map[string]any{"sha": "cccccccccccccccccccccccccccccccccccccccc"}}))
	useDefaultTransport(t, reg)
	path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: octo/root@v1
      - uses: actions/checkout@v4.2.1
`)
	lock := "version: '" + parserlock.Version + "'\ndependencies:\n" +
		"  'octo/root@v1':\n    ref: 'v1'\n    commit: 'sha1-" + rootSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"    uses:\n      - 'actions/checkout@v4.2.1'\n" +
		"  'actions/checkout@v4.2.1':\n    ref: 'v4.2.1'\n    commit: 'sha1-" + parityTestSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"workflows:\n  '.github/workflows/workflow.yml':\n    - 'octo/root@v1'\n"
	lockPath := filepath.Join(".github", "workflows", "actions.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(lock), 0o600))

	p, err := runParity(t, reg, "--relock", "--no-narrow", "--no-interactive", path)
	require.ErrorIs(t, err, errSilent, "%+v", p.Findings)
	var unreachable []string
	for _, f := range p.Findings {
		if f.Category == "unreachable-pin" && f.Severity == "error" {
			unreachable = append(unreachable, f.Dependency)
		}
	}
	assert.Contains(t, unreachable, "actions/checkout@v4.2.1", "%+v", p.Findings)
	after, rerr := os.ReadFile(lockPath)
	require.NoError(t, rerr)
	assert.Equal(t, lock, string(after), "nothing is written")
}

// TestParity_AcceptMovedKeepsRedirectedSharedPinBlocking: the workflow and
// a composite share octo/old@v4, whose recorded commit left v4's history,
// and octo/old now redirects to octo/new. --accept-moved re-resolves the
// workflow's use under the new name, so the composite's octo/old entry
// would stay at the rejected commit: nothing may be written.
func TestParity_AcceptMovedKeepsRedirectedSharedPinBlocking(t *testing.T) {
	const rootSHA = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	reg := &httpmock.Registry{}
	reg.RegisterReusable(httpmock.GraphQL(`commit: object\(oid`), func(req *http.Request) (*http.Response, error) {
		var body struct {
			Variables map[string]any `json:"variables"`
		}
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		data := map[string]any{}
		for k, v := range body.Variables {
			i, ok := strings.CutPrefix(k, "oid")
			if !ok {
				continue
			}
			nwo := body.Variables["owner"+i].(string) + "/" + body.Variables["name"+i].(string)
			head := v
			if nwo != "octo/root" {
				head, nwo = parityTestMoved, "octo/new"
			}
			data["a"+i] = map[string]any{"nameWithOwner": nwo, "databaseId": 1, "commit": map[string]any{"oid": v},
				"tag": map[string]any{"oid": head}, "tagRef": map[string]any{"oid": head}}
		}
		return httpmock.JSONResponse(map[string]any{"data": data})(req)
	})
	reg.RegisterReusable(httpmock.GraphQL(`.`), func(req *http.Request) (*http.Response, error) {
		var body struct {
			Variables map[string]any `json:"variables"`
		}
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		data := map[string]any{}
		for k, v := range body.Variables {
			if i, ok := strings.CutPrefix(k, "owner"); ok {
				nwo := v.(string) + "/" + body.Variables["name"+i].(string)
				if nwo == "octo/root" {
					data["a"+i] = testRepoResponse(nwo, rootSHA, "name: r\nruns:\n  using: composite\n  steps:\n    - uses: octo/old@v4\n")
				} else {
					data["a"+i] = testRepoResponse("octo/new", parityTestMoved, nodeActionYAML)
				}
			}
		}
		return httpmock.JSONResponse(map[string]any{"data": data})(req)
	})
	for _, nwo := range []string{"octo/old", "octo/new"} {
		reg.RegisterReusable(httpmock.REST("GET", `repos/`+nwo+`/compare/`),
			httpmock.JSONResponse(map[string]any{"status": "diverged", "merge_base_commit": map[string]any{"sha": "cccccccccccccccccccccccccccccccccccccccc"}}))
		reg.RegisterReusable(httpmock.REST("GET", `^/repos/`+nwo+`$`),
			httpmock.JSONResponse(map[string]any{"full_name": "octo/new", "id": 1, "owner": map[string]any{"id": 1}}))
	}
	useDefaultTransport(t, reg)
	path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: octo/root@v1
      - uses: octo/old@v4
`)
	lock := "version: '" + parserlock.Version + "'\ndependencies:\n" +
		"  'octo/root@v1':\n    ref: 'v1'\n    commit: 'sha1-" + rootSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"    uses:\n      - 'octo/old@v4'\n" +
		"  'octo/old@v4':\n    ref: 'v4'\n    commit: 'sha1-" + parityTestSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"workflows:\n  '.github/workflows/workflow.yml':\n    - 'octo/old@v4'\n    - 'octo/root@v1'\n"
	lockPath := filepath.Join(".github", "workflows", "actions.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(lock), 0o600))
	wfBefore, err := os.ReadFile(path)
	require.NoError(t, err)

	_, stderr, err := runCommandWithHTTP(t, reg, "--accept-moved", "--no-narrow", "--no-interactive", path)
	after, rerr := os.ReadFile(lockPath)
	require.NoError(t, rerr)
	require.ErrorIs(t, err, errSilent, "%s\n%s", stderr, after)
	assert.Contains(t, stderr, "upgrade or replace octo/root@v1 with a version whose pin of octo/old can be verified", "the advice names what keeps the entry, not the override that just failed")
	assert.NotContains(t, stderr, "run `gh actions-lock --accept-moved`")
	assert.Contains(t, stderr, "octo/old")
	assert.Equal(t, lock, string(after), "nothing is written")
	wfAfter, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(wfBefore), string(wfAfter))
}

// TestParity_RelockKeepsSiblingTagAtSameCommit: v4 moved onto the commit
// v4.2.1 is already locked at. Re-locking v4 must still write v4's own
// entry, so the workflow's `uses: v4` stays locked and verifies; so must a
// composite's edge to v4.
func TestParity_RelockKeepsSiblingTagAtSameCommit(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		cmp       string
		base      string
		composite bool
	}{
		{"relock, moved ahead", []string{"--relock"}, "ahead", parityTestSHA, false},
		{"accept-moved, history rewritten", []string{"--accept-moved"}, "diverged", "cccccccccccccccccccccccccccccccccccccccc", false},
		{"relock, moved ahead, composite", []string{"--relock"}, "ahead", parityTestSHA, true},
		{"accept-moved, history rewritten, composite", []string{"--accept-moved"}, "diverged", "cccccccccccccccccccccccccccccccccccccccc", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const rootSHA = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
			reg := &httpmock.Registry{}
			reg.RegisterReusable(httpmock.GraphQL(`commit: object\(oid`), func(req *http.Request) (*http.Response, error) {
				var body struct {
					Variables map[string]any `json:"variables"`
				}
				require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
				data := map[string]any{}
				for k, v := range body.Variables {
					if i, ok := strings.CutPrefix(k, "oid"); ok {
						nwo, head := "actions/checkout", parityTestMoved
						if body.Variables["owner"+i] == "octo" {
							nwo, head = "octo/root", rootSHA
						}
						data["a"+i] = map[string]any{"nameWithOwner": nwo, "databaseId": 1, "commit": map[string]any{"oid": v},
							"tag": map[string]any{"oid": head}, "tagRef": map[string]any{"oid": head}}
					}
				}
				return httpmock.JSONResponse(map[string]any{"data": data})(req)
			})
			reg.RegisterReusable(httpmock.GraphQL(`.`), func(req *http.Request) (*http.Response, error) {
				var body struct {
					Variables map[string]any `json:"variables"`
				}
				require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
				data := map[string]any{}
				for k, v := range body.Variables {
					if i, ok := strings.CutPrefix(k, "owner"); ok {
						if v == "octo" {
							data["a"+i] = testRepoResponse("octo/root", rootSHA, "name: r\nruns:\n  using: composite\n  steps:\n    - uses: actions/checkout@v4\n")
							continue
						}
						data["a"+i] = testRepoResponse("actions/checkout", parityTestMoved, nodeActionYAML)
					}
				}
				return httpmock.JSONResponse(map[string]any{"data": data})(req)
			})
			reg.RegisterReusable(httpmock.REST("GET", `repos/actions/checkout/compare/`),
				httpmock.JSONResponse(map[string]any{"status": tc.cmp, "merge_base_commit": map[string]any{"sha": tc.base}}))
			useDefaultTransport(t, reg)
			steps, roots, root := "", "", ""
			if tc.composite {
				steps = "      - uses: octo/root@v1\n"
				roots = "    - 'octo/root@v1'\n"
				root = "  'octo/root@v1':\n    ref: 'v1'\n    commit: 'sha1-" + rootSHA + "'\n    owner_id: 1\n    repo_id: 1\n    uses:\n      - 'actions/checkout@v4'\n"
			}
			path := writeTempWorkflow(t, "name: ci\non: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n"+steps+
				"      - uses: actions/checkout@v4\n      - uses: actions/checkout@v4.2.1\n")
			lock := "version: '" + parserlock.Version + "'\ndependencies:\n" + root +
				"  'actions/checkout@v4':\n    ref: 'v4'\n    commit: 'sha1-" + parityTestSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
				"  'actions/checkout@v4.2.1':\n    ref: 'v4.2.1'\n    commit: 'sha1-" + parityTestMoved + "'\n    owner_id: 1\n    repo_id: 1\n" +
				"workflows:\n  '.github/workflows/workflow.yml':\n    - 'actions/checkout@v4'\n    - 'actions/checkout@v4.2.1'\n" + roots
			lockPath := filepath.Join(".github", "workflows", "actions.lock")
			require.NoError(t, os.WriteFile(lockPath, []byte(lock), 0o600))

			_, stderr, err := runCommandWithHTTP(t, reg, append(tc.args, "--no-narrow", "--no-interactive", path)...)
			after, rerr := os.ReadFile(lockPath)
			require.NoError(t, rerr)
			require.NoError(t, err, "%s\n%s", stderr, after)
			assert.Contains(t, string(after), "'actions/checkout@v4':\n        ref: 'v4'\n        commit: 'sha1-"+parityTestMoved+"'")
			assert.NotContains(t, string(after), parityTestSHA)
			_, stderr, err = runCommandWithHTTP(t, reg, "--verify", path)
			require.NoError(t, err, "the relocked lock verifies: %s", stderr)
		})
	}
}

// reposDown fails every request about the given owner/repos except the
// parity query, which reports the pins themselves.
type reposDown struct {
	inner http.RoundTripper
	nwos  []string
}

func (c reposDown) RoundTrip(req *http.Request) (*http.Response, error) {
	if !httpmock.GraphQL(`commit: object\(oid`)(req) {
		for _, nwo := range c.nwos {
			owner, repo, _ := strings.Cut(nwo, "/")
			if strings.Contains(req.URL.Path, "/repos/"+nwo+"/") || strings.HasSuffix(req.URL.Path, "/repos/"+nwo) || httpmock.GraphQLForRepo(owner, repo)(req) {
				return httpmock.StatusResponse(http.StatusServiceUnavailable)(req)
			}
		}
	}
	return c.inner.RoundTrip(req)
}

// TestParity_RelockKeepsDescendantOfUnresolvedChildBlocking: the fresh
// graph can't speak for what lies under a composite that didn't resolve.
// root -> middle -> leaf: root resolves unchanged, middle doesn't, so leaf
// is absent from the fresh graph. That proves nothing about it: its failed
// check must still block.
func TestParity_RelockKeepsDescendantOfUnresolvedChildBlocking(t *testing.T) {
	const rootSHA = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	const middleSHA = "cccccccccccccccccccccccccccccccccccccccc"
	root := "name: r\nruns:\n  using: composite\n  steps:\n    - uses: octo/middle@v1\n"
	reg := &httpmock.Registry{}
	reg.RegisterReusable(httpmock.GraphQL(`commit: object\(oid`), func(req *http.Request) (*http.Response, error) {
		var body struct {
			Variables map[string]any `json:"variables"`
		}
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		data := map[string]any{}
		for k, v := range body.Variables {
			i, ok := strings.CutPrefix(k, "oid")
			if !ok {
				continue
			}
			nwo := body.Variables["owner"+i].(string) + "/" + body.Variables["name"+i].(string)
			head := v
			if nwo == "actions/checkout" {
				head = parityTestMoved
			}
			data["a"+i] = map[string]any{
				"nameWithOwner": nwo,
				"databaseId":    1,
				"commit":        map[string]any{"oid": v},
				"tag":           map[string]any{"oid": head},
				"tagRef":        map[string]any{"oid": head},
			}
		}
		return httpmock.JSONResponse(map[string]any{"data": data})(req)
	})
	reg.RegisterReusable(httpmock.GraphQLForRepo("octo", "root"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": testRepoResponse("octo/root", rootSHA, root)},
	}))
	rt := reposDown{reg, []string{"octo/middle", "actions/checkout"}}
	useDefaultTransport(t, rt)
	path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: octo/root@v1
`)
	lock := "version: '" + parserlock.Version + "'\ndependencies:\n" +
		"  'octo/root@v1':\n    ref: 'v1'\n    commit: 'sha1-" + rootSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"    uses:\n      - 'octo/middle@v1'\n" +
		"  'octo/middle@v1':\n    ref: 'v1'\n    commit: 'sha1-" + middleSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"    uses:\n      - 'actions/checkout@v4'\n" +
		"  'actions/checkout@v4':\n    ref: 'v4'\n    commit: 'sha1-" + parityTestSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"workflows:\n  '.github/workflows/workflow.yml':\n    - 'octo/root@v1'\n"
	lockPath := filepath.Join(".github", "workflows", "actions.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(lock), 0o600))

	_, stderr, err := runCommandWithHTTP(t, rt, "--relock", "--no-narrow", "--no-interactive", path)
	require.ErrorIs(t, err, errSilent, stderr)
	assert.Contains(t, stderr, "could not be resolved")
	after, rerr := os.ReadFile(lockPath)
	require.NoError(t, rerr)
	assert.Equal(t, lock, string(after), "nothing is written")
}

// TestParity_AcceptMovedRepairsGoneRecordedRef: the recorded tag of a
// full-SHA entry was deleted. The finding recommends --accept-moved, so
// --accept-moved must record a branch that contains the commit, and still
// refuse a commit no branch contains.
func TestParity_AcceptMovedRepairsGoneRecordedRef(t *testing.T) {
	for _, status := range []string{"behind", "diverged"} {
		t.Run(status, func(t *testing.T) { testAcceptMovedGoneRef(t, status) })
	}
}

func testAcceptMovedGoneRef(t *testing.T, status string) {
	const head = "dddddddddddddddddddddddddddddddddddddddd"
	base := parityTestSHA
	if status == "diverged" {
		base = "cccccccccccccccccccccccccccccccccccccccc"
	}
	reg := &httpmock.Registry{}
	// Every pin check finds the commit; no recorded ref resolves.
	for range 4 {
		reg.Register(httpmock.GraphQL(`commit: object\(oid`), func(req *http.Request) (*http.Response, error) {
			var body struct {
				Variables map[string]any `json:"variables"`
			}
			require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
			data := map[string]any{}
			for k := range body.Variables {
				if i, ok := strings.CutPrefix(k, "oid"); ok {
					data["a"+i] = map[string]any{
						"nameWithOwner": "actions/checkout",
						"databaseId":    1,
						"owner":         map[string]any{"databaseId": 1},
						"commit":        map[string]any{"oid": parityTestSHA},
					}
				}
			}
			return httpmock.JSONResponse(map[string]any{"data": data})(req)
		})
	}
	for range 4 {
		reg.Register(httpmock.GraphQLForRepo("actions", "checkout"), httpmock.JSONResponse(map[string]any{
			"data": map[string]any{"a0": testRepoResponse("actions/checkout", parityTestSHA, nodeActionYAML)},
		}))
		reg.Register(httpmock.REST("GET", `repos/actions/checkout$`),
			httpmock.JSONResponse(map[string]any{"full_name": "actions/checkout", "default_branch": "main", "id": 1, "owner": map[string]any{"id": 1}}))
		reg.Register(httpmock.REST("GET", `repos/actions/checkout/git/ref/heads/main`),
			httpmock.JSONResponse(map[string]any{"ref": "refs/heads/main", "object": map[string]any{"sha": head, "type": "commit"}}))
		reg.Register(httpmock.REST("GET", `repos/actions/checkout/tags`), httpmock.JSONResponse(httpmock.TagListResponse()))
		reg.Register(httpmock.REST("GET", `repos/actions/checkout/releases`), httpmock.JSONResponse([]map[string]any{}))
		reg.Register(httpmock.REST("GET", `repos/actions/checkout/branches`), httpmock.JSONResponse(httpmock.BranchListResponse("main", head)))
		reg.Register(httpmock.REST("GET", `repos/actions/checkout/compare/`),
			httpmock.JSONResponse(map[string]any{"status": status, "merge_base_commit": map[string]any{"sha": base}}))
	}
	httpmock.NoTagsNamed(reg, "actions", "checkout")
	path := writeFullSHAWorkflow(t, "v6.1.0")

	lockBefore := readTempLockfilePins(t)

	p, err := runParity(t, reg, "--accept-moved", "--no-interactive", path)
	lock := readTempLockfilePins(t)
	if status == "diverged" {
		require.ErrorIs(t, err, errSilent)
		assert.Equal(t, lockBefore, lock, "no branch contains the commit")
		return
	}
	require.NoError(t, err, "%+v", p.Findings)
	assert.Contains(t, lock, "ref: 'main'")
	assert.Contains(t, lock, "sha1-"+parityTestSHA)
}

// TestParity_AcceptMovedRepairLocksRewrittenPin: --accept-moved repairs a
// full-SHA pin whose recorded ref is gone, and narrowing rewrites `uses:`
// to the tag at that commit. Every rewritten `uses:` must have its own lock
// entry, so the result verifies.
func TestParity_AcceptMovedRepairLocksRewrittenPin(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.RegisterReusable(httpmock.GraphQL(`commit: object\(oid`), func(req *http.Request) (*http.Response, error) {
		var body struct {
			Variables map[string]any `json:"variables"`
		}
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		data := map[string]any{}
		for k, v := range body.Variables {
			i, ok := strings.CutPrefix(k, "oid")
			if !ok {
				continue
			}
			repo := map[string]any{"nameWithOwner": "actions/checkout", "databaseId": 1, "owner": map[string]any{"databaseId": 1}, "commit": map[string]any{"oid": v}}
			if body.Variables["tagRef"+i] == "refs/tags/v7.0.0" {
				repo["tag"], repo["tagRef"] = map[string]any{"oid": v}, map[string]any{"name": "v7.0.0"}
			}
			data["a"+i] = repo
		}
		return httpmock.JSONResponse(map[string]any{"data": data})(req)
	})
	reg.RegisterReusable(httpmock.GraphQL(`.`), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": testRepoResponse("actions/checkout", parityTestSHA, nodeActionYAML)},
	}))
	reg.RegisterReusable(httpmock.REST("GET", `repos/actions/checkout$`),
		httpmock.JSONResponse(map[string]any{"full_name": "actions/checkout", "default_branch": "main", "id": 1, "owner": map[string]any{"id": 1}}))
	reg.RegisterReusable(httpmock.REST("GET", `repos/actions/checkout/tags`), httpmock.JSONResponse(httpmock.TagListResponse("v7.0.0", parityTestSHA)))
	reg.RegisterReusable(httpmock.REST("GET", `repos/actions/checkout/releases`), httpmock.JSONResponse([]map[string]any{}))
	reg.RegisterReusable(httpmock.REST("GET", `repos/actions/checkout/branches`), httpmock.JSONResponse(httpmock.BranchListResponse("main", parityTestMoved)))
	reg.RegisterReusable(httpmock.REST("GET", `repos/actions/checkout/compare/`),
		httpmock.JSONResponse(map[string]any{"status": "diverged", "merge_base_commit": map[string]any{"sha": "cccccccccccccccccccccccccccccccccccccccc"}}))
	reg.RegisterReusable(httpmock.REST("GET", `repos/actions/checkout/git/ref/tags/v7.0.0`),
		httpmock.JSONResponse(map[string]any{"ref": "refs/tags/v7.0.0", "object": map[string]any{"sha": parityTestSHA, "type": "commit"}}))
	path := writeFullSHAWorkflow(t, "v6-gone")

	_, stderr, err := runCommandWithHTTP(t, reg, "--accept-moved", "--no-interactive", path)
	lock := readTempLockfilePins(t)
	require.NoError(t, err, "%s\n%s", stderr, lock)
	wf, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(wf), "uses: actions/checkout@v7.0.0")
	assert.Contains(t, lock, "- 'actions/checkout@v7.0.0'")
	assert.Contains(t, lock, "'actions/checkout@v7.0.0':")
	assert.NotContains(t, lock, "v6-gone")

	_, stderr, err = runCommandWithHTTP(t, reg, "--verify", path)
	require.NoError(t, err, "the repaired workflow verifies: %s", stderr)
}

// TestParity_FullSHAKeyTitleMatchesCause: a full-SHA pin's recorded ref
// that is gone is titled as a missing ref, and only a ref that exists but
// lost the commit is titled as history.
func TestParity_FullSHAKeyTitleMatchesCause(t *testing.T) {
	const live = "cccccccccccccccccccccccccccccccccccccccc"
	tests := []struct {
		name    string
		head    any
		compare bool
		want    string
		notWant string
	}{
		{"recorded ref gone", nil, false, "Ref no longer exists actions/checkout@" + parityTestSHA, "Pinned commit not in upstream history"},
		{"recorded ref lost the commit", map[string]any{"oid": live}, true, "Pinned commit not in upstream history actions/checkout@" + parityTestSHA, "Ref no longer exists"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := &httpmock.Registry{}
			reg.Register(httpmock.GraphQL(`commit: object\(oid`), httpmock.JSONResponse(map[string]any{
				"data": map[string]any{"a0": map[string]any{
					"nameWithOwner": "actions/checkout",
					"databaseId":    1,
					"commit":        map[string]any{"oid": parityTestSHA},
					"head":          tt.head,
					"headRef":       tt.head,
				}},
			}))
			if tt.compare {
				reg.Register(httpmock.REST("GET", `compare/`), httpmock.JSONResponse(map[string]any{
					"status": "diverged", "merge_base_commit": map[string]any{"sha": parityTestMoved},
				}))
			}
			path := writeFullSHAWorkflow(t, "main")

			_, stderr, err := runCommandWithHTTP(t, reg, "--verify", path)
			require.ErrorIs(t, err, errSilent)
			assert.Contains(t, stderr, tt.want)
			assert.NotContains(t, stderr, tt.notWant)
		})
	}
}

// TestParity_FullSHAKeyMovedTagWarns: a full-SHA `uses:` already draws a
// sha-as-ref warning for the same dependency; the moved recorded tag must
// still show under --verify.
func TestParity_FullSHAKeyMovedTagWarns(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(httpmock.GraphQL(`commit: object\(oid`), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": map[string]any{
			"nameWithOwner": "actions/checkout",
			"databaseId":    1,
			"commit":        map[string]any{"oid": parityTestSHA},
			"tag":           map[string]any{"oid": parityTestMoved},
			"tagRef":        map[string]any{},
		}},
	}))
	reg.Register(httpmock.REST("GET", `repos/actions/checkout/compare/`+parityTestSHA+`\.\.\.`+parityTestMoved),
		httpmock.JSONResponse(map[string]any{"status": "ahead", "merge_base_commit": map[string]any{"sha": parityTestSHA}}))
	path := writeFullSHAWorkflow(t, "v6.1.0")

	_, stderr, err := runCommandWithHTTP(t, reg, "--verify", path)
	require.NoError(t, err, stderr)
	assert.Contains(t, stderr, "tag v6.1.0 now points at bbbbbbbbbbbb; locked commit 9ddf35b71a48 is still in its history")
	assert.Equal(t, 1, strings.Count(stderr, "pinned to a bare SHA"), "the sha-as-ref notice still shows once")
}

// TestParity_AcceptMovedKeepsTransitiveFullSHAPinBlocking: a composite's
// full-SHA child whose recorded ref is gone isn't repaired by
// --accept-moved, which doesn't rebuild an unchanged composite's
// dependencies, so it must keep blocking rather than pass unchanged.
func TestParity_AcceptMovedKeepsTransitiveFullSHAPinBlocking(t *testing.T) {
	const parentSHA = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	composite := "name: c\nruns:\n  using: composite\n  steps:\n    - uses: actions/checkout@" + parityTestSHA + "\n"
	reg := &httpmock.Registry{}
	reg.RegisterReusable(httpmock.GraphQL(`commit: object\(oid`), func(req *http.Request) (*http.Response, error) {
		var body struct {
			Variables map[string]any `json:"variables"`
		}
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		data := map[string]any{}
		for k, v := range body.Variables {
			i, ok := strings.CutPrefix(k, "oid")
			if !ok {
				continue
			}
			repo := map[string]any{
				"nameWithOwner": body.Variables["owner"+i].(string) + "/" + body.Variables["name"+i].(string),
				"databaseId":    1,
				"commit":        map[string]any{"oid": v},
			}
			if v == parentSHA {
				repo["tag"], repo["tagRef"] = map[string]any{"oid": parentSHA}, map[string]any{}
			}
			data["a"+i] = repo
		}
		return httpmock.JSONResponse(map[string]any{"data": data})(req)
	})
	reg.RegisterReusable(httpmock.GraphQLForRepo("octo", "composite"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": testRepoResponse("octo/composite", parentSHA, composite)},
	}))
	reg.RegisterReusable(httpmock.GraphQLForRepo("actions", "checkout"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": testRepoResponse("actions/checkout", parityTestSHA, nodeActionYAML)},
	}))
	path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: octo/composite@v1
`)
	lock := "version: '" + parserlock.Version + "'\ndependencies:\n" +
		"  'octo/composite@v1':\n    ref: 'v1'\n    commit: 'sha1-" + parentSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"    uses:\n      - 'actions/checkout@" + parityTestSHA + "'\n" +
		"  'actions/checkout@" + parityTestSHA + "':\n    ref: 'v4-gone'\n    commit: 'sha1-" + parityTestSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"workflows:\n  '.github/workflows/workflow.yml':\n    - 'octo/composite@v1'\n"
	lockPath := filepath.Join(".github", "workflows", "actions.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(lock), 0o600))

	_, stderr, err := runCommandWithHTTP(t, reg, "--accept-moved", "--no-narrow", "--no-interactive", path)
	require.ErrorIs(t, err, errSilent, stderr)
	assert.Contains(t, stderr, "ref v4-gone is not a tag or branch of actions/checkout")
	assert.Contains(t, stderr, "upgrade or replace octo/composite@v1 with a version whose pin of actions/checkout can be verified, or stop using it")
	assert.NotContains(t, stderr, "--accept-moved", "an override that can't repair it is never suggested")
	after, rerr := os.ReadFile(lockPath)
	require.NoError(t, rerr)
	assert.Equal(t, lock, string(after), "nothing is written")
}

// TestParity_AcceptMovedKeepsSharedFullSHAPinBlocking: a full-SHA pin the
// workflow uses directly, and a composite also pins, is one lock entry.
// --accept-moved repairs the direct use, but planning keeps the composite's
// recorded edge to the entry, moved composite or not, so the entry would keep
// its gone ref and the next --verify would reject it. It must keep blocking.
func TestParity_AcceptMovedKeepsSharedFullSHAPinBlocking(t *testing.T) {
	for _, moved := range []bool{false, true} {
		for _, narrow := range []bool{false, true} {
			t.Run(fmt.Sprintf("moved=%v narrow=%v", moved, narrow), func(t *testing.T) { testAcceptMovedSharedPin(t, moved, narrow) })
		}
	}
}

func testAcceptMovedSharedPin(t *testing.T, moved, narrow bool) {
	const parentSHA = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	liveParent := parentSHA
	if moved {
		liveParent = "ffffffffffffffffffffffffffffffffffffffff"
	}
	const head = "dddddddddddddddddddddddddddddddddddddddd"
	composite := "name: c\nruns:\n  using: composite\n  steps:\n    - uses: actions/checkout@" + parityTestSHA + "\n"
	reg := &httpmock.Registry{}
	reg.RegisterReusable(httpmock.GraphQL(`commit: object\(oid`), func(req *http.Request) (*http.Response, error) {
		var body struct {
			Variables map[string]any `json:"variables"`
		}
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		data := map[string]any{}
		for k, v := range body.Variables {
			i, ok := strings.CutPrefix(k, "oid")
			if !ok {
				continue
			}
			repo := map[string]any{
				"nameWithOwner": body.Variables["owner"+i].(string) + "/" + body.Variables["name"+i].(string),
				"databaseId":    1,
				"commit":        map[string]any{"oid": v},
			}
			if v == parentSHA || v == liveParent {
				repo["tag"], repo["tagRef"] = map[string]any{"oid": liveParent}, map[string]any{"oid": liveParent}
			}
			data["a"+i] = repo
		}
		return httpmock.JSONResponse(map[string]any{"data": data})(req)
	})
	// Both direct refs resolve in one batch.
	repos := map[string]map[string]any{
		"octo/composite":   testRepoResponse("octo/composite", liveParent, composite),
		"actions/checkout": testRepoResponse("actions/checkout", parityTestSHA, nodeActionYAML),
	}
	reg.RegisterReusable(httpmock.GraphQL(`.`), func(req *http.Request) (*http.Response, error) {
		var body struct {
			Variables map[string]any `json:"variables"`
		}
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		data := map[string]any{}
		for k, v := range body.Variables {
			if i, ok := strings.CutPrefix(k, "owner"); ok {
				data["a"+i] = repos[v.(string)+"/"+body.Variables["name"+i].(string)]
			}
		}
		return httpmock.JSONResponse(map[string]any{"data": data})(req)
	})
	reg.RegisterReusable(httpmock.REST("GET", `repos/actions/checkout$`),
		httpmock.JSONResponse(map[string]any{"full_name": "actions/checkout", "default_branch": "main", "id": 1, "owner": map[string]any{"id": 1}}))
	reg.RegisterReusable(httpmock.REST("GET", `repos/actions/checkout/git/ref/heads/main`),
		httpmock.JSONResponse(map[string]any{"ref": "refs/heads/main", "object": map[string]any{"sha": head, "type": "commit"}}))
	reg.RegisterReusable(httpmock.REST("GET", `repos/actions/checkout/tags`), httpmock.JSONResponse(httpmock.TagListResponse()))
	reg.RegisterReusable(httpmock.REST("GET", `repos/actions/checkout/releases`), httpmock.JSONResponse([]map[string]any{}))
	reg.RegisterReusable(httpmock.REST("GET", `repos/actions/checkout/branches`), httpmock.JSONResponse(httpmock.BranchListResponse("main", head)))
	reg.RegisterReusable(httpmock.REST("GET", `repos/actions/checkout/compare/`),
		httpmock.JSONResponse(map[string]any{"status": "behind", "merge_base_commit": map[string]any{"sha": parityTestSHA}}))
	reg.RegisterReusable(httpmock.REST("GET", `repos/octo/composite/compare/`),
		httpmock.JSONResponse(map[string]any{"status": "ahead", "merge_base_commit": map[string]any{"sha": parentSHA}}))
	httpmock.NoTagsNamed(reg, "actions", "checkout")
	path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: octo/composite@v1
      - uses: actions/checkout@`+parityTestSHA+`
`)
	lock := "version: '" + parserlock.Version + "'\ndependencies:\n" +
		"  'octo/composite@v1':\n    ref: 'v1'\n    commit: 'sha1-" + parentSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"    uses:\n      - 'actions/checkout@" + parityTestSHA + "'\n" +
		"  'actions/checkout@" + parityTestSHA + "':\n    ref: 'v4-gone'\n    commit: 'sha1-" + parityTestSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"workflows:\n  '.github/workflows/workflow.yml':\n    - 'octo/composite@v1'\n    - 'actions/checkout@" + parityTestSHA + "'\n"
	lockPath := filepath.Join(".github", "workflows", "actions.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(lock), 0o600))

	args := []string{"--accept-moved", "--no-interactive", path}
	if !narrow {
		args = append([]string{"--no-narrow"}, args...)
	}
	wfBefore, err := os.ReadFile(path)
	require.NoError(t, err)
	_, stderr, err := runCommandWithHTTP(t, reg, args...)
	after, rerr := os.ReadFile(lockPath)
	require.NoError(t, rerr)
	wfAfter, werr := os.ReadFile(path)
	require.NoError(t, werr)
	assert.Equal(t, string(wfBefore), string(wfAfter), "the workflow is not rewritten")
	require.ErrorIs(t, err, errSilent, "%s\n%s", stderr, after)
	assert.Contains(t, stderr, "ref v4-gone is not a tag or branch of actions/checkout")
	assert.Contains(t, stderr, "upgrade or replace octo/composite@v1 with a version whose pin of actions/checkout can be verified, or stop using it")
	assert.NotContains(t, stderr, "--accept-moved", "an override that can't repair it is never suggested")
	assert.Equal(t, lock, string(after), "nothing is written")
}

// TestParity_AcceptMovedKeepsRepairedCompositeChildren: --accept-moved
// re-records a direct full-SHA composite whose recorded ref is gone. The
// repair must keep the composite's dependencies, and the result must verify.
func TestParity_AcceptMovedKeepsRepairedCompositeChildren(t *testing.T) {
	const compSHA = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	const head = "dddddddddddddddddddddddddddddddddddddddd"
	composite := "name: c\nruns:\n  using: composite\n  steps:\n    - uses: actions/checkout@v4\n"
	reg := &httpmock.Registry{}
	reg.RegisterReusable(httpmock.GraphQL(`commit: object\(oid`), func(req *http.Request) (*http.Response, error) {
		var body struct {
			Variables map[string]any `json:"variables"`
		}
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		data := map[string]any{}
		for k, v := range body.Variables {
			i, ok := strings.CutPrefix(k, "oid")
			if !ok {
				continue
			}
			nwo := body.Variables["owner"+i].(string) + "/" + body.Variables["name"+i].(string)
			repo := map[string]any{"nameWithOwner": nwo, "databaseId": 1, "commit": map[string]any{"oid": v}}
			switch {
			case nwo == "actions/checkout":
				repo["tag"], repo["tagRef"] = map[string]any{"oid": v}, map[string]any{"oid": v}
			case body.Variables["headRef"+i] == "refs/heads/main":
				repo["head"], repo["headRef"] = map[string]any{"oid": head}, map[string]any{"oid": head}
			}
			data["a"+i] = repo
		}
		return httpmock.JSONResponse(map[string]any{"data": data})(req)
	})
	repos := map[string]map[string]any{
		"octo/composite":   testRepoResponse("octo/composite", compSHA, composite),
		"actions/checkout": testRepoResponse("actions/checkout", parityTestSHA, nodeActionYAML),
	}
	reg.RegisterReusable(httpmock.GraphQL(`.`), func(req *http.Request) (*http.Response, error) {
		var body struct {
			Variables map[string]any `json:"variables"`
		}
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		data := map[string]any{}
		for k, v := range body.Variables {
			if i, ok := strings.CutPrefix(k, "owner"); ok {
				data["a"+i] = repos[v.(string)+"/"+body.Variables["name"+i].(string)]
			}
		}
		return httpmock.JSONResponse(map[string]any{"data": data})(req)
	})
	reg.RegisterReusable(httpmock.REST("GET", `repos/octo/composite$`),
		httpmock.JSONResponse(map[string]any{"full_name": "octo/composite", "default_branch": "main", "id": 1, "owner": map[string]any{"id": 1}}))
	reg.RegisterReusable(httpmock.REST("GET", `repos/octo/composite/git/ref/heads/main`),
		httpmock.JSONResponse(map[string]any{"ref": "refs/heads/main", "object": map[string]any{"sha": head, "type": "commit"}}))
	reg.RegisterReusable(httpmock.REST("GET", `repos/octo/composite/tags`), httpmock.JSONResponse(httpmock.TagListResponse()))
	reg.RegisterReusable(httpmock.REST("GET", `repos/octo/composite/releases`), httpmock.JSONResponse([]map[string]any{}))
	reg.RegisterReusable(httpmock.REST("GET", `repos/octo/composite/branches`), httpmock.JSONResponse(httpmock.BranchListResponse("main", head)))
	reg.RegisterReusable(httpmock.REST("GET", `repos/octo/composite/compare/`),
		httpmock.JSONResponse(map[string]any{"status": "behind", "merge_base_commit": map[string]any{"sha": compSHA}}))
	httpmock.NoTagsNamed(reg, "octo", "composite")
	path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: octo/composite@`+compSHA+`
`)
	lock := "version: '" + parserlock.Version + "'\ndependencies:\n" +
		"  'octo/composite@" + compSHA + "':\n    ref: 'v1-gone'\n    commit: 'sha1-" + compSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"    uses:\n      - 'actions/checkout@v4'\n" +
		"  'actions/checkout@v4':\n    ref: 'v4'\n    commit: 'sha1-" + parityTestSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"workflows:\n  '.github/workflows/workflow.yml':\n    - 'octo/composite@" + compSHA + "'\n"
	lockPath := filepath.Join(".github", "workflows", "actions.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(lock), 0o600))

	_, stderr, err := runCommandWithHTTP(t, reg, "--accept-moved", "--no-narrow", "--no-interactive", path)
	after, rerr := os.ReadFile(lockPath)
	require.NoError(t, rerr)
	require.NoError(t, err, "%s\n%s", stderr, after)
	assert.NotContains(t, string(after), "v1-gone")
	assert.Contains(t, string(after), "'actions/checkout@v4':", "the composite's child stays")
	assert.Contains(t, string(after), "- 'actions/checkout@v4'", "and so does its edge")

	_, stderr, err = runCommandWithHTTP(t, reg, "--verify", path)
	require.NoError(t, err, "the repaired lock verifies: %s", stderr)
}

// TestParity_AcceptMovedKeepsNestedSharedFullSHAPinBlocking: the shared
// full-SHA entry hangs under a nested composite (outer -> inner -> pin),
// and the workflow uses it directly too. Whichever composite moved,
// planning keeps a recorded edge to the old entry, so --accept-moved must
// keep blocking rather than write a lock --verify rejects.
func TestParity_AcceptMovedKeepsNestedSharedFullSHAPinBlocking(t *testing.T) {
	for _, moved := range []string{"", "octo/outer", "octo/inner"} {
		t.Run("moved="+moved, func(t *testing.T) { testNestedSharedPin(t, moved) })
	}
}

func testNestedSharedPin(t *testing.T, moved string) {
	const (
		outerSHA = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
		innerSHA = "cccccccccccccccccccccccccccccccccccccccc"
		liveSHA  = "ffffffffffffffffffffffffffffffffffffffff"
		head     = "dddddddddddddddddddddddddddddddddddddddd"
	)
	recorded := map[string]string{"octo/outer": outerSHA, "octo/inner": innerSHA}
	live := func(nwo string) string {
		if nwo == moved {
			return liveSHA
		}
		return recorded[nwo]
	}
	reg := &httpmock.Registry{}
	reg.RegisterReusable(httpmock.GraphQL(`commit: object\(oid`), func(req *http.Request) (*http.Response, error) {
		var body struct {
			Variables map[string]any `json:"variables"`
		}
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		data := map[string]any{}
		for k, v := range body.Variables {
			i, ok := strings.CutPrefix(k, "oid")
			if !ok {
				continue
			}
			nwo := body.Variables["owner"+i].(string) + "/" + body.Variables["name"+i].(string)
			repo := map[string]any{"nameWithOwner": nwo, "databaseId": 1, "commit": map[string]any{"oid": v}}
			if _, ok := recorded[nwo]; ok {
				repo["tag"], repo["tagRef"] = map[string]any{"oid": live(nwo)}, map[string]any{"oid": live(nwo)}
			}
			data["a"+i] = repo
		}
		return httpmock.JSONResponse(map[string]any{"data": data})(req)
	})
	repos := map[string]map[string]any{
		"octo/outer":       testRepoResponse("octo/outer", live("octo/outer"), "name: o\nruns:\n  using: composite\n  steps:\n    - uses: octo/inner@v1\n"),
		"octo/inner":       testRepoResponse("octo/inner", live("octo/inner"), "name: i\nruns:\n  using: composite\n  steps:\n    - uses: actions/checkout@"+parityTestSHA+"\n"),
		"actions/checkout": testRepoResponse("actions/checkout", parityTestSHA, nodeActionYAML),
	}
	reg.RegisterReusable(httpmock.GraphQL(`.`), func(req *http.Request) (*http.Response, error) {
		var body struct {
			Variables map[string]any `json:"variables"`
		}
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		data := map[string]any{}
		for k, v := range body.Variables {
			if i, ok := strings.CutPrefix(k, "owner"); ok {
				data["a"+i] = repos[v.(string)+"/"+body.Variables["name"+i].(string)]
			}
		}
		return httpmock.JSONResponse(map[string]any{"data": data})(req)
	})
	for nwo, sha := range recorded {
		reg.RegisterReusable(httpmock.REST("GET", `repos/`+nwo+`/compare/`),
			httpmock.JSONResponse(map[string]any{"status": "ahead", "merge_base_commit": map[string]any{"sha": sha}}))
	}
	reg.RegisterReusable(httpmock.REST("GET", `repos/actions/checkout$`),
		httpmock.JSONResponse(map[string]any{"full_name": "actions/checkout", "default_branch": "main", "id": 1, "owner": map[string]any{"id": 1}}))
	reg.RegisterReusable(httpmock.REST("GET", `repos/actions/checkout/git/ref/heads/main`),
		httpmock.JSONResponse(map[string]any{"ref": "refs/heads/main", "object": map[string]any{"sha": head, "type": "commit"}}))
	reg.RegisterReusable(httpmock.REST("GET", `repos/actions/checkout/tags`), httpmock.JSONResponse(httpmock.TagListResponse()))
	reg.RegisterReusable(httpmock.REST("GET", `repos/actions/checkout/releases`), httpmock.JSONResponse([]map[string]any{}))
	reg.RegisterReusable(httpmock.REST("GET", `repos/actions/checkout/branches`), httpmock.JSONResponse(httpmock.BranchListResponse("main", head)))
	reg.RegisterReusable(httpmock.REST("GET", `repos/actions/checkout/compare/`),
		httpmock.JSONResponse(map[string]any{"status": "behind", "merge_base_commit": map[string]any{"sha": parityTestSHA}}))
	httpmock.NoTagsNamed(reg, "actions", "checkout")
	path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: octo/outer@v1
      - uses: actions/checkout@`+parityTestSHA+`
`)
	lock := "version: '" + parserlock.Version + "'\ndependencies:\n" +
		"  'octo/outer@v1':\n    ref: 'v1'\n    commit: 'sha1-" + outerSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"    uses:\n      - 'octo/inner@v1'\n" +
		"  'octo/inner@v1':\n    ref: 'v1'\n    commit: 'sha1-" + innerSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"    uses:\n      - 'actions/checkout@" + parityTestSHA + "'\n" +
		"  'actions/checkout@" + parityTestSHA + "':\n    ref: 'v4-gone'\n    commit: 'sha1-" + parityTestSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"workflows:\n  '.github/workflows/workflow.yml':\n    - 'octo/outer@v1'\n    - 'actions/checkout@" + parityTestSHA + "'\n"
	lockPath := filepath.Join(".github", "workflows", "actions.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(lock), 0o600))

	_, stderr, err := runCommandWithHTTP(t, reg, "--accept-moved", "--no-narrow", "--no-interactive", path)
	after, rerr := os.ReadFile(lockPath)
	require.NoError(t, rerr)
	require.ErrorIs(t, err, errSilent, "%s\n%s", stderr, after)
	assert.Contains(t, stderr, "ref v4-gone is not a tag or branch of actions/checkout")
	assert.Contains(t, stderr, "upgrade or replace octo/inner@v1")
	assert.Equal(t, lock, string(after), "nothing is written")
}

// refusalDetail is the finding detail for a `uses:` ref parity refuses.
func refusalDetail(ref string) string {
	if ghapi.DescribeName(ref) {
		return "ref " + ref + " looks like `git describe` output"
	}
	return "ref " + ref + " is not a tag or branch of actions/checkout"
}

// TestParity_AcceptMovedReadOnlyStillBlocks: read-only runs never plan, so
// --accept-moved can't repair anything there and must not downgrade the
// failure of a full-SHA pin whose recorded ref is gone.
func TestParity_AcceptMovedReadOnlyStillBlocks(t *testing.T) {
	for _, mode := range []string{"--verify", "--no-fix"} {
		t.Run(mode, func(t *testing.T) {
			reg := &httpmock.Registry{}
			reg.RegisterReusable(httpmock.GraphQL(`commit: object\(oid`), httpmock.JSONResponse(map[string]any{
				"data": map[string]any{"a0": map[string]any{
					"nameWithOwner": "actions/checkout",
					"databaseId":    1,
					"commit":        map[string]any{"oid": parityTestSHA},
				}},
			}))
			reg.RegisterReusable(httpmock.GraphQLForRepo("actions", "checkout"), httpmock.JSONResponse(map[string]any{
				"data": map[string]any{"a0": testRepoResponse("actions/checkout", parityTestSHA, nodeActionYAML)},
			}))
			path := writeFullSHAWorkflow(t, "v6.1.0")

			p, err := runParity(t, reg, mode, "--accept-moved", path)
			require.ErrorIs(t, err, errSilent)
			var blocking []string
			for _, f := range p.Findings {
				if f.Severity == "error" {
					blocking = append(blocking, f.Category)
				}
			}
			assert.Contains(t, blocking, "unreachable-pin")
		})
	}
}

// writeFullSHAWorkflow writes a workflow using actions/checkout at
// parityTestSHA, locked to that commit with the given recorded ref.
// withRepoID answers a repository fixture with the repo ID the identity
// proof compares against the lockfile's.
func withRepoID(repo map[string]any, id int) map[string]any {
	repo["databaseId"] = id
	return repo
}

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
		require.NoError(t, err, stderr)
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

// TestSHARecordedRef_RenameKeepsFullSHAUse: a workflow's own full-SHA pin
// whose entry records the SHA as its ref, in a repository since renamed
// with the same repo ID, moves to the new name and keeps its full SHA; only
// the entry's ref is re-recorded.
func TestSHARecordedRef_RenameKeepsFullSHAUse(t *testing.T) {
	const (
		oldName = "krzema12/github-actions-typing"
		newName = "typesafegithub/github-actions-typing"
	)
	reg := &httpmock.Registry{}
	for range 3 {
		reg.Register(httpmock.GraphQL(`commit: object\(oid`), renamedParity)
		reg.Register(httpmock.GraphQLForRepo("krzema12", "github-actions-typing"), httpmock.JSONResponse(map[string]any{
			"data": map[string]any{"a0": withRepoID(testRepoResponse(newName, parityTestSHA, nodeActionYAML), 1)},
		}))
		for _, nwo := range []string{oldName, newName} {
			reg.Register(httpmock.REST("GET", `^/repos/`+nwo+`$`), httpmock.JSONResponse(map[string]any{
				"full_name": newName, "id": 1, "default_branch": "main", "owner": map[string]any{"id": 42},
			}))
			reg.Register(httpmock.REST("GET", `repos/`+nwo+`/tags`), httpmock.JSONResponse(httpmock.TagListResponse("v2.2.2", parityTestSHA)))
			reg.Register(httpmock.REST("GET", `repos/`+nwo+`/releases`), httpmock.JSONResponse([]map[string]any{}))
			reg.Register(httpmock.REST("GET", `repos/`+nwo+`/git/ref/heads/main`),
				httpmock.JSONResponse(map[string]any{"ref": "refs/heads/main", "object": map[string]any{"sha": parityTestSHA, "type": "commit"}}))
			reg.Register(httpmock.REST("GET", `repos/`+nwo+`/branches`), httpmock.JSONResponse(httpmock.BranchListResponse("main", parityTestSHA)))
		}
	}
	httpmock.NoTagsNamed(reg, "typesafegithub", "github-actions-typing")
	isolateRunLogs(t)
	path := writeTempWorkflow(t, strings.Replace(transferWorkflow, "@v2.2.2", "@"+parityTestSHA, 1),
		oldName+"@"+parityTestSHA+"=sha1-"+parityTestSHA)

	_, stderr, err := runCommandWithHTTP(t, parityFallback{reg}, "--no-interactive", path)
	require.NoError(t, err, stderr)
	wf, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(wf), "uses: "+newName+"@"+parityTestSHA)
	lock := readTempLockfilePins(t)
	assert.Contains(t, lock, "'"+newName+"@"+parityTestSHA+"':\n        ref: 'v2.2.2'")
	assert.NotContains(t, lock, "krzema12")
}

// TestParity_AnswerSettlesFailedRefMovedCheck: a ref-moved comparison that
// fails and then succeeds when parity asks the same question settles as
// the ref-moved warning, so --relock still re-pins the entry. Any other
// check succeeding would leave the failure blocking.
func TestParity_AnswerSettlesFailedRefMovedCheck(t *testing.T) {
	const pinned, live = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	reg := &httpmock.Registry{}
	reg.Register(httpmock.GraphQL(`commit: object\(oid`), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{
			"a0": map[string]any{"nameWithOwner": "example/action", "commit": map[string]any{"oid": pinned}, "tag": map[string]any{"oid": live}, "tagRef": map[string]any{}},
			"a1": map[string]any{"nameWithOwner": "example/action", "commit": map[string]any{"oid": live}, "tag": map[string]any{"oid": live}, "tagRef": map[string]any{}},
		},
	}))
	reg.Register(httpmock.GraphQLForRepo("example", "action"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": testRepoResponse("example/action", live, nodeActionYAML)},
	}))
	reg.Register(httpmock.REST("GET", "repos/example/action/compare/"), httpmock.StatusResponse(http.StatusServiceUnavailable))
	reg.Register(httpmock.REST("GET", "repos/example/action/compare/"), httpmock.JSONResponse(map[string]any{
		"status": "ahead", "merge_base_commit": map[string]any{"sha": pinned},
	}))
	path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: example/action@v1
`, "example/action@v1=sha1-"+pinned)

	p, err := runParity(t, reg, "--relock", "--no-fix", path)
	require.NoError(t, err, "%+v", p.Findings)
	reg.Verify(t)
	var categories []string
	for _, f := range p.Findings {
		categories = append(categories, f.Category)
	}
	assert.Equal(t, []string{"ref-moved"}, categories)
}
