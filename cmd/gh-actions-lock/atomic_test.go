package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const cleanWorkflow = `name: clean
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/setup-go@v5.0.0
`

// writeCleanWorkflow adds a workflow the run could pin on its own, plus a
// local action the run would migrate to `$/…` when withLocal is set.
func writeCleanWorkflow(t *testing.T, reg *httpmock.Registry, withLocal bool) string {
	t.Helper()
	reg.Register(httpmock.GraphQLForRepo("actions", "setup-go"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": testRepoResponse("actions/setup-go", parityTestMoved, nodeActionYAML)},
	}))
	reg.Register(httpmock.REST("GET", `^/repos/actions/setup-go/tags`),
		httpmock.JSONResponse(httpmock.TagListResponse("v5.0.0", parityTestMoved)))
	reg.Register(httpmock.REST("GET", `repos/actions/setup-go$`), httpmock.JSONResponse(map[string]any{
		"default_branch": "main", "id": 2, "owner": map[string]any{"id": 1},
	}))
	require.NoError(t, os.MkdirAll(".git", 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(".github", "actions", "local"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(".github", "actions", "local", "action.yml"),
		[]byte("runs:\n  using: composite\n  steps:\n    - run: echo hi\n      shell: bash\n"), 0o600))
	path := filepath.Join(".github", "workflows", "clean.yml")
	body := cleanWorkflow
	if withLocal {
		body += "      - uses: ./.github/actions/local\n"
	}
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return filepath.ToSlash(path)
}

// snapshotWorkflows reads every file under .github so a test can assert the
// run left the tree byte-identical.
func snapshotWorkflows(t *testing.T) map[string]string {
	t.Helper()
	files := map[string]string{}
	require.NoError(t, filepath.WalkDir(".github", func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		files[p] = string(b)
		return err
	}))
	return files
}

// repoIDMismatch answers parity as intact except for actions/checkout, which
// now resolves to a different repository ID than the one locked.
type repoIDMismatch struct{}

func (repoIDMismatch) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	resp, err := parityOK(req)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Data map[string]map[string]any `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	for _, a := range payload.Data {
		if a["nameWithOwner"] == "actions/checkout" {
			a["databaseId"] = 99
		}
	}
	return httpmock.JSONResponse(payload)(req)
}

// TestRun_CleanWorkflowPins is the control for the tests below: on its own,
// the clean workflow is pinned and migrated.
func TestRun_CleanWorkflowPins(t *testing.T) {
	reg := &httpmock.Registry{}
	writeTempWorkflow(t, "name: ci\non: push\n")
	clean := writeCleanWorkflow(t, reg, true)

	_, stderr, err := runCommandWithHTTP(t, reg, clean)
	require.NoError(t, err, stderr)
	assert.Contains(t, readTempLockfilePins(t), "actions/setup-go")
	b, err := os.ReadFile(clean)
	require.NoError(t, err)
	assert.Contains(t, string(b), "$/.github/actions/local")
}

func TestRun_BlockingErrorWritesNothing(t *testing.T) {
	for name, args := range map[string][]string{"terminal": nil, "json": {"--json"}} {
		t.Run(name, func(t *testing.T) {
			reg := &httpmock.Registry{}
			reg.Register(httpmock.GraphQL(`commit: object\(oid`), repoIDMismatch{}.RoundTrip)
			blocked := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4.2.1
`, "actions/checkout@v4.2.1=sha1-"+parityTestSHA)
			clean := writeCleanWorkflow(t, reg, true)
			before := snapshotWorkflows(t)

			_, stderr, err := runCommandWithHTTP(t, reg, append(args, blocked, clean)...)
			require.ErrorIs(t, err, errSilent)
			assert.Equal(t, before, snapshotWorkflows(t), "no file may change")
			if name == "terminal" {
				assert.Contains(t, stderr, "different repository than the one locked")
				assert.Contains(t, stderr, "Nothing was written")
			}
		})
	}
}

func TestRun_UnloadableWorkflowWritesNothing(t *testing.T) {
	reg := &httpmock.Registry{}
	broken := writeTempWorkflow(t, "name: ci\non: [push\n")
	clean := writeCleanWorkflow(t, reg, false)
	before := snapshotWorkflows(t)

	_, stderr, err := runCommandWithHTTP(t, reg, "--no-migrate-local-actions", broken, clean)
	require.ErrorIs(t, err, errSilent)
	assert.Equal(t, before, snapshotWorkflows(t), "no file may change")
	assert.NoFileExists(t, filepath.Join(".github", "workflows", "actions.lock"))
	assert.Contains(t, stderr, "failed to load workflow")
	assert.Contains(t, stderr, "Nothing was written")
}
