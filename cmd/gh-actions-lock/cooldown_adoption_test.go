package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type cooldownTransport struct {
	mu           sync.Mutex
	commitDate   string
	releaseDate  string
	composite    bool
	commitReads  int
	releaseReads int
	manifestRefs []string
}

func (c *cooldownTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case httpmock.GraphQL(`commit: object\(oid`)(req):
		return parityOK(req)
	case httpmock.GraphQL(`file: file\(path`)(req):
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
			name := body.Variables[fmt.Sprintf("name%d", i)]
			expr := body.Variables[fmt.Sprintf("expr%d", i)]
			sha := parityTestSHA
			if strings.HasPrefix(expr, parityTestMoved) {
				sha = parityTestMoved
			}
			manifest := nodeActionYAML
			if c.composite && name == "checkout" {
				manifest = "runs:\n  using: composite\n  steps:\n    - uses: actions/setup-go@main\n"
			}
			c.manifestRefs = append(c.manifestRefs, owner+"/"+name+"@"+expr)
			data[fmt.Sprintf("a%d", i)] = testRepoResponse(owner+"/"+name, sha, manifest)
		}
		return httpmock.JSONResponse(map[string]any{"data": data})(req)
	case strings.Contains(req.URL.Path, "/commits/"):
		c.commitReads++
		date := c.commitDate
		if c.composite && strings.Contains(req.URL.Path, "/checkout/") {
			date = "2020-01-01T00:00:00Z"
		}
		return httpmock.JSONResponse(map[string]any{
			"sha": parityTestSHA, "commit": map[string]any{"committer": map[string]any{"date": date}},
		})(req)
	case strings.HasSuffix(req.URL.Path, "/tags") && !strings.Contains(req.URL.Path, "/releases/"):
		return httpmock.JSONResponse(httpmock.TagListResponse("v4.2.2", parityTestSHA, "v4", parityTestSHA))(req)
	case strings.HasSuffix(req.URL.Path, "/releases"):
		c.releaseReads++
		var releases []map[string]any
		if c.releaseDate != "" {
			releases = append(releases, map[string]any{"tag_name": "v4.2.2", "published_at": c.releaseDate})
		}
		return httpmock.JSONResponse(releases)(req)
	case strings.Contains(req.URL.Path, "/releases/tags/"):
		return httpmock.StatusResponse(404)(req)
	case strings.Contains(req.URL.Path, "/git/ref/heads/"):
		return httpmock.JSONResponse(map[string]any{"object": map[string]any{"sha": parityTestSHA, "type": "commit"}})(req)
	case strings.HasPrefix(req.URL.Path, "/repos/") && strings.Count(req.URL.Path, "/") == 3:
		nwo := strings.TrimPrefix(req.URL.Path, "/repos/")
		return httpmock.JSONResponse(map[string]any{
			"full_name": nwo, "default_branch": "main", "id": 1, "owner": map[string]any{"id": 1},
		})(req)
	}
	return nil, fmt.Errorf("unexpected cooldown fixture request: %s %s", req.Method, req.URL)
}

func configureDependabotCooldown(t *testing.T, days int) {
	t.Helper()
	require.NoError(t, os.WriteFile(".github/dependabot.yml", []byte(fmt.Sprintf(
		"version: 2\nupdates:\n  - package-ecosystem: github-actions\n    cooldown:\n      default-days: %d\n", days)), 0o600))
	t.Setenv("GH_ACTIONS_LOCK_CONFIG", filepath.Join(t.TempDir(), "absent"))
}

func TestCooldownRejectsNewAdoptionWithoutWrites(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ref       string
		mode      string
		noNarrow  bool
		recorded  bool
		composite bool
		release   bool
	}{
		{name: "partial version still guarded after narrowing", ref: "v4", release: true},
		{name: "no narrow still guarded", ref: "v4", noNarrow: true, release: true},
		{name: "exact tag still guarded", ref: "v4.2.2", release: true},
		{name: "branch still guarded", ref: "main"},
		{name: "full SHA still guarded", ref: parityTestSHA, noNarrow: true},
		{name: "normalized full SHA still guarded", ref: parityTestSHA, release: true},
		{name: "transitive branch still guarded", ref: "v4.2.2", composite: true},
		{name: "relock still guarded", ref: "main", mode: "--relock", recorded: true},
		{name: "accept moved still guarded", ref: "main", mode: "--accept-moved", recorded: true},
		{name: "unknown date fails closed", ref: "main"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var pins []string
			if tc.recorded {
				pins = []string{"actions/checkout@" + tc.ref + "=sha1-" + parityTestMoved}
			}
			path := writeTempWorkflow(t, "jobs:\n  build:\n    steps:\n      - uses: actions/checkout@"+tc.ref+"\n", pins...)
			configureDependabotCooldown(t, 7)
			before := snapshotWorkflows(t)
			var lockBefore string
			if tc.recorded {
				lockBefore = readTempLockfilePins(t)
			}
			rt := &cooldownTransport{composite: tc.composite, commitDate: time.Now().Add(-time.Hour).Format(time.RFC3339)}
			if tc.release {
				// A newly published release of old code must not bypass age.
				rt.commitDate = "2020-01-01T00:00:00Z"
				rt.releaseDate = time.Now().Add(-time.Hour).Format(time.RFC3339)
			}
			if tc.name == "unknown date fails closed" {
				rt.commitDate = ""
			}
			args := []string{"--no-interactive", path}
			if tc.noNarrow {
				args = append(args, "--no-narrow")
			}
			if tc.mode != "" {
				args = append(args, tc.mode)
			}
			out, err := runParity(t, rt, args...)
			require.ErrorIs(t, err, errSilent)
			assert.False(t, out.Valid)
			found := false
			for _, f := range out.Findings {
				if f.Category == "release-cooldown" {
					found = true
					assert.Equal(t, "error", f.Severity)
					assert.Contains(t, f.Detail, "cooldown")
					if tc.composite {
						assert.Contains(t, f.Detail, "actions/setup-go@main")
					}
				}
			}
			assert.True(t, found, "%+v", out.Findings)
			assert.Positive(t, rt.commitReads, "the candidate date must actually be checked")
			assert.Equal(t, before, snapshotWorkflows(t))
			if tc.recorded {
				assert.Equal(t, lockBefore, readTempLockfilePins(t))
			} else {
				assert.NoFileExists(t, ".github/workflows/actions.lock")
			}
		})
	}
}

func TestCooldownAllowsOldCandidateAndKeepsApprovedPins(t *testing.T) {
	for _, tc := range []struct {
		name     string
		recorded bool
		mode     string
	}{
		{name: "old candidate is adopted"},
		{name: "sticky pin is not re-aged", recorded: true},
		{name: "verify is not adoption", recorded: true, mode: "--verify"},
		{name: "unchanged relock is not adoption", recorded: true, mode: "--relock"},
		{name: "unchanged accept moved is not adoption", recorded: true, mode: "--accept-moved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var pins []string
			if tc.recorded {
				pins = []string{"actions/checkout@v4.2.2=sha1-" + parityTestSHA}
			}
			path := writeTempWorkflow(t, "jobs:\n  build:\n    steps:\n      - uses: actions/checkout@v4.2.2\n", pins...)
			configureDependabotCooldown(t, 7)
			rt := &cooldownTransport{commitDate: "2020-01-01T00:00:00Z"}
			args := []string{"--no-narrow", "--no-interactive", path}
			if tc.recorded {
				// Unknown dates cannot invalidate an already-approved target.
				rt.commitDate = ""
			}
			if tc.mode != "" {
				args = append(args, tc.mode)
			}
			_, stderr, err := runCommandWithHTTP(t, rt, args...)
			require.NoError(t, err, stderr)
			assert.Contains(t, readTempLockfilePins(t), parityTestSHA)
			if tc.recorded {
				assert.Zero(t, rt.commitReads, "verification must not re-age an approved commit")
			} else {
				assert.Positive(t, rt.commitReads)
			}
		})
	}
}

func TestCooldownRefusalExplainsCandidateInTerminal(t *testing.T) {
	path := writeTempWorkflow(t, "jobs:\n  build:\n    steps:\n      - uses: actions/checkout@main\n")
	configureDependabotCooldown(t, 7)
	rt := &cooldownTransport{commitDate: time.Now().Format(time.RFC3339)}
	_, stderr, err := runCommandWithHTTP(t, rt, "--no-interactive", path)
	require.ErrorIs(t, err, errSilent)
	assert.Contains(t, stderr, "release cooldown requires 7 days")
	assert.Contains(t, stderr, "actions/checkout@main")
	assert.Contains(t, stderr, "Nothing was written")
	assert.NoFileExists(t, ".github/workflows/actions.lock")
}

func TestCooldownRefusalRollsBackLocalMigration(t *testing.T) {
	path := writeTempWorkflow(t, "jobs:\n  build:\n    steps:\n      - uses: ./.github/actions/local\n      - uses: actions/checkout@main\n")
	require.NoError(t, os.MkdirAll(".github/actions/local", 0o755))
	require.NoError(t, os.WriteFile(".github/actions/local/action.yml", []byte(
		"runs:\n  using: composite\n  steps:\n    - run: echo local\n      shell: bash\n"), 0o600))
	configureDependabotCooldown(t, 7)
	root, err := os.Getwd()
	require.NoError(t, err)
	t.Setenv("GH_ACTIONS_LOCK_REPO_ROOT", root)
	before := snapshotWorkflows(t)
	rt := &cooldownTransport{commitDate: time.Now().Format(time.RFC3339)}
	out, err := runParity(t, rt, "--no-interactive", path)
	require.ErrorIs(t, err, errSilent)
	assert.False(t, out.Valid)
	assert.Positive(t, rt.commitReads, "%+v", out.Findings)
	assert.Equal(t, before, snapshotWorkflows(t))
}
