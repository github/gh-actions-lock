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
	lockstore "github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/pinpool"
	"github.com/github/gh-actions-lock/internal/resolve"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckCommand_JSONWithHTTPMocks(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	reg.Register(
		httpmock.GraphQLForRepo("actions", "checkout"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/checkout", "de0fac2e4500dabe0009e67214ff5f5447ce83dd", nodeActionYAML),
			},
		}),
	)
	reg.Register(
		httpmock.GraphQLForRepo("actions", "setup-go"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/setup-go", "4a3601121dd01d1626a1e23e37211e3254c1c06c", nodeActionYAML),
			},
		}),
	)

	workflowPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
      - uses: actions/setup-go@v6
`,
		"actions/checkout@v6=sha1-de0fac2e4500dabe0009e67214ff5f5447ce83dd",
		"actions/setup-go@v6=sha1-4a3601121dd01d1626a1e23e37211e3254c1c06c",
	)

	stdout, _, err := runCommandWithHTTP(t, reg,
		"--rescan", "--no-fix", "--json=valid,findings", workflowPath,
	)
	require.NoError(t, err)

	var payload struct {
		Valid    bool             `json:"valid"`
		Findings []format.Finding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.True(t, payload.Valid)
	assert.Empty(t, payload.Findings)
}

func TestCheckCommand_RewritesMovedRepository(t *testing.T) {
	const (
		oldNWO = "krzema12/github-actions-typing"
		newNWO = "typesafegithub/github-actions-typing"
		ref    = "v2.2.2"
		sha    = "9ddf35b71a482be7d8922b28e8d00df16b77e315"
	)
	for _, tt := range []struct {
		name string
		ref  string
		pins []string
		args []string
	}{
		{name: "fresh onboarding", ref: ref},
		{
			name: "existing immutable lockfile",
			ref:  ref,
			pins: []string{oldNWO + "@" + ref + "=sha1-" + sha},
		},
		{
			name: "existing mutable lockfile",
			ref:  "v2",
			pins: []string{oldNWO + "@v2=sha1-" + sha},
		},
		{
			name: "rescan",
			ref:  ref,
			pins: []string{oldNWO + "@" + ref + "=sha1-" + sha},
			args: []string{"--rescan"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reg := &httpmock.Registry{}
			defer reg.Verify(t)
			reg.Register(
				httpmock.GraphQLForRepo("krzema12", "github-actions-typing"),
				httpmock.JSONResponse(map[string]any{
					"data": map[string]any{
						"a0": testRepoResponse(newNWO, sha, nodeActionYAML),
					},
				}),
			)
			if tt.name == "existing mutable lockfile" {
				reg.Register(
					httpmock.REST("GET", `repos/krzema12/github-actions-typing$`),
					httpmock.JSONResponse(map[string]any{
						"full_name": newNWO,
						"id":        502427408,
						"owner":     map[string]any{"id": 129620060},
					}),
				)
			}
			reg.Register(
				httpmock.REST("GET", `repos/typesafegithub/github-actions-typing$`),
				httpmock.JSONResponse(map[string]any{
					"full_name": newNWO,
					"id":        502427408,
					"owner":     map[string]any{"id": 129620060},
				}),
			)
			workflowPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: `+oldNWO+`@`+tt.ref+`
`, tt.pins...)
			args := append(tt.args, "--no-narrow", workflowPath)
			stdout, stderr, err := runCommandWithHTTP(t, reg, args...)

			require.NoError(t, err, "stdout:\n%s", stdout)
			assert.Contains(t, stdout+stderr, "Action repository transferred: "+oldNWO+" → "+newNWO)
			workflow, readErr := os.ReadFile(workflowPath)
			require.NoError(t, readErr)
			assert.Contains(t, string(workflow), "uses: "+newNWO+"@"+tt.ref)
			assert.NotContains(t, string(workflow), oldNWO)
			pins := readTempLockfilePins(t)
			assert.Contains(t, pins, "'"+newNWO+"@"+tt.ref+"'")
			assert.NotContains(t, pins, oldNWO)

			localReg := &httpmock.Registry{}
			_, _, verifyErr := runCommandWithHTTP(t, localReg, "--verify-local", workflowPath)
			require.NoError(t, verifyErr)
			localReg.Verify(t)
		})
	}
}

func TestCheckCommand_RejectsReplacedRepository(t *testing.T) {
	const (
		nwo = "owner/action"
		ref = "v1"
		sha = "1111111111111111111111111111111111111111"
	)
	for _, tt := range []struct {
		name string
		args []string
	}{
		{name: "default"},
		{name: "verify", args: []string{"--verify"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reg := &httpmock.Registry{}
			defer reg.Verify(t)
			reg.Register(
				httpmock.REST("GET", `repos/owner/action$`),
				httpmock.JSONResponse(map[string]any{
					"full_name": nwo,
					"id":        2,
					"owner":     map[string]any{"id": 1},
				}),
			)
			reg.Register(
				httpmock.GraphQLForRepo("owner", "action"),
				httpmock.JSONResponse(map[string]any{
					"data": map[string]any{
						"a0": testRepoResponse(nwo, sha, nodeActionYAML),
					},
				}),
			)
			workflowPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: `+nwo+`@`+ref+`
`, nwo+"@"+ref+"=sha1-"+sha)
			workflowBefore, err := os.ReadFile(workflowPath)
			require.NoError(t, err)
			lockPath := filepath.Join(".github", "workflows", "actions.lock")
			lockBefore, err := os.ReadFile(lockPath)
			require.NoError(t, err)

			args := append(tt.args, "--no-narrow", workflowPath)
			stdout, stderr, err := runCommandWithHTTP(t, reg, args...)

			require.Error(t, err)
			output := stdout + stderr + err.Error()
			assert.Contains(t, output, "repository identity changed for "+nwo+": the lockfile records repository ID 1, but the current repository ID is 2")
			assert.Contains(t, output, "This may indicate a namespace takeover")
			assert.Contains(t, output, "review "+nwo+" before trusting it")
			workflowAfter, readErr := os.ReadFile(workflowPath)
			require.NoError(t, readErr)
			assert.Equal(t, workflowBefore, workflowAfter)
			lockAfter, readErr := os.ReadFile(lockPath)
			require.NoError(t, readErr)
			assert.Equal(t, lockBefore, lockAfter)
		})
	}
}

func TestCheckCommand_PrefersLiveMovedRepositoryOverSeededAlias(t *testing.T) {
	const (
		oldNWO   = "old/action"
		newNWO   = "new/action"
		oldSHA   = "1111111111111111111111111111111111111111"
		liveSHA  = "2222222222222222222222222222222222222222"
		childSHA = "3333333333333333333333333333333333333333"
	)
	for _, tt := range []struct {
		name string
		refs []string
	}{
		{name: "seeded alias first", refs: []string{newNWO + "@v1", oldNWO + "@v1"}},
		{name: "live redirect first", refs: []string{oldNWO + "@v1", newNWO + "@v1"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reg := &httpmock.Registry{}
			defer reg.Verify(t)
			reg.Register(
				httpmock.REST("GET", `repos/new/action$`),
				httpmock.JSONResponse(map[string]any{
					"full_name": newNWO,
					"id":        1,
					"owner":     map[string]any{"id": 1},
				}),
			)
			reg.Register(
				httpmock.GraphQLForRepo("old", "action"),
				httpmock.JSONResponse(map[string]any{
					"data": map[string]any{
						"a0": testRepoResponse(newNWO, liveSHA, "runs:\n  using: composite\n  steps:\n    - uses: child/action@v1\n"),
					},
				}),
			)
			reg.Register(
				httpmock.GraphQLForRepo("child", "action"),
				httpmock.JSONResponse(map[string]any{
					"data": map[string]any{
						"a0": testRepoResponse("child/action", childSHA, nodeActionYAML),
					},
				}),
			)
			reg.Register(
				httpmock.REST("GET", `repos/child/action$`),
				httpmock.JSONResponse(map[string]any{
					"full_name": "child/action",
					"id":        4,
					"owner":     map[string]any{"id": 3},
				}),
			)
			workflowPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: `+strings.Join(tt.refs, `
      - uses: `)+`
`, newNWO+"@v1=sha1-"+oldSHA)

			stdout, _, err := runCommandWithHTTP(t, reg, "--no-narrow", workflowPath)

			require.NoError(t, err, "stdout:\n%s", stdout)
			store, loadErr := lockstore.LoadState(".", nil)
			require.NoError(t, loadErr)
			file := store.File()
			action, ok := file.Dependencies[newNWO+"@v1"]
			require.True(t, ok)
			assert.Equal(t, "sha1-"+liveSHA, action.Commit)
			assert.Equal(t, []string{"child/action@v1"}, action.Uses)
			workflow, readErr := os.ReadFile(workflowPath)
			require.NoError(t, readErr)
			assert.NotContains(t, string(workflow), oldNWO)
		})
	}
}

func TestCheckCommand_VerifyRejectsMovedRepositoryInExistingLockfile(t *testing.T) {
	const (
		oldNWO = "krzema12/github-actions-typing"
		newNWO = "typesafegithub/github-actions-typing"
		ref    = "v2.2.2"
		sha    = "9ddf35b71a482be7d8922b28e8d00df16b77e315"
	)
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(
		httpmock.GraphQLForRepo("krzema12", "github-actions-typing"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse(newNWO, sha, nodeActionYAML),
			},
		}),
	)
	workflowPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: `+oldNWO+`@`+ref+`
`, oldNWO+"@"+ref+"=sha1-"+sha)
	workflowBefore, readErr := os.ReadFile(workflowPath)
	require.NoError(t, readErr)
	lockPath := filepath.Join(".github", "workflows", "actions.lock")
	lockBefore, readErr := os.ReadFile(lockPath)
	require.NoError(t, readErr)

	stdout, stderr, err := runCommandWithHTTP(t, reg, "--verify", "--no-narrow", workflowPath)

	require.Error(t, err)
	assert.Contains(t, stdout+stderr, "repository "+oldNWO+" has been renamed or transferred to "+newNWO)
	workflowAfter, readErr := os.ReadFile(workflowPath)
	require.NoError(t, readErr)
	assert.Equal(t, workflowBefore, workflowAfter)
	lockAfter, readErr := os.ReadFile(lockPath)
	require.NoError(t, readErr)
	assert.Equal(t, lockBefore, lockAfter)
}

func TestCheckCommand_VerifyRejectsKnownMoveWhenResolutionFails(t *testing.T) {
	const (
		oldNWO = "old/action"
		newNWO = "new/action"
		ref    = "v1"
		sha    = "1111111111111111111111111111111111111111"
	)
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(
		httpmock.REST("GET", `repos/old/action$`),
		httpmock.JSONResponse(map[string]any{"full_name": newNWO}),
	)
	reg.Register(
		httpmock.GraphQLForRepo("old", "action"),
		httpmock.StatusResponse(http.StatusInternalServerError),
	)
	workflowPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: `+oldNWO+`@`+ref+`
`, oldNWO+"@"+ref+"=sha1-"+sha)

	stdout, _, err := runCommandWithHTTP(t, reg, "--verify", "--json=valid,findings", workflowPath)

	require.ErrorIs(t, err, errSilent)
	var payload struct {
		Valid    bool             `json:"valid"`
		Findings []format.Finding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.False(t, payload.Valid)
	require.Len(t, payload.Findings, 2)
	assert.Equal(t, "reachability-unknown", payload.Findings[0].Category)
	assert.Equal(t, "ref-changed", payload.Findings[1].Category)
	assert.Contains(t, payload.Findings[1].Detail, oldNWO+" has been renamed or transferred to "+newNWO)
}

func TestCheckCommand_FixRejectsKnownMoveWhenResolutionFails(t *testing.T) {
	const (
		oldNWO = "old/action"
		newNWO = "new/action"
		ref    = "v1"
		sha    = "1111111111111111111111111111111111111111"
	)
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(
		httpmock.REST("GET", `repos/old/action$`),
		httpmock.JSONResponse(map[string]any{"full_name": newNWO}),
	)
	for range 2 {
		reg.Register(
			httpmock.GraphQLForRepo("old", "action"),
			httpmock.StatusResponse(http.StatusInternalServerError),
		)
	}
	workflowPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: `+oldNWO+`@`+ref+`
`, oldNWO+"@"+ref+"=sha1-"+sha)
	workflowBefore, readErr := os.ReadFile(workflowPath)
	require.NoError(t, readErr)
	lockPath := filepath.Join(".github", "workflows", "actions.lock")
	lockBefore, readErr := os.ReadFile(lockPath)
	require.NoError(t, readErr)

	stdout, stderr, err := runCommandWithHTTP(t, reg, "--no-narrow", workflowPath)

	require.Error(t, err)
	require.ErrorContains(t, err, "resolving transferred repository")
	assert.NotContains(t, stdout+stderr, "All workflows valid")
	workflowAfter, readErr := os.ReadFile(workflowPath)
	require.NoError(t, readErr)
	assert.Equal(t, workflowBefore, workflowAfter)
	lockAfter, readErr := os.ReadFile(lockPath)
	require.NoError(t, readErr)
	assert.Equal(t, lockBefore, lockAfter)
}

func TestCheckCommand_RejectsAnchoredMovedRepositoryRewrite(t *testing.T) {
	const (
		oldNWO = "krzema12/github-actions-typing"
		newNWO = "typesafegithub/github-actions-typing"
		ref    = "v2.2.2"
		sha    = "9ddf35b71a482be7d8922b28e8d00df16b77e315"
	)
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(
		httpmock.GraphQLForRepo("krzema12", "github-actions-typing"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse(newNWO, sha, nodeActionYAML),
			},
		}),
	)
	workflowPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - &typing
        uses: `+oldNWO+`@`+ref+`
      - *typing
`)
	before, readErr := os.ReadFile(workflowPath)
	require.NoError(t, readErr)

	_, _, err := runCommandWithHTTP(t, reg, "--no-narrow", workflowPath)

	require.ErrorContains(t, err, "cannot update an anchored or aliased `uses:` value")
	after, readErr := os.ReadFile(workflowPath)
	require.NoError(t, readErr)
	assert.Equal(t, string(before), string(after))
	_, statErr := os.Stat(filepath.Join(".github", "workflows", "actions.lock"))
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestCheckCommand_RejectsMovedRepositoryInRemoteComposite(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(
		httpmock.GraphQLForRepo("root", "composite"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("root/composite", strings.Repeat("a", 40), "runs:\n  using: composite\n  steps:\n    - uses: old/action@v1\n"),
			},
		}),
	)
	reg.Register(
		httpmock.GraphQLForRepo("old", "action"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("new/action", strings.Repeat("b", 40), nodeActionYAML),
			},
		}),
	)
	workflowPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: root/composite@v1
`)

	_, _, err := runCommandWithHTTP(t, reg, "--no-narrow", workflowPath)

	var transferred *resolve.TransferredRepositoryError
	require.ErrorAs(t, err, &transferred)
	assert.Equal(t, "old/action", transferred.Original)
	assert.Equal(t, "new/action", transferred.Canonical)
	assert.Equal(t, "root/composite@v1", transferred.Parent)
	_, statErr := os.Stat(filepath.Join(".github", "workflows", "actions.lock"))
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestCheckCommand_RejectsTransferredRecordedRemoteCompositeRef(t *testing.T) {
	const (
		parentNWO = "root/composite"
		oldNWO    = "old/action"
		newNWO    = "new/action"
		parentSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		childSHA  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(
		httpmock.REST("GET", `repos/root/composite$`),
		httpmock.JSONResponse(map[string]any{"full_name": parentNWO}),
	)
	reg.Register(
		httpmock.REST("GET", `repos/old/action$`),
		httpmock.JSONResponse(map[string]any{"full_name": newNWO}),
	)
	reg.Register(
		httpmock.GraphQLForRepo("root", "composite"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse(parentNWO, parentSHA, "runs:\n  using: composite\n  steps:\n    - uses: "+oldNWO+"@v1\n"),
			},
		}),
	)
	reg.Register(
		httpmock.GraphQLForRepo("old", "action"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse(newNWO, childSHA, nodeActionYAML),
			},
		}),
	)

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755))
	workflowPath := filepath.Join(dir, ".github", "workflows", "ci.yml")
	workflow := "name: ci\non: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: " + parentNWO + "@v1\n"
	require.NoError(t, os.WriteFile(workflowPath, []byte(workflow), 0o600))
	lockPath := filepath.Join(dir, ".github", "workflows", "actions.lock")
	lockYAML := "version: '" + parserlock.Version + "'\ndependencies:\n" +
		"  '" + parentNWO + "@v1':\n" +
		"    ref: 'v1'\n    commit: 'sha1-" + parentSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"    uses:\n      - '" + oldNWO + "@v1'\n" +
		"  '" + oldNWO + "@v1':\n" +
		"    ref: 'v1'\n    commit: 'sha1-" + childSHA + "'\n    owner_id: 2\n    repo_id: 2\n" +
		"workflows:\n  '.github/workflows/ci.yml':\n    - '" + parentNWO + "@v1'\n"
	require.NoError(t, os.WriteFile(lockPath, []byte(lockYAML), 0o600))
	t.Chdir(dir)
	workflowArg := ".github/workflows/ci.yml"

	workflowBefore, err := os.ReadFile(workflowPath)
	require.NoError(t, err)
	lockBefore, err := os.ReadFile(lockPath)
	require.NoError(t, err)

	stdout, stderr, err := runCommandWithHTTP(t, reg, "--no-narrow", workflowArg)

	require.Error(t, err)
	require.ErrorContains(t, err, oldNWO+" has been renamed or transferred to "+newNWO)
	require.ErrorContains(t, err, "upstream composite "+parentNWO+"@v1")
	assert.NotContains(t, stdout+stderr, "All workflows valid")
	workflowAfter, readErr := os.ReadFile(workflowPath)
	require.NoError(t, readErr)
	assert.Equal(t, workflowBefore, workflowAfter)
	lockAfter, readErr := os.ReadFile(lockPath)
	require.NoError(t, readErr)
	assert.Equal(t, lockBefore, lockAfter)
}

func TestCheck_BareSHAUsesExactMajorTagAndVerifiesLocally(t *testing.T) {
	const (
		sha         = "b6e2e70617bc3265edd6dab6c906732b2f1ae151"
		ancestorSHA = "09f2f74827fd0000000000000000000000000000"
	)

	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.GraphQLForRepo("dawidd6", "action-download-artifact"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("dawidd6/action-download-artifact", sha, nodeActionYAML),
			},
		}),
	)
	reg.Register(
		httpmock.REST("GET", `repos/dawidd6/action-download-artifact$`),
		httpmock.JSONResponse(map[string]any{
			"default_branch": "main",
			"id":             2,
			"owner":          map[string]any{"id": 1},
		}),
	)
	reg.Register(
		httpmock.REST("GET", `repos/dawidd6/action-download-artifact/git/ref/heads/main`),
		httpmock.JSONResponse(map[string]any{
			"ref": "refs/heads/main", "object": map[string]any{"sha": sha, "type": "commit"},
		}),
	)
	reg.Register(
		httpmock.REST("GET", `repos/dawidd6/action-download-artifact/tags`),
		httpmock.JSONResponse(httpmock.TagListResponse(
			"v21", sha,
			"v3.1.4", ancestorSHA,
		)),
	)
	reg.Register(
		httpmock.REST("GET", `repos/dawidd6/action-download-artifact/releases`),
		httpmock.JSONResponse([]map[string]any{}),
	)
	reg.Register(
		httpmock.REST("GET", `repos/dawidd6/action-download-artifact/compare/09f2f74827fd0000000000000000000000000000\.\.\.b6e2e70617bc3265edd6dab6c906732b2f1ae151`),
		httpmock.JSONResponse(httpmock.CompareAncestorResponse(ancestorSHA)),
	)

	workflowPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: dawidd6/action-download-artifact@b6e2e70617bc3265edd6dab6c906732b2f1ae151
`)

	_, _, err := runCommandWithHTTP(t, reg, workflowPath)
	require.NoError(t, err)

	workflow, err := os.ReadFile(workflowPath)
	require.NoError(t, err)
	assert.Contains(t, string(workflow), "dawidd6/action-download-artifact@v21")
	assert.NotContains(t, string(workflow), "@v3.1.4")

	lock := readTempLockfilePins(t)
	assert.Contains(t, lock, "'dawidd6/action-download-artifact@v21':")
	assert.Contains(t, lock, "ref: 'v21'")
	assert.Contains(t, lock, "commit: 'sha1-"+sha+"'")
	assert.NotContains(t, lock, "v3.1.4")

	_, _, err = runCommandWithHTTP(t, &httpmock.Registry{}, "--verify-local", workflowPath)
	require.NoError(t, err)
}

func TestCheck_ChangedTagAtSameCommitRekeysAndVerifiesLocally(t *testing.T) {
	const sha = "94de994a9f6fffee200243214e17002e2920bb59"

	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.GraphQLForRepo("dawidd6", "action-send-mail"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("dawidd6/action-send-mail", sha, nodeActionYAML),
			},
		}),
	)
	reg.Register(
		httpmock.REST("GET", `repos/dawidd6/action-send-mail/tags`),
		httpmock.JSONResponse(httpmock.TagListResponse("v18", sha, "v3.12.0", sha)),
	)
	reg.Register(
		httpmock.REST("GET", `repos/dawidd6/action-send-mail/releases`),
		httpmock.JSONResponse([]map[string]any{}),
	)

	workflowPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: dawidd6/action-send-mail@v18
`, "dawidd6/action-send-mail@v3.12.0=sha1-"+sha)

	_, _, err := runCommandWithHTTP(t, reg, workflowPath)
	require.NoError(t, err)

	lock := readTempLockfilePins(t)
	assert.Contains(t, lock, "'dawidd6/action-send-mail@v18':")
	assert.Contains(t, lock, "ref: 'v18'")
	assert.Contains(t, lock, "commit: 'sha1-"+sha+"'")
	assert.NotContains(t, lock, "v3.12.0")

	_, _, err = runCommandWithHTTP(t, &httpmock.Registry{}, "--verify-local", workflowPath)
	require.NoError(t, err)
}

const nodeActionYAML = "name: Test Action\nruns:\n  using: node20\n"

func testRepoResponse(nameWithOwner, oid, actionYAML string) map[string]any {
	return map[string]any{
		"nameWithOwner": nameWithOwner,
		"object": map[string]any{
			"oid": oid,
			"file": map[string]any{
				"object": map[string]any{
					"text": actionYAML,
				},
			},
			"fileYaml": nil,
		},
	}
}

// writeTempWorkflow writes a workflow YAML body to a scratch repo at
// .github/workflows/workflow.yml and (if pins are provided) materializes the
// detached lockfile at .github/workflows/actions.lock. The fixture body must
// NOT carry an embedded `dependencies:` block — pin metadata lives in the
// detached lockfile per the per-repo dependency-pinning architecture.
// Returns the relative workflow path; the test's working directory is
// chdir'd to the scratch repo.
func writeTempWorkflow(t *testing.T, body string, pins ...string) string {
	t.Helper()

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755))

	body = strings.TrimSpace(body) + "\n"
	if strings.Contains(body, "\ndependencies:") {
		t.Fatalf("writeTempWorkflow: workflow body contains embedded `dependencies:` block; pass pins as the variadic argument instead")
	}

	wfRel := filepath.Join(".github", "workflows", "workflow.yml")
	wfPath := filepath.Join(dir, wfRel)
	require.NoError(t, os.WriteFile(wfPath, []byte(body), 0o600))

	if len(pins) > 0 {
		writeTempLockfile(t, dir, "workflow.yml", pins)
	}

	t.Chdir(dir)
	return filepath.ToSlash(wfRel)
}

// writeTempLockfile writes a minimal lockfile for the given pins. Each
// pinString has the form "owner/repo@ref" (the v0.0.2 pin key format).
// A synthetic commit hash is generated from the pin key for each entry.
// The ref field in each action body matches the pin key's ref. Overrides
// can pass "owner/repo@ref=sha1-hex" to supply an explicit commit.
func writeTempLockfile(t *testing.T, repoDir, wfName string, pinStrings []string) {
	t.Helper()
	var sb strings.Builder
	sb.WriteString("version: '" + parserlock.Version + "'\ndependencies:\n")
	for _, raw := range pinStrings {
		key, commit := raw, ""
		if idx := strings.Index(raw, "="); idx >= 0 {
			key = raw[:idx]
			commit = raw[idx+1:]
		}
		pin, ok := parserlock.ParsePin(key)
		if !ok {
			t.Fatalf("writeTempLockfile: invalid pin %q", key)
		}
		if commit == "" {
			// Generate deterministic fake commit from pin key.
			commit = "sha1-" + strings.Repeat(string("abcdef0123456789"[len(key)%16]), 40)
		}
		sb.WriteString("  '" + key + "':\n    ref: '" + pin.Ref + "'\n    commit: '" + commit + "'\n    owner_id: 1\n    repo_id: 1\n")
	}
	sb.WriteString("workflows:\n  '.github/workflows/" + wfName + "':\n")
	for _, raw := range pinStrings {
		key := raw
		if idx := strings.Index(raw, "="); idx >= 0 {
			key = raw[:idx]
		}
		sb.WriteString("    - '" + key + "'\n")
	}
	p := filepath.Join(repoDir, ".github", "workflows", "actions.lock")
	require.NoError(t, os.WriteFile(p, []byte(sb.String()), 0o600))
}

// readTempLockfilePins returns the raw pin strings from the actions.lock file
// in the current working directory. Useful for assertions in write/upgrade
// tests that previously inspected the workflow YAML directly.
func readTempLockfilePins(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(".github", "workflows", "actions.lock"))
	require.NoError(t, err)
	return string(b)
}

func runCommandWithHTTP(t *testing.T, rt http.RoundTripper, args ...string) (string, string, error) {
	t.Helper()

	stdoutR, stdoutW, err := os.Pipe()
	require.NoError(t, err)
	stderrR, stderrW, err := os.Pipe()
	require.NoError(t, err)

	newResolver := func(hostname string, pool *pinpool.Pool) (*resolve.Resolver, error) {
		return resolve.New(hostname, pool, resolve.WithTransport(rt))
	}

	cmd := newRootCmd(newResolver)
	cmd.SetOut(stdoutW)
	cmd.SetErr(stderrW)
	cmd.SetArgs(args)
	runErr := cmd.Execute()

	_ = stdoutW.Close()
	_ = stderrW.Close()

	stdoutBytes, readErr := io.ReadAll(stdoutR)
	require.NoError(t, readErr)
	stderrBytes, readErr := io.ReadAll(stderrR)
	require.NoError(t, readErr)

	return string(stdoutBytes), string(stderrBytes), runErr
}

// ==========================================================================
// Supply Chain Attack Reachability Tests
//
// These tests model real-world attacks where tag mutation or fork-network
// injection was used to compromise GitHub Actions. The reachability check
// should catch cases where a pinned SHA exists in the GitHub fork network
// but is NOT on the canonical repository's ref lineage.
//
// References:
//   - tj-actions/changed-files (CVE-2025-30066): tag v44 pointed to malicious commit from fork
//   - reviewdog/action-setup: tag mutation via compromised PAT
//   - xygeni/xygeni-action: C2 reverse shell backdoor via tag poisoning
//   - aquasecurity/trivy-action: scanner-to-stealer tag manipulation
// ==========================================================================

// TestCheck_TjActionsChangedFiles_TagMutationAttack models the March 2025
// tj-actions/changed-files attack (CVE-2025-30066) where attackers
// compromised a maintainer PAT and force-pushed tag v44 to a malicious
// commit. The malicious commit is NOT reachable from the legitimate tag.
// TestCheck_TamperedAndUnreachable verifies that when a pinned SHA differs
// from live resolution AND the old SHA is unreachable, both errors are reported.
// TestCheck_Reachable verifies the happy path: pinned SHA matches live
// resolution and is reachable — validation passes with no errors or warnings.
func TestCheck_Reachable(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	sha := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"

	reg.Register(
		httpmock.GraphQLForRepo("example", "action"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("example/action", sha, nodeActionYAML),
			},
		}),
	)
	workflowPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: example/action@v1
`,
		"example/action@v1=sha1-"+sha,
	)

	stdout, _, err := runCommandWithHTTP(t, reg,
		"--rescan", "--no-fix", "--json=valid,findings", workflowPath,
	)
	require.NoError(t, err)

	var payload struct {
		Valid    bool             `json:"valid"`
		Findings []format.Finding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.True(t, payload.Valid)
	assert.Empty(t, payload.Findings)
}

// ==========================================================================
// Lockfile Forgery Detection Tests
//
// These tests verify that the ancestry check promotes ref-moved to
// unreachable-pin when the pinned SHA is not an ancestor of the live SHA.
// This detects cases where someone manually injected a SHA into the lockfile
// that was never part of the ref's legitimate history.
// ==========================================================================

// TestCheck_UnreachablePin_NotAncestor verifies that when the Compare API
// shows the pinned SHA is NOT an ancestor of the live SHA, the finding is
// promoted from ref-moved to unreachable-pin.
func TestCheck_UnreachablePin_NotAncestor(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	pinnedSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	liveSHA := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	// GraphQL resolution returns the live SHA (different from pinned).
	reg.Register(
		httpmock.GraphQLForRepo("example", "action"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("example/action", liveSHA, nodeActionYAML),
			},
		}),
	)

	// Compare API: merge_base ≠ pinnedSHA → not an ancestor.
	reg.Register(
		httpmock.REST("GET", "repos/example/action/compare/"),
		httpmock.JSONResponse(map[string]any{
			"status": "diverged",
			"merge_base_commit": map[string]any{
				"sha": "cccccccccccccccccccccccccccccccccccccccc",
			},
		}),
	)

	workflowPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: example/action@v1
`,
		"example/action@v1=sha1-"+pinnedSHA,
	)

	stdout, _, err := runCommandWithHTTP(t, reg,
		"--rescan", "--no-fix", "--json=valid,findings", workflowPath,
	)
	require.ErrorIs(t, err, errSilent, "JSON mode should exit non-zero for forgery findings")

	var payload struct {
		Valid    bool             `json:"valid"`
		Findings []format.Finding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.False(t, payload.Valid)

	categories := map[string]bool{}
	for _, f := range payload.Findings {
		categories[f.Category] = true
	}
	assert.True(t, categories["unreachable-pin"], "should detect lockfile forgery: %+v", payload.Findings)
	assert.False(t, categories["ref-moved"], "should NOT have ref-moved (promoted to forgery): %+v", payload.Findings)
}

// TestCheck_UnreachablePin_LegitAncestor verifies that when the Compare API
// confirms the pinned SHA IS an ancestor of the live SHA, the finding stays
// as ref-moved (legitimate tag movement, not forgery).
func TestCheck_UnreachablePin_LegitAncestor(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	pinnedSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	liveSHA := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	reg.Register(
		httpmock.GraphQLForRepo("example", "action"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("example/action", liveSHA, nodeActionYAML),
			},
		}),
	)

	// Compare API: merge_base == pinnedSHA → legitimate ancestor.
	reg.Register(
		httpmock.REST("GET", "repos/example/action/compare/"),
		httpmock.JSONResponse(map[string]any{
			"status": "ahead",
			"merge_base_commit": map[string]any{
				"sha": pinnedSHA,
			},
		}),
	)

	workflowPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: example/action@v1
`,
		"example/action@v1=sha1-"+pinnedSHA,
	)

	stdout, _, err := runCommandWithHTTP(t, reg,
		"--rescan", "--no-fix", "--json=valid,findings", workflowPath,
	)
	require.NoError(t, err, "ref-moved is a warning, should not error")

	var payload struct {
		Valid    bool             `json:"valid"`
		Findings []format.Finding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.True(t, payload.Valid, "ref-moved is a warning, workflow is still valid")

	categories := map[string]bool{}
	for _, f := range payload.Findings {
		categories[f.Category] = true
	}
	assert.True(t, categories["ref-moved"], "should keep as ref-moved for legit ancestor: %+v", payload.Findings)
	assert.False(t, categories["unreachable-pin"], "should NOT have unreachable-pin: %+v", payload.Findings)
}

// TestCheck_UnreachablePin_RateLimited verifies that when the ancestry
// check is rate-limited, the finding surfaces as ancestry-unknown — not
// ref-moved (which would imply a benign-but-known move) and not
// unreachable-pin (which requires an authoritative not-ancestor answer).
func TestCheck_UnreachablePin_RateLimited(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	pinnedSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	liveSHA := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	reg.Register(
		httpmock.GraphQLForRepo("example", "action"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("example/action", liveSHA, nodeActionYAML),
			},
		}),
	)

	// Compare API: rate limited.
	reg.Register(
		httpmock.REST("GET", "repos/example/action/compare/"),
		httpmock.StatusResponse(429),
	)

	workflowPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: example/action@v1
`,
		"example/action@v1=sha1-"+pinnedSHA,
	)

	stdout, _, err := runCommandWithHTTP(t, reg,
		"--rescan", "--no-fix", "--json=valid,findings", workflowPath,
	)
	require.NoError(t, err, "ref-moved is a warning, should not error")

	var payload struct {
		Valid    bool             `json:"valid"`
		Findings []format.Finding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.True(t, payload.Valid, "ancestry-unknown is a warning, workflow is still valid")

	categories := map[string]bool{}
	for _, f := range payload.Findings {
		categories[f.Category] = true
	}
	assert.True(t, categories["ancestry-unknown"], "should classify as ancestry-unknown when rate limited: %+v", payload.Findings)
	assert.False(t, categories["unreachable-pin"], "should NOT have unreachable-pin when rate limited: %+v", payload.Findings)
	assert.False(t, categories["ref-moved"], "should NOT downgrade to ref-moved when rate limited: %+v", payload.Findings)
	assert.False(t, categories["valid"], "rate-limited ancestry must not regress to CategoryValid: %+v", payload.Findings)

	// Verify the detail mentions the inconclusive ancestry check.
	for _, f := range payload.Findings {
		if f.Category == "ancestry-unknown" {
			assert.Contains(t, f.Detail, "ancestry check inconclusive")
		}
	}
}

func TestCheckCommand_JSONDependenciesWithRequiredBy(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	compositeYAML := "name: Setup Go\nruns:\n  using: composite\n  steps:\n    - uses: actions/cache/save@v4\n"

	// Per-ref resolution queries (parallel resolver resolves one ref per worker).
	reg.Register(
		httpmock.GraphQLForRepo("actions", "checkout"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/checkout", "de0fac2e4500dabe0009e67214ff5f5447ce83dd", nodeActionYAML),
			},
		}),
	)
	reg.Register(
		httpmock.GraphQLForRepo("actions", "setup-go"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/setup-go", "d35c59abb061a4a6fb18e82ac0862c26744d6ab5", compositeYAML),
			},
		}),
	)
	// Transitive dep discovered from composite (cache/save).
	reg.Register(
		httpmock.GraphQLForRepo("actions", "cache"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/cache", "5a3ec84eff668545956fd18022155c47e93e2684", nodeActionYAML),
			},
		}),
	)

	workflowPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
      - uses: actions/setup-go@v6
`,
		"actions/checkout@v6",
		"actions/setup-go@v6",
		// Transitive dependency (via actions/setup-go@v6).
		"actions/cache@v4",
	)

	// Test per-workflow dependencies view
	stdout, _, err := runCommandWithHTTP(t, reg,
		"--rescan", "--no-fix", "--json=workflows", workflowPath,
	)
	require.NoError(t, err)

	var payload struct {
		Workflows []struct {
			Path         string              `json:"path"`
			Valid        bool                `json:"valid"`
			Dependencies []format.Dependency `json:"dependencies"`
		} `json:"workflows"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	require.Len(t, payload.Workflows, 1)

	wf := payload.Workflows[0]
	assert.True(t, wf.Valid)
	require.Len(t, wf.Dependencies, 3)

	// Find the transitive dep
	var transitiveDep *format.Dependency
	for i := range wf.Dependencies {
		if wf.Dependencies[i].NWO == "actions/cache" {
			transitiveDep = &wf.Dependencies[i]
			break
		}
	}
	require.NotNil(t, transitiveDep, "transitive dep actions/cache should be present")
	assert.False(t, transitiveDep.Direct)
	assert.Equal(t, []string{"actions/setup-go@v6"}, transitiveDep.RequiredBy)

	// Direct deps should NOT have required_by
	for _, d := range wf.Dependencies {
		if d.Direct {
			assert.Empty(t, d.RequiredBy, "direct dep %s should not have required_by", d.NWO)
		}
	}
}

func TestCheckCommand_JSONDependenciesInfersRequiredByWithoutComments(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	compositeYAML := "name: Setup Go\nruns:\n  using: composite\n  steps:\n    - uses: actions/cache/save@v4\n"

	reg.Register(
		httpmock.GraphQLForRepo("actions", "setup-go"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/setup-go", "d35c59abb061a4a6fb18e82ac0862c26744d6ab5", compositeYAML),
			},
		}),
	)
	reg.Register(
		httpmock.GraphQLForRepo("actions", "cache"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/cache", "5a3ec84eff668545956fd18022155c47e93e2684", nodeActionYAML),
			},
		}),
	)

	workflowPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/setup-go@v6
`,
		"actions/setup-go@v6",
		"actions/cache@v4",
	)

	stdout, _, err := runCommandWithHTTP(t, reg,
		"--rescan", "--no-fix", "--json=workflows", workflowPath,
	)
	require.NoError(t, err)

	var payload struct {
		Workflows []struct {
			Dependencies []format.Dependency `json:"dependencies"`
		} `json:"workflows"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	require.Len(t, payload.Workflows, 1)

	var transitiveDep *format.Dependency
	for i := range payload.Workflows[0].Dependencies {
		if payload.Workflows[0].Dependencies[i].NWO == "actions/cache" {
			transitiveDep = &payload.Workflows[0].Dependencies[i]
			break
		}
	}
	require.NotNil(t, transitiveDep, "transitive dep actions/cache should be present")
	assert.False(t, transitiveDep.Direct)
	assert.Equal(t, []string{"actions/setup-go@v6"}, transitiveDep.RequiredBy)
}

func TestCheckCommand_JSONDefaultFieldsExcludesDependencies(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	reg.Register(
		httpmock.GraphQLForRepo("actions", "checkout"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/checkout", "de0fac2e4500dabe0009e67214ff5f5447ce83dd", nodeActionYAML),
			},
		}),
	)

	workflowPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
`,
		"actions/checkout@v6",
	)

	// --json with no value should use the default fields (valid,findings,workflows)
	stdout, _, err := runCommandWithHTTP(t, reg,
		"--rescan", "--no-fix", "--json", workflowPath,
	)
	require.NoError(t, err)

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(stdout), &raw))

	// Default should include valid, findings, workflows
	assert.Contains(t, raw, "valid")
	assert.Contains(t, raw, "findings")
	assert.Contains(t, raw, "workflows")
	// Default should NOT include dependencies (opt-in only)
	assert.NotContains(t, raw, "dependencies", "default --json should not include top-level dependencies to avoid duplication with workflows")
}

// TestCheck_SeedFromLockfile_SkipsHTTPForCachedDeps verifies that
// SeedFromLockfile pre-warms the resolution cache so known deps skip
// action-file resolution, while new deps still resolve from the network.
// The workflow has two deps: checkout (in lockfile) and setup-go (not in
// lockfile). Checkout needs only one repository identity request.
func TestCheck_SeedFromLockfile_SkipsHTTPForCachedDeps(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	checkoutSHA := "de0fac2e4500dabe0009e67214ff5f5447ce83dd"
	setupGoSHA := "4a3601121dd01d1626a1e23e37211e3254c1c06c"

	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout$`),
		httpmock.JSONResponse(map[string]any{
			"full_name": "actions/checkout",
			"id":        1,
			"owner":     map[string]any{"id": 1},
		}),
	)
	// No GraphQL stub for checkout: after its NWO is validated, the seed
	// must still serve its action resolution from cache.
	reg.Register(
		httpmock.GraphQLForRepo("actions", "setup-go"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/setup-go", setupGoSHA, nodeActionYAML),
			},
		}),
	)

	// Workflow references both, but lockfile only has checkout.
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755))
	wfBody := strings.TrimSpace(`
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
      - uses: actions/setup-go@v6
`) + "\n"
	wfPath := filepath.Join(dir, ".github", "workflows", "workflow.yml")
	require.NoError(t, os.WriteFile(wfPath, []byte(wfBody), 0o600))

	// Lockfile records ONLY checkout — setup-go is "new".
	lockYAML := "version: '" + parserlock.Version + "'\ndependencies:\n" +
		"  'actions/checkout@v6" + "':\n" +
		"    ref: 'v6'\n    commit: 'sha1-" + checkoutSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"workflows:\n" +
		"  '.github/workflows/workflow.yml':\n" +
		"    - 'actions/checkout@v6" + "'\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".github", "workflows", "actions.lock"), []byte(lockYAML), 0o600))
	t.Chdir(dir)

	// Run WITHOUT --rescan so SeedFromLockfile is active.
	stdout, _, err := runCommandWithHTTP(t, reg,
		"--no-fix", "--json=valid,findings",
		".github/workflows/workflow.yml",
	)
	// setup-go is resolved but not yet pinned → "not-pinned" finding → errSilent.
	// That's expected: we're testing that checkout was served from cache, not
	// that the overall check passes.
	require.ErrorIs(t, err, errSilent)

	var payload struct {
		Valid    bool             `json:"valid"`
		Findings []format.Finding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))

	// The finding should be about setup-go being unpinned, NOT about checkout.
	// If checkout required GraphQL action resolution, no stub would match.
	require.Len(t, payload.Findings, 1)
	assert.Equal(t, "not-pinned", payload.Findings[0].Category)
	assert.Contains(t, payload.Findings[0].Dependency, "setup-go")
}

// TestCheck_NoFix_WritesNothing proves --no-fix is read-only: it reports
// findings and returns a non-zero exit, but never touches the workflow or the
// lockfile on disk. setup-go is unpinned (a blocking finding under --no-fix),
// so the run exits errSilent while leaving both files byte-for-byte unchanged.
func TestCheck_NoFix_WritesNothing(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	checkoutSHA := "de0fac2e4500dabe0009e67214ff5f5447ce83dd"
	setupGoSHA := "4a3601121dd01d1626a1e23e37211e3254c1c06c"

	// setup-go is the unpinned (new) dep — it resolves from the mock. checkout
	// is seeded from the lockfile and must not hit the network.
	reg.Register(
		httpmock.GraphQLForRepo("actions", "setup-go"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/setup-go", setupGoSHA, nodeActionYAML),
			},
		}),
	)

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755))
	wfBody := strings.TrimSpace(`
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
      - uses: actions/setup-go@v6
`) + "\n"
	wfPath := filepath.Join(dir, ".github", "workflows", "workflow.yml")
	require.NoError(t, os.WriteFile(wfPath, []byte(wfBody), 0o600))

	lockYAML := "version: '" + parserlock.Version + "'\ndependencies:\n" +
		"  'actions/checkout@v6" + "':\n" +
		"    ref: 'v6'\n    commit: 'sha1-" + checkoutSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"workflows:\n" +
		"  '.github/workflows/workflow.yml':\n" +
		"    - 'actions/checkout@v6" + "'\n"
	lockPath := filepath.Join(dir, ".github", "workflows", "actions.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(lockYAML), 0o600))
	t.Chdir(dir)

	wfBefore, err := os.ReadFile(wfPath)
	require.NoError(t, err)
	lockBefore, err := os.ReadFile(lockPath)
	require.NoError(t, err)

	// Terminal mode (no --json), read-only. setup-go is unpinned → !valid →
	// errSilent.
	_, _, runErr := runCommandWithHTTP(t, reg,
		"--no-fix", ".github/workflows/workflow.yml",
	)
	require.ErrorIs(t, runErr, errSilent)

	wfAfter, err := os.ReadFile(wfPath)
	require.NoError(t, err)
	lockAfter, err := os.ReadFile(lockPath)
	require.NoError(t, err)

	assert.Equal(t, string(wfBefore), string(wfAfter), "--no-fix must not modify the workflow")
	assert.Equal(t, string(lockBefore), string(lockAfter), "--no-fix must not modify the lockfile")
}

// TestCheck_DefaultJSON_AutofixWrites proves --json is a renderer, not a
// read-only switch: a bare --json run (no --no-fix) still autofixes. setup-go
// is unpinned but fixable, so the pipeline pins it, writes the lockfile, and
// exits 0 — while stdout carries the JSON render. The single dep uses a
// full-semver ref (v6.0.0), so neither tag narrowing nor reverse lookup needs
// the network; the remaining metadata stub covers the lockfile write.
func TestCheck_DefaultJSON_AutofixWrites(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	setupGoSHA := "4a3601121dd01d1626a1e23e37211e3254c1c06c"

	// Resolve setup-go@v6.0.0 → setupGoSHA.
	reg.Register(
		httpmock.GraphQLForRepo("actions", "setup-go"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/setup-go", setupGoSHA, nodeActionYAML),
			},
		}),
	)
	repoMeta := httpmock.JSONResponse(map[string]any{
		"id":    2,
		"owner": map[string]any{"id": 1},
	})
	reg.Register(httpmock.REST("GET", `repos/actions/setup-go$`), repoMeta)

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755))
	wfBody := strings.TrimSpace(`
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/setup-go@v6.0.0
`) + "\n"
	wfPath := filepath.Join(dir, ".github", "workflows", "workflow.yml")
	require.NoError(t, os.WriteFile(wfPath, []byte(wfBody), 0o600))
	t.Chdir(dir)

	// Bare --json: renderer only, autofix still runs.
	stdout, _, err := runCommandWithHTTP(t, reg,
		"--json=valid,findings",
		".github/workflows/workflow.yml",
	)
	require.NoError(t, err)

	// stdout is the JSON render — proves --json still selects the renderer.
	var payload struct {
		Valid    bool             `json:"valid"`
		Findings []format.Finding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))

	// The lockfile gained the setup-go pin — proves --json no longer implies
	// read-only.
	pins := readTempLockfilePins(t)
	assert.Contains(t, pins, "actions/setup-go", "autofix should have pinned setup-go")
}

// TestCheck_Rescan_DetectsRefMovementDespiteLockfile is a regression test
// ensuring --rescan does NOT seed the resolution cache. If seeding occurred,
// the resolver would return the stale lockfile SHA and the ref-moved finding
// would be suppressed — exactly the bug we fixed.
func TestCheck_Rescan_DetectsRefMovementDespiteLockfile(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	staleSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	liveSHA := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	// HTTP returns the LIVE SHA (different from what the lockfile recorded).
	reg.Register(
		httpmock.GraphQLForRepo("example", "action"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("example/action", liveSHA, nodeActionYAML),
			},
		}),
	)

	// Compare API: staleSHA is a legitimate ancestor of liveSHA (tag moved forward).
	reg.Register(
		httpmock.REST("GET", "repos/example/action/compare/"),
		httpmock.JSONResponse(map[string]any{
			"status": "ahead",
			"merge_base_commit": map[string]any{
				"sha": staleSHA,
			},
		}),
	)

	workflowPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: example/action@v1
`,
		"example/action@v1=sha1-"+staleSHA,
	)

	stdout, _, err := runCommandWithHTTP(t, reg,
		"--rescan", "--no-fix", "--json=valid,findings", workflowPath,
	)
	// ref-moved is a warning (valid=true), not an error.
	require.NoError(t, err, "ref-moved is a warning, should not error")

	var payload struct {
		Valid    bool             `json:"valid"`
		Findings []format.Finding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.True(t, payload.Valid, "ref-moved is a warning")

	hasRefMoved := false
	for _, f := range payload.Findings {
		if f.Category == "ref-moved" {
			hasRefMoved = true
		}
	}
	assert.True(t, hasRefMoved,
		"--rescan must detect ref movement (stale lockfile SHA vs live SHA); "+
			"if this fails, SeedFromLockfile is poisoning the resolution cache during rescan: %+v",
		payload.Findings)
}

func TestCheckCommand_JSONDeduplicatesDependencies(t *testing.T) {
	// When two workflow files share the same dep, top-level dependencies
	// should deduplicate them.
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	// No HTTP stub: both workflows are fully recorded in the lockfile, so
	// the fast path skips every network round-trip. The dedup logic under
	// test operates purely on the inventory built from disk.

	wf1 := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
`,
		"actions/checkout@v6",
	)

	wf2Path := filepath.Join(filepath.Dir(wf1), "workflow2.yml")
	require.NoError(t, os.WriteFile(wf2Path, []byte(strings.TrimSpace(`
name: deploy
on: push
jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
`)+"\n"), 0o600))

	// Add wf2's deps to the lockfile (writeTempWorkflow only seeded wf1).
	writeTempLockfile(t, ".", "workflow.yml",
		[]string{"actions/checkout@v6"})
	// Replace with a multi-workflow lockfile.
	lockYAML := "version: " + parserlock.Version + "\n" +
		"dependencies:\n" +
		"  actions/checkout@v6:\n" +
		"    ref: v6\n    commit: sha1-de0fac2e4500dabe0009e67214ff5f5447ce83dd\n    owner_id: 1\n    repo_id: 1\n" +
		"workflows:\n" +
		"  .github/workflows/workflow.yml:\n" +
		"    - actions/checkout@v6\n" +
		"  .github/workflows/workflow2.yml:\n" +
		"    - actions/checkout@v6\n"
	require.NoError(t, os.WriteFile(filepath.Join(".github", "workflows", "actions.lock"), []byte(lockYAML), 0o600))

	stdout, _, err := runCommandWithHTTP(t, reg,
		"--no-fix", "--json=dependencies", wf1, wf2Path,
	)
	require.NoError(t, err)

	var payload struct {
		Dependencies []format.Dependency `json:"dependencies"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))

	// Should be deduplicated — 1 dep, not 2
	assert.Len(t, payload.Dependencies, 1, "top-level dependencies should be deduplicated across workflows")
	assert.Equal(t, "actions/checkout", payload.Dependencies[0].NWO)
}

func TestRootCommand_JSONExpandsDirectoryArguments(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	dir := t.TempDir()
	first := filepath.Join(dir, "first.yml")
	second := filepath.Join(dir, "second.yaml")
	ignored := filepath.Join(dir, "notes.txt")
	workflow := []byte(strings.TrimSpace(`
name: run only
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: echo ok
`) + "\n")
	require.NoError(t, os.WriteFile(first, workflow, 0o600))
	require.NoError(t, os.WriteFile(second, workflow, 0o600))
	require.NoError(t, os.WriteFile(ignored, []byte("not a workflow\n"), 0o600))

	stdout, _, err := runCommandWithHTTP(t, reg, "--no-fix", "--json=valid,workflows", dir)
	require.NoError(t, err)

	var payload struct {
		Valid     bool `json:"valid"`
		Workflows []struct {
			Path  string `json:"path"`
			Valid bool   `json:"valid"`
		} `json:"workflows"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.True(t, payload.Valid)
	require.Len(t, payload.Workflows, 2)
	assert.Equal(t, first, payload.Workflows[0].Path)
	assert.Equal(t, second, payload.Workflows[1].Path)
}

func TestCheckCommand_JSONLoadErrorIsInvalid(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	missingPath := filepath.Join(t.TempDir(), "missing.yml")
	stdout, _, err := runCommandWithHTTP(t, reg, "--no-fix", "--json=valid,findings", missingPath)
	require.ErrorIs(t, err, errSilent)

	var payload struct {
		Valid    bool             `json:"valid"`
		Findings []format.Finding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.False(t, payload.Valid)
	require.Len(t, payload.Findings, 1)
	assert.Equal(t, "error", payload.Findings[0].Severity)
}

func TestCheckCommand_LoadErrorFailsFixMode(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
	}{
		{name: "migration enabled"},
		{name: "migration disabled", args: []string{"--no-migrate-local-actions"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reg := &httpmock.Registry{}
			defer reg.Verify(t)

			workflowPath := writeTempWorkflow(t, "name: [")
			args := append(tt.args, workflowPath)
			_, stderr, err := runCommandWithHTTP(t, reg, args...)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "parsing workflow YAML")
			assert.NotContains(t, stderr, "All 1 workflow valid")
		})
	}
}

// TestCheck_Relock_BumpsMovedBranchRef covers github/actions-dispatch#751:
// a branch ref (main) whose upstream head advanced is trusted as-is on a
// normal run and merely flagged ref-moved under --rescan. --relock must
// re-resolve it and rewrite the lockfile to the new live SHA.
func TestCheck_Relock_BumpsMovedBranchRef(t *testing.T) {
	reg := &httpmock.Registry{}

	staleSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	liveSHA := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	reg.Register(
		httpmock.GraphQLForRepo("example", "action"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("example/action", liveSHA, nodeActionYAML),
			},
		}),
	)
	// staleSHA is a legitimate ancestor of liveSHA (branch advanced) → ref-moved.
	reg.Register(
		httpmock.REST("GET", "repos/example/action/compare/"),
		httpmock.JSONResponse(map[string]any{
			"status": "ahead",
			"merge_base_commit": map[string]any{
				"sha": staleSHA,
			},
		}),
	)
	// Reverse-lookup stubs: main resolves to the live head; no tags.
	reg.Register(
		httpmock.REST("GET", "repos/example/action/branches"),
		httpmock.JSONResponse([]any{
			map[string]any{"name": "main", "commit": map[string]any{"sha": liveSHA}},
		}),
	)
	reg.Register(
		httpmock.REST("GET", "repos/example/action/tags"),
		httpmock.JSONResponse([]any{}),
	)

	workflowPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: example/action@main
`,
		"example/action@main=sha1-"+staleSHA,
	)

	stdout, _, err := runCommandWithHTTP(t, reg,
		"--relock", "--no-narrow", workflowPath,
	)
	require.NoError(t, err, "stdout:\n%s", stdout)

	pins := readTempLockfilePins(t)
	assert.Contains(t, pins, liveSHA, "--relock should rewrite the lockfile to the live SHA")
	assert.NotContains(t, pins, staleSHA, "--relock should drop the stale SHA")
}

// TestCheck_DefaultRun_DoesNotBumpBranchRef is the counterpart to the relock
// test: without --relock a mutable branch ref is trusted from the lockfile
// (fast path, no network) and its recorded SHA is left as-is.
func TestCheck_DefaultRun_DoesNotBumpBranchRef(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t) // no stubs should be hit on the fast path

	staleSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	workflowPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: example/action@main
`,
		"example/action@main=sha1-"+staleSHA,
	)

	_, _, err := runCommandWithHTTP(t, reg, workflowPath)
	require.NoError(t, err)

	assert.Contains(t, readTempLockfilePins(t), staleSHA,
		"a default run must not bump a trusted branch ref")
}
