package main

import (
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMissingRefNamesTheCause proves a ref that does not exist fails with an
// error finding that names it, rather than a fixable ref-changed plus a
// reachability warning.
func TestMissingRefNamesTheCause(t *testing.T) {
	refs := []struct {
		name, ref, detail, remedy string
	}{
		{"tag", "v999.0.0", "ref v999.0.0 does not exist in actions/checkout",
			"point `uses:` at a tag or branch that exists"},
		{"commit", "7b403c9ec14bd3ae0bbf793c2bee8815a7ac920b", "commit 7b403c9ec14b does not exist or is not reachable in actions/checkout",
			"pin a commit that exists in actions/checkout, or point `uses:` at a tag or branch"},
		{"short sha", "7b403c9", "ref 7b403c9 does not resolve in actions/checkout",
			"if this is a commit, use the full 40-character SHA; otherwise point `uses:` at a tag or branch that exists"},
	}
	for _, tt := range refs {
		for _, proxy := range []bool{false, true} {
			t.Run(tt.name+"/"+map[bool]string{false: "graphql", true: "dependabot proxy"}[proxy], func(t *testing.T) {
				testMissingRef(t, tt.ref, tt.detail, tt.remedy, proxy)
			})
		}
	}
}

func testMissingRef(t *testing.T, ref, detail, remedy string, proxy bool) {
	t.Helper()
	reg := &httpmock.Registry{}
	// Each resolve pass (plan, then diagnosis) asks again, for both
	// the JSON and the terminal run.
	for range 6 {
		registerMissingRef(reg, ref, proxy)
	}
	if proxy {
		t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
		useDefaultTransport(t, reg)
	}
	path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4.2.2
`, "actions/checkout@v4.2.2")
	wf, err := os.ReadFile(path)
	require.NoError(t, err)
	wf = []byte(strings.Replace(string(wf), "@v4.2.2", "@"+ref, 1))
	require.NoError(t, os.WriteFile(path, wf, 0o600))
	lockBefore := readTempLockfilePins(t)

	stdout, _, err := runCommandWithHTTP(t, reg,
		"--no-onboard", "--no-narrow", "--no-interactive", "--json=findings", path)
	require.ErrorIs(t, err, errSilent)
	var out struct {
		Findings []struct {
			Category    string `json:"category"`
			Severity    string `json:"severity"`
			Detail      string `json:"detail"`
			Remediation string `json:"remediation"`
			DocURL      string `json:"doc_url"`
		} `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &out), stdout)
	found := false
	for _, f := range out.Findings {
		if f.Category == "not-pinned" && f.Severity == "error" {
			found = true
			assert.Equal(t, detail, f.Detail)
			assert.Equal(t, remedy, f.Remediation)
			assert.Empty(t, f.DocURL, "the security-hardening page doesn't explain a missing ref")
		}
	}
	assert.True(t, found, stdout)
	assert.Equal(t, lockBefore, readTempLockfilePins(t))
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(wf), string(after))

	_, stderr, err := runCommandWithHTTP(t, reg,
		"--no-onboard", "--no-narrow", "--no-interactive", path)
	require.ErrorIs(t, err, errSilent)
	assert.Contains(t, stderr, "Ref does not exist actions/checkout@"+ref)
	assert.Contains(t, stderr, detail)
	assert.Contains(t, stderr, remedy)
	assert.NotContains(t, stderr, "Re-run without")
	assert.NotContains(t, stderr, "Not pinned")
	assert.NotContains(t, stderr, "security-hardening")
}

func registerMissingRef(reg *httpmock.Registry, ref string, proxy bool) {
	if !proxy {
		reg.Register(httpmock.GraphQL(`repository\(owner:`), httpmock.JSONResponse(map[string]any{
			"data": map[string]any{"a0": map[string]any{"nameWithOwner": "actions/checkout", "object": nil}},
		}))
		return
	}
	reg.Register(httpmock.REST("GET", `^/repos/actions/checkout$`), httpmock.JSONResponse(map[string]any{
		"full_name": "actions/checkout", "id": 1, "owner": map[string]any{"id": 1},
	}))
	refs := []string{ref}
	if len(ref) != 40 {
		refs = []string{"refs/tags/" + ref, "refs/heads/" + ref}
	}
	for _, name := range refs {
		reg.Register(httpmock.REST("GET", `^/repos/actions/checkout/commits/`+regexp.QuoteMeta(name)+`$`),
			httpmock.StatusResponse(http.StatusUnprocessableEntity))
	}
}
