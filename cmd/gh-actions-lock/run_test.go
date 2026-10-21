package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/cmd/gh-actions-lock/format"
	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRun_CooldownWarningsOnlyOnFixRuns(t *testing.T) {
	for _, mode := range []struct {
		name     string
		args     []string
		readOnly bool
	}{
		{"resolve", nil, false},
		{"relock", []string{"--relock"}, false},
		{"accept-moved", []string{"--accept-moved"}, false},
		{"no-fix", []string{"--no-fix"}, true},
		{"verify", []string{"--verify"}, true},
		{"verify-local", []string{"--verify-local"}, true},
		{"no-fix-relock", []string{"--no-fix", "--relock"}, true},
		{"verify-relock", []string{"--verify", "--relock"}, true},
		{"no-fix-accept-moved", []string{"--no-fix", "--accept-moved"}, true},
	} {
		for _, jsonOut := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/json=%v", mode.name, jsonOut), func(t *testing.T) {
				const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
				pins := []string{}
				if mode.readOnly {
					pins = append(pins, "example/action@main=sha1-"+sha)
				}
				path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: example/action@main
`, pins...)
				t.Setenv("GH_ACTIONS_LOCK_CONFIG", filepath.Join(t.TempDir(), "none.yml"))
				require.NoError(t, os.WriteFile(filepath.Join(".github", "dependabot.yml"), []byte(`
version: 2
updates:
  - package-ecosystem: github-actions
    directory: /
    cooldown:
      default-days: 7
      exclude: ["example/*"]
`), 0o644))
				before, err := os.ReadFile(path)
				require.NoError(t, err)
				lockBefore := ""
				if mode.readOnly {
					lockBefore = readTempLockfilePins(t)
				}

				reg := &httpmock.Registry{}
				if mode.name != "verify-local" {
					reg.Register(httpmock.GraphQLForRepo("example", "action"), httpmock.JSONResponse(map[string]any{
						"data": map[string]any{"a0": testRepoResponse("example/action", sha, nodeActionYAML)},
					}))
				}
				if !mode.readOnly {
					reg.Register(httpmock.REST("GET", "repos/example/action$"), httpmock.JSONResponse(map[string]any{
						"id": 2, "owner": map[string]any{"id": 1}, "default_branch": "main",
					}))
				}
				args := append([]string{"--no-interactive"}, mode.args...)
				if jsonOut {
					args = append(args, "--json=valid,findings")
				}
				stdout, stderr, err := runCommandWithHTTP(t, reg, append(args, path)...)
				require.NoError(t, err, "stdout:\n%s\nstderr:\n%s", stdout, stderr)
				const warning = "Dependabot cooldown include/exclude filters are not supported and were ignored"
				if jsonOut {
					var payload struct {
						Valid    bool             `json:"valid"`
						Findings []format.Finding `json:"findings"`
					}
					require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
					if mode.readOnly {
						assert.True(t, payload.Valid)
						assert.Empty(t, payload.Findings)
					} else {
						// The fix pinned the action; only the warning remains.
						assert.True(t, payload.Valid)
						require.Len(t, payload.Findings, 1)
						assert.Equal(t, "cooldown-config-ignored", payload.Findings[0].Category)
						assert.Equal(t, "warning", payload.Findings[0].Severity)
						assert.Equal(t, warning, payload.Findings[0].Detail)
					}
				} else if mode.readOnly {
					assert.NotContains(t, stdout+stderr, warning)
				} else {
					assert.Contains(t, stdout+stderr, warning)
				}
				if mode.readOnly {
					after, err := os.ReadFile(path)
					require.NoError(t, err)
					assert.Equal(t, before, after)
					assert.Equal(t, lockBefore, readTempLockfilePins(t))
				} else {
					assert.Contains(t, readTempLockfilePins(t), sha)
				}
				reg.Verify(t)
			})
		}
	}
}

func TestRun_ParitySSOGuidance(t *testing.T) {
	const message = "Resource protected by organization SAML enforcement. You must grant your Personal Access token access to this organization."
	for _, mode := range []struct {
		name string
		args []string
	}{
		{"fix", nil},
		{"no-fix", []string{"--no-fix"}},
		{"verify", []string{"--verify"}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			t.Setenv("CODESPACES", "")
			reg := &httpmock.Registry{}
			reg.Register(httpmock.GraphQL(`commit: object\(oid`), func(req *http.Request) (*http.Response, error) {
				resp, err := httpmock.JSONResponse(map[string]string{"message": message})(req)
				if err != nil {
					return nil, err
				}
				resp.StatusCode = http.StatusForbidden
				return resp, nil
			})
			reg.Register(httpmock.REST("GET", `^/repos/krzema12/github-actions-typing$`), httpmock.StatusResponse(http.StatusNotFound))
			path := writeTempWorkflow(t, transferWorkflow, "krzema12/github-actions-typing@v2.2.2=sha1-"+parityTestSHA)
			lockBefore := readTempLockfilePins(t)

			_, stderr, err := runCommandWithHTTP(t, reg, append(mode.args, "--no-interactive", path)...)
			require.ErrorIs(t, err, errSilent)
			assert.Contains(t, stderr, "GitHub returned 403 Forbidden: "+message)
			assert.NotContains(t, stderr, "https://api.github.com/")
			assert.Equal(t, 1, strings.Count(stderr, message))
			if mode.name != "fix" {
				assert.Equal(t, 1, strings.Count(stderr, "gh auth refresh -h github.com"))
			}
			assert.Equal(t, lockBefore, readTempLockfilePins(t))
			reg.Verify(t)
		})
	}
}

func TestRun_ParitySSORateLimitedGuidance(t *testing.T) {
	const message = "Resource protected by organization SAML enforcement."
	for _, mode := range []struct {
		name string
		args []string
	}{
		{"fix", nil},
		{"no-fix", []string{"--no-fix"}},
		{"verify", []string{"--verify"}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			t.Setenv("CODESPACES", "true")
			reg := &httpmock.Registry{}
			reg.Register(httpmock.GraphQL(`commit: object\(oid`), httpmock.JSONResponse(map[string]any{
				"data": map[string]any{"a0": map[string]any{
					"nameWithOwner": "krzema12/github-actions-typing",
					"databaseId":    1,
					"commit":        map[string]any{"oid": parityTestSHA},
					"tag":           map[string]any{"oid": parityTestMoved},
					"tagRef":        map[string]any{"oid": parityTestMoved},
				}},
			}))
			reg.RegisterReusable(httpmock.REST("HEAD", `^/orgs/krzema12$`), httpmock.StatusResponse(http.StatusOK))
			reg.Register(httpmock.REST("GET", `^/repos/krzema12/github-actions-typing/compare/`), func(req *http.Request) (*http.Response, error) {
				resp, err := httpmock.JSONResponse(map[string]string{"message": message})(req)
				if err != nil {
					return nil, err
				}
				resp.StatusCode = http.StatusForbidden
				return resp, nil
			})
			reg.Register(httpmock.REST("GET", `^/repos/krzema12/github-actions-typing/compare/`), httpmock.StatusResponse(http.StatusTooManyRequests))
			useDefaultTransport(t, reg)
			path := writeTempWorkflow(t, transferWorkflow, "krzema12/github-actions-typing@v2.2.2=sha1-"+parityTestSHA)
			lockBefore := readTempLockfilePins(t)

			_, stderr, err := runCommandWithHTTP(t, reg, append(mode.args, "--no-interactive", path)...)
			require.ErrorIs(t, err, errSilent)
			assert.Contains(t, stderr, "SSO authorization required")
			assert.Contains(t, stderr, "Authorize it at https://github.com/orgs/krzema12/sso and retry")
			assert.Contains(t, stderr, "anonymous fallback for public repositories was rate-limited")
			assert.NotContains(t, stderr, "https://api.github.com/")
			assert.Equal(t, 1, strings.Count(stderr, "Authorize it at"))
			if mode.name != "fix" {
				assert.Equal(t, 1, strings.Count(stderr, "Your codespace's GITHUB_TOKEN cannot be SSO-authorized."))
			}
			assert.Equal(t, lockBefore, readTempLockfilePins(t))
			reg.Verify(t)
		})
	}
}

func composite(uses ...string) string {
	var b strings.Builder
	b.WriteString("runs:\n  using: composite\n  steps:\n")
	for _, u := range uses {
		b.WriteString("    - uses: " + u + "\n")
	}
	return b.String()
}

// writeManifestRepo writes one workflow per name using its refs, and a
// lockfile recording each entry with the given `uses:`. Entry commits are
// distinct; each workflow records the entries its refs name.
func writeManifestRepo(t *testing.T, workflows map[string][]string, entries map[string][]string) {
	t.Helper()
	dir := t.TempDir()
	wfDir := filepath.Join(dir, ".github", "workflows")
	require.NoError(t, os.MkdirAll(wfDir, 0o755))
	var lock strings.Builder
	lock.WriteString("version: '" + parserlock.Version + "'\nworkflows:\n")
	for _, name := range slices.Sorted(maps.Keys(workflows)) {
		body := "on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n"
		lock.WriteString("    '.github/workflows/" + name + "':\n")
		var keys []string
		for _, ref := range workflows[name] {
			body += "      - uses: " + ref + "\n"
			r := parserlock.ParseActionRef(ref)
			if k := r.NWO() + "@" + r.Ref; !slices.Contains(keys, k) {
				keys = append(keys, k)
				lock.WriteString("        - '" + k + "'\n")
			}
		}
		require.NoError(t, os.WriteFile(filepath.Join(wfDir, name), []byte(body), 0o600))
	}
	lock.WriteString("dependencies:\n")
	for i, key := range slices.Sorted(maps.Keys(entries)) {
		_, ref, _ := strings.Cut(key, "@")
		if parserlock.IsFullSha(ref) {
			ref = "v0"
		}
		fmt.Fprintf(&lock, "    '%s':\n        ref: '%s'\n        commit: 'sha1-%s'\n        owner_id: 1\n        repo_id: %d\n", key, ref, entrySHA(i, key), i+1)
		if len(entries[key]) > 0 {
			lock.WriteString("        uses:\n")
			for _, u := range entries[key] {
				lock.WriteString("            - '" + u + "'\n")
			}
		}
	}
	require.NoError(t, os.WriteFile(filepath.Join(wfDir, "actions.lock"), []byte(lock.String()), 0o600))
	t.Chdir(dir)
}

// entrySHA is entry i's commit; a full-SHA key keeps its own.
func entrySHA(i int, key string) string {
	if _, ref, _ := strings.Cut(key, "@"); parserlock.IsFullSha(ref) {
		return strings.ToLower(ref)
	}
	return fmt.Sprintf("%040x", i+1)
}

const childSHA = "ABCDEF0123456789ABCDEF0123456789ABCDEF01"

func TestRun_LockedManifests(t *testing.T) {
	for name, tc := range map[string]struct {
		workflow  []string
		entries   map[string][]string
		manifests map[string]string
		// blocked is the detail of the blocking finding; "" means valid.
		blocked string
	}{
		"recorded edges": {
			workflow:  []string{"acme/comp@v1"},
			entries:   map[string][]string{"acme/comp@v1": {"acme/child@v2"}, "acme/child@v2": nil},
			manifests: map[string]string{"acme/comp": composite("acme/child@v2", "./local", "docker://alpine")},
		},
		"erased edge": {
			workflow:  []string{"acme/comp@v1"},
			entries:   map[string][]string{"acme/comp@v1": nil, "acme/child@v2": nil},
			manifests: map[string]string{"acme/comp": composite("acme/child@v2")},
			blocked:   "acme/comp@v1 at locked commit 000000000000 uses acme/child@v2, but its lockfile entry acme/comp@v1 does not record it",
		},
		"erased nested edge": {
			workflow: []string{"acme/comp@v1"},
			entries:  map[string][]string{"acme/comp@v1": {"acme/mid@v1"}, "acme/mid@v1": nil, "acme/leaf@v1": nil},
			manifests: map[string]string{
				"acme/comp": composite("acme/mid@v1"),
				"acme/mid":  composite("acme/leaf@v1"),
			},
			blocked: "acme/mid@v1 at locked commit 000000000000 uses acme/leaf@v1",
		},
		"sub-action reached from a sibling": {
			workflow: []string{"acme/tools/a@v1"},
			entries:  map[string][]string{"acme/tools@v1": {"acme/x@v1"}, "acme/x@v1": nil},
			manifests: map[string]string{
				"acme/tools/a": composite("$/b", "acme/x@v1"),
				"acme/tools/b": composite("acme/y@v1"),
			},
			blocked: "acme/tools/b@v1 at locked commit 000000000000 uses acme/y@v1",
		},
		"full SHA in another case": {
			workflow:  []string{"acme/comp@v1"},
			entries:   map[string][]string{"acme/comp@v1": {"acme/child@" + strings.ToLower(childSHA)}, "acme/child@" + strings.ToLower(childSHA): nil},
			manifests: map[string]string{"acme/comp": composite("acme/child@" + childSHA)},
		},
		"missing manifest": {
			workflow:  []string{"acme/comp/gone@v1"},
			entries:   map[string][]string{"acme/comp@v1": nil},
			manifests: map[string]string{"acme/comp/gone": ""},
			blocked:   "acme/comp/gone@v1 has no action.yml, action.yaml or Dockerfile at locked commit 000000000000",
		},
		"Dockerfile-only actions": {
			workflow:  []string{"acme/docker@v1", "acme/comp@v1"},
			entries:   map[string][]string{"acme/docker@v1": nil, "acme/comp@v1": {"acme/tools@v1"}, "acme/tools@v1": nil},
			manifests: map[string]string{"acme/docker": dockerfileOnly, "acme/comp": composite("acme/tools/build@v1"), "acme/tools/build": dockerfileOnly},
		},
	} {
		for _, mode := range [][]string{{"--verify"}, {"--no-fix"}, {}} {
			t.Run(name+"/"+strings.Join(append([]string{"default"}, mode...)[min(len(mode), 1):], ""), func(t *testing.T) {
				writeManifestRepo(t, map[string][]string{"ci.yml": tc.workflow}, tc.entries)
				before, err := os.ReadFile(".github/workflows/actions.lock")
				require.NoError(t, err)
				reg := &httpmock.Registry{}
				registerLockedManifests(reg, tc.manifests)

				p, err := runParity(t, reg, mode...)
				if tc.blocked == "" {
					require.NoError(t, err)
					assert.True(t, p.Valid, "%+v", p.Findings)
					return
				}
				require.ErrorIs(t, err, errSilent)
				assert.False(t, p.Valid)
				var details []string
				for _, f := range p.Findings {
					if f.Severity == "error" {
						assert.Equal(t, "incomplete-lockfile", f.Category)
						details = append(details, f.Detail)
					}
				}
				require.Len(t, details, 1, "%+v", p.Findings)
				assert.Contains(t, details[0], tc.blocked)
				after, err := os.ReadFile(".github/workflows/actions.lock")
				require.NoError(t, err)
				assert.Equal(t, string(before), string(after), "a blocked workflow is not written")
			})
		}
	}
}

// TestRun_LockedManifestsDiscloseLocalActions: a run that keeps a recorded
// composite still discloses the `./…` uses in its locked manifest.
func TestRun_LockedManifestsDiscloseLocalActions(t *testing.T) {
	for _, mode := range [][]string{{"--verify"}, {"--no-fix"}, {}} {
		t.Run(strings.Join(append([]string{"default"}, mode...)[min(len(mode), 1):], ""), func(t *testing.T) {
			writeManifestRepo(t, map[string][]string{"ci.yml": {"acme/comp@v1"}}, map[string][]string{"acme/comp@v1": nil})
			reg := &httpmock.Registry{}
			registerLockedManifests(reg, map[string]string{"acme/comp": composite("./local")})

			p, err := runParity(t, reg, mode...)
			require.NoError(t, err)
			assert.True(t, p.Valid, "%+v", p.Findings)
			local := localActionFindings(p.Findings)
			require.Len(t, local, 1, "%+v", p.Findings)
			assert.Equal(t, "warning", local[0].Severity)
			assert.Equal(t, "acme/comp@v1", local[0].RequiredBy)
			assert.Contains(t, local[0].Detail, "cannot follow workspace-local action ./local used by acme/comp@v1")
		})
	}
}

// TestRun_LockedManifestsOneBatchPerLevel: reads are batched across
// workflows, one request per composite nesting level.
func TestRun_LockedManifestsOneBatchPerLevel(t *testing.T) {
	writeManifestRepo(t, map[string][]string{"a.yml": {"acme/a@v1"}, "b.yml": {"acme/b@v1"}},
		map[string][]string{"acme/a@v1": {"acme/leaf@v1"}, "acme/b@v1": {"acme/leaf@v1"}, "acme/leaf@v1": nil})
	reg := &httpmock.Registry{}
	reads := registerLockedManifests(reg, map[string]string{"acme/a": composite("acme/leaf@v1"), "acme/b": composite("acme/leaf@v1")})

	p, err := runParity(t, reg, "--verify")
	require.NoError(t, err)
	assert.True(t, p.Valid, "%+v", p.Findings)
	assert.EqualValues(t, 2, reads.Load(), "the roots, then their shared child")
}

// TestRun_LockedManifestsUnderRelock: a read-only relock leaves the lockfile
// as recorded, so it is checked; a writable one re-resolves from live
// manifests and reads none at locked commits.
func TestRun_LockedManifestsUnderRelock(t *testing.T) {
	manifests := map[string]string{"acme/comp": composite("acme/child@v2")}
	for _, tc := range []struct {
		args  []string
		reads int64
	}{
		{[]string{"--verify", "--relock"}, 1},
		{[]string{"--no-fix", "--accept-moved"}, 1},
		{[]string{"--relock"}, 0},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			writeManifestRepo(t, map[string][]string{"ci.yml": {"acme/comp@v1"}},
				map[string][]string{"acme/comp@v1": nil, "acme/child@v2": nil})
			reg := &httpmock.Registry{}
			reads := registerLockedManifests(reg, manifests)
			registerLive(reg, manifests, map[string]string{"acme/comp": entrySHA(1, ""), "acme/child": entrySHA(0, "")})

			p, err := runParity(t, reg, append(tc.args, "--no-narrow")...)
			assert.Equal(t, tc.reads, reads.Load())
			if tc.reads == 0 {
				require.NoError(t, err, "%+v", p.Findings)
				return
			}
			require.ErrorIs(t, err, errSilent)
			assert.Contains(t, fmt.Sprint(p.Findings), "does not record it")
		})
	}
}

// registerLive answers live resolution of each owner/repo at commits.
func registerLive(reg *httpmock.Registry, manifests, commits map[string]string) {
	reg.RegisterReusable(httpmock.GraphQL(`file: file\(path`), actionFiles(manifests, commits))
	reg.RegisterReusable(httpmock.REST("GET", `/(tags|releases)$`), httpmock.JSONResponse([]any{}))
}

// TestRun_LockedManifestsBesideAFix: a workflow whose other pins a default
// run would fix is still checked, and the damage blocks the whole write.
func TestRun_LockedManifestsBesideAFix(t *testing.T) {
	writeManifestRepo(t, map[string][]string{"ci.yml": {"acme/comp@v1"}},
		map[string][]string{"acme/comp@v1": nil, "acme/child@v2": nil})
	require.NoError(t, os.WriteFile(".github/workflows/ci.yml", []byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: acme/comp@v1\n      - uses: acme/new@v1\n"), 0o600))
	before, err := os.ReadFile(".github/workflows/actions.lock")
	require.NoError(t, err)
	reg := &httpmock.Registry{}
	registerLockedManifests(reg, map[string]string{"acme/comp": composite("acme/child@v2")})
	registerLive(reg, nil, map[string]string{"acme/new": entrySHA(9, "")})

	p, err := runParity(t, reg, "--no-narrow")
	require.ErrorIs(t, err, errSilent)
	assert.Contains(t, fmt.Sprint(p.Findings), "does not record it")
	after, err := os.ReadFile(".github/workflows/actions.lock")
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after))
}

// TestRun_LockedManifestsSeededRoot: a workflow newly using an action
// another workflow locked takes it from the lockfile without a live read,
// so its manifest is checked before the entry is reused.
func TestRun_LockedManifestsSeededRoot(t *testing.T) {
	writeManifestRepo(t, map[string][]string{"a.yml": {"acme/comp@v1"}},
		map[string][]string{"acme/comp@v1": nil, "acme/child@v2": nil})
	require.NoError(t, os.WriteFile(".github/workflows/b.yml", []byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: acme/comp@v1\n"), 0o600))
	before, err := os.ReadFile(".github/workflows/actions.lock")
	require.NoError(t, err)
	reg := &httpmock.Registry{}
	registerLockedManifests(reg, map[string]string{"acme/comp": composite("acme/child@v2")})

	p, err := runParity(t, reg, ".github/workflows/b.yml")
	require.ErrorIs(t, err, errSilent)
	require.NotEmpty(t, p.Findings)
	assert.Contains(t, p.Findings[len(p.Findings)-1].Detail, "uses acme/child@v2, but its lockfile entry acme/comp@v1 does not record it")
	after, err := os.ReadFile(".github/workflows/actions.lock")
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after))
}

// TestRun_LockedManifestsReusedSeedChildren: a workflow newly reaching a
// composite another workflow locked, directly or through a live composite,
// takes its recorded children unread, so they are walked too.
func TestRun_LockedManifestsReusedSeedChildren(t *testing.T) {
	for name, tc := range map[string]struct {
		uses      string
		manifests map[string]string
		want      string
	}{
		"seeded child under a live root": {
			uses:      "acme/live@v1",
			manifests: map[string]string{"acme/live": composite("acme/comp@v1"), "acme/comp": composite("acme/child@v2", "acme/extra@v1")},
			want:      "uses acme/extra@v1, but its lockfile entry acme/comp@v1 does not record it",
		},
		"missing commit under a reused seed": {
			uses:      "acme/comp@v1",
			manifests: map[string]string{"acme/comp": composite("acme/child@v2"), "acme/child": missingCommit},
			want:      "could not read locked manifest acme/child@v2",
		},
	} {
		t.Run(name, func(t *testing.T) {
			writeManifestRepo(t, map[string][]string{"a.yml": {"acme/comp@v1"}},
				map[string][]string{"acme/comp@v1": {"acme/child@v2"}, "acme/child@v2": nil})
			require.NoError(t, os.WriteFile(".github/workflows/b.yml", []byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: "+tc.uses+"\n"), 0o600))
			before, err := os.ReadFile(".github/workflows/actions.lock")
			require.NoError(t, err)
			reg := &httpmock.Registry{}
			registerLockedManifests(reg, tc.manifests)
			registerLive(reg, tc.manifests, map[string]string{"acme/live": entrySHA(9, "")})

			p, err := runParity(t, reg, "--no-narrow", ".github/workflows/b.yml")
			require.ErrorIs(t, err, errSilent)
			assert.Contains(t, fmt.Sprint(p.Findings), tc.want)
			after, err := os.ReadFile(".github/workflows/actions.lock")
			require.NoError(t, err)
			assert.Equal(t, string(before), string(after))
		})
	}
}

// TestRun_LockedManifestsTerminal: a read-only run shows how to repair the
// lockfile and doesn't suggest that a plain run would.
func TestRun_LockedManifestsTerminal(t *testing.T) {
	writeManifestRepo(t, map[string][]string{"ci.yml": {"acme/comp@v1"}},
		map[string][]string{"acme/comp@v1": nil, "acme/child@v2": nil})
	reg := &httpmock.Registry{}
	registerLockedManifests(reg, map[string]string{"acme/comp": composite("acme/child@v2")})

	stdout, stderr, err := runCommandWithHTTP(t, reg, "--no-fix")
	require.ErrorIs(t, err, errSilent)
	term := stdout + stderr
	assert.Contains(t, term, "Lockfile doesn't match locked manifest acme/child@v2")
	assert.Contains(t, term, "restore the edge from version control, or run `gh actions-lock --relock`")
	assert.NotContains(t, term, "Re-run without")
}

// TestRun_LockedManifestsInconclusive: a failed read leaves dependencies
// unverified, so it blocks as a retryable error; behind the Dependabot
// proxy it aborts so the update is retried.
func TestRun_LockedManifestsInconclusive(t *testing.T) {
	for _, proxy := range []bool{false, true} {
		t.Run(fmt.Sprint("proxy=", proxy), func(t *testing.T) {
			writeManifestRepo(t, map[string][]string{"ci.yml": {"acme/comp@v1"}}, map[string][]string{"acme/comp@v1": nil})
			reg := &httpmock.Registry{}
			reg.RegisterReusable(lockedManifestRead, httpmock.StatusResponse(http.StatusBadGateway))
			if proxy {
				t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
				useDefaultTransport(t, reg)
				reg.Register(httpmock.REST("GET", `^/repos/acme/comp$`), httpmock.JSONResponse(map[string]any{"full_name": "acme/comp", "id": 1, "owner": map[string]any{"id": 1}}))
				reg.Register(httpmock.REST("GET", `^/repos/acme/comp/git/ref/tags/v1$`), httpmock.JSONResponse(httpmock.GitRefResponse("refs/tags/v1", entrySHA(0, ""))))
				reg.RegisterReusable(httpmock.REST("GET", `/commits/`), httpmock.StatusResponse(http.StatusBadGateway))
			}

			stdout, stderr, err := runCommandWithHTTP(t, reg, "--verify", "--json=valid,findings")
			if proxy {
				require.Error(t, err)
				assert.NotErrorIs(t, err, errSilent)
				assert.Contains(t, err.Error()+stderr, "could not read locked manifest acme/comp@v1")
				return
			}
			require.ErrorIs(t, err, errSilent, stderr)
			var p parityPayload
			require.NoError(t, json.Unmarshal([]byte(stdout), &p), stdout)
			assert.False(t, p.Valid)
			require.Len(t, p.Findings, 1)
			assert.Equal(t, "reachability-unknown", p.Findings[0].Category)
			assert.Equal(t, "error", p.Findings[0].Severity)
			assert.True(t, p.Findings[0].Retryable)
		})
	}
}

// TestRun_RelockRebuildsUnchangedComposite: octo/comp@v1 is locked at its
// current commit with no recorded children, but its manifest uses
// octo/child@v2. A plain run reports the missing edge and writes nothing;
// --relock rebuilds the closure from the manifest even though no ref moved.
func TestRun_RelockRebuildsUnchangedComposite(t *testing.T) {
	const compSHA, childSHA = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", "cccccccccccccccccccccccccccccccccccccccc"
	reg := &httpmock.Registry{}
	stubGraph(t, reg, nil,
		map[string]string{"octo/comp": compSHA, "octo/child": childSHA},
		map[string]string{"octo/comp": "name: c\nruns:\n  using: composite\n  steps:\n    - uses: octo/child@v2\n"})
	reg.RegisterReusable(httpmock.REST("GET", `repos/octo/child$`), httpmock.JSONResponse(map[string]any{"id": 2, "owner": map[string]any{"id": 1}}))
	useDefaultTransport(t, reg)
	path := writeTempWorkflow(t, "name: ci\non: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: octo/comp@v1\n")
	lock := "version: 'v0.0.3'\nworkflows:\n    '.github/workflows/workflow.yml':\n        - 'octo/comp@v1'\n" +
		"dependencies:\n    'octo/comp@v1':\n        ref: 'v1'\n        commit: 'sha1-" + compSHA + "'\n        owner_id: 1\n        repo_id: 1\n"
	lockPath := filepath.Join(".github", "workflows", "actions.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(lock), 0o600))

	_, stderr, err := runCommandWithHTTP(t, reg, "--no-narrow", "--no-interactive", path)
	require.ErrorIs(t, err, errSilent, stderr)
	assert.Contains(t, stderr, "gh actions-lock --relock")
	assert.Equal(t, lock, readTempLockfilePins(t), "nothing is written")

	_, stderr, err = runCommandWithHTTP(t, reg, "--relock", "--no-narrow", "--no-interactive", path)
	require.NoError(t, err, stderr)
	after := readTempLockfilePins(t)
	assert.Contains(t, after, "'octo/comp@v1':\n        ref: 'v1'\n        commit: 'sha1-"+compSHA+"'\n        owner_id: 1\n        repo_id: 1\n        uses:\n            - 'octo/child@v2'\n")
	assert.Contains(t, after, "'octo/child@v2':\n        ref: 'v2'\n        commit: 'sha1-"+childSHA+"'")
}

// TestRun_DanglingEdgeBlocksUntilRelock: a recorded edge with no entry
// fails --verify-local and a plain run, which writes nothing; --relock
// rebuilds the graph.
func TestRun_DanglingEdgeBlocksUntilRelock(t *testing.T) {
	const compSHA, childSHA = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", "cccccccccccccccccccccccccccccccccccccccc"
	reg := &httpmock.Registry{}
	stubGraph(t, reg, nil,
		map[string]string{"octo/comp": compSHA, "octo/child": childSHA},
		map[string]string{"octo/comp": "name: c\nruns:\n  using: composite\n  steps:\n    - uses: octo/child@v2\n"})
	reg.RegisterReusable(httpmock.REST("GET", `repos/octo/child$`), httpmock.JSONResponse(map[string]any{"id": 2, "owner": map[string]any{"id": 1}}))
	useDefaultTransport(t, reg)
	path := writeTempWorkflow(t, "name: ci\non: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: octo/comp@v1\n")
	lock := "version: 'v0.0.3'\nworkflows:\n    '.github/workflows/workflow.yml':\n        - 'octo/comp@v1'\n" +
		"dependencies:\n    'octo/comp@v1':\n        ref: 'v1'\n        commit: 'sha1-" + compSHA + "'\n        owner_id: 1\n        repo_id: 1\n        uses:\n            - 'octo/child@v2'\n"
	lockPath := filepath.Join(".github", "workflows", "actions.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(lock), 0o600))

	for _, mode := range []string{"--verify-local", "--verify"} {
		p, err := runParity(t, reg, mode, path)
		require.ErrorIs(t, err, errSilent, mode)
		require.Len(t, p.Findings, 1, "%s: %+v", mode, p.Findings)
		assert.Equal(t, "error", p.Findings[0].Severity)
		assert.Contains(t, p.Findings[0].Detail, "octo/comp@v1 uses octo/child@v2, which has no dependencies entry")
		assert.Contains(t, p.Findings[0].Remediation, "gh actions-lock --relock")
		_, stderr, err := runCommandWithHTTP(t, reg, mode, path)
		require.ErrorIs(t, err, errSilent, mode)
		assert.Contains(t, stderr, "! run `gh actions-lock --relock`", mode)
		assert.NotContains(t, stderr, "Run without", mode)
		assert.NotContains(t, stderr, "Re-run without", mode)
	}

	_, stderr, err := runCommandWithHTTP(t, reg, "--no-narrow", "--no-interactive", path)
	require.Error(t, err, stderr)
	assert.Contains(t, err.Error(), "gh actions-lock --relock")
	assert.Equal(t, lock, readTempLockfilePins(t), "nothing is written")

	_, stderr, err = runCommandWithHTTP(t, reg, "--relock", "--no-narrow", "--no-interactive", path)
	require.NoError(t, err, stderr)
	assert.Contains(t, readTempLockfilePins(t), "'octo/child@v2':\n        ref: 'v2'\n        commit: 'sha1-"+childSHA+"'")
	_, stderr, err = runCommandWithHTTP(t, reg, "--verify-local", path)
	require.NoError(t, err, stderr)
}

// TestRun_ContainerImages: images the workflow or its actions run by tag
// are one warning each in JSON, whether the action was locked or resolved
// this run, and leave the run valid.
func TestRun_ContainerImages(t *testing.T) {
	manifests := map[string]string{
		"acme/dock":  "runs:\n  using: docker\n  image: docker://ghcr.io/acme/dock:1\n",
		"acme/comp":  composite("docker://alpine:3", "docker://alpine@sha256:abc"),
		"acme/build": "runs:\n  using: docker\n  image: Dockerfile\n",
	}
	workflow := "on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    container: alpine:3\n    services:\n      db:\n        image: ${{ matrix.image }}\n    steps:\n      - uses: acme/dock@v1\n      - uses: acme/comp@v1\n      - uses: acme/build@v1\n      - uses: docker://alpine:3\n"
	for name, register := range map[string]func(*httpmock.Registry){
		"locked": func(reg *httpmock.Registry) { registerLockedManifests(reg, manifests) },
		"resolved": func(reg *httpmock.Registry) {
			registerLive(reg, manifests, map[string]string{"acme/dock": entrySHA(1, ""), "acme/comp": entrySHA(2, ""), "acme/build": entrySHA(3, "")})
		},
	} {
		t.Run(name, func(t *testing.T) {
			entries := map[string][]string{"acme/build@v1": nil, "acme/comp@v1": nil, "acme/dock@v1": nil}
			if name == "resolved" {
				entries = map[string][]string{"acme/other@v1": nil}
			}
			writeManifestRepo(t, map[string][]string{"ci.yml": slices.Sorted(maps.Keys(entries))}, entries)
			require.NoError(t, os.WriteFile(".github/workflows/ci.yml", []byte(workflow), 0o600))
			reg := &httpmock.Registry{}
			register(reg)

			p, err := runParity(t, reg, "--no-fix")
			if name == "resolved" {
				require.ErrorIs(t, err, errSilent, "the new pins aren't locked yet")
			} else {
				require.NoError(t, err)
				assert.True(t, p.Valid, "%+v", p.Findings)
			}
			byDetail := map[string]format.Finding{}
			for _, f := range p.Findings {
				if f.Category == "container-image" {
					byDetail[f.Detail] = f
				}
			}
			assert.Len(t, byDetail, 4, "%+v", p.Findings)
			alpine := byDetail["container image alpine:3 is not pinned by digest; used by job test, job test container, acme/comp@v1"]
			assert.Equal(t, "warning", alpine.Severity)
			assert.Equal(t, checks.ContainerImage.DocURL(), alpine.DocURL)
			assert.Empty(t, alpine.RequiredBy)
			dock := byDetail["container image ghcr.io/acme/dock:1 is not pinned by digest; used by acme/dock@v1"]
			assert.Equal(t, "acme/dock@v1", dock.RequiredBy)
			expr := byDetail["container image `${{ matrix.image }}` is chosen at runtime, so the lockfile can't check it; used by job test service db"]
			assert.Contains(t, expr.Remediation, "pin each value of the matrix or input by digest")
			build := byDetail["acme/build@v1 builds its container from Dockerfile; the lockfile doesn't cover its base image; used by acme/build@v1"]
			assert.Equal(t, "acme/build@v1", build.RequiredBy)
		})
	}
}

// TestRun_LocalActionsAreOpaque: a fix run locks the workflow's remote refs,
// leaves `./…` uses and their action files byte-identical, and reports each
// local action as a non-blocking warning. --no-migrate-local-actions changes nothing.
func TestRun_LocalActionsAreOpaque(t *testing.T) {
	for name, extra := range map[string][]string{"default": nil, "no-migrate flag": {"--no-migrate-local-actions"}} {
		t.Run(name, func(t *testing.T) {
			reg := &httpmock.Registry{}
			writeTempWorkflow(t, "name: ci\non: push\n")
			clean := writeCleanWorkflow(t, reg, true)
			before := snapshotWorkflows(t)

			p, err := runParity(t, reg, append(extra, clean)...)
			require.NoError(t, err)
			local := localActionFindings(p.Findings)
			require.Len(t, local, 1, "%+v", p.Findings)
			assert.Equal(t, "warning", local[0].Severity)
			assert.Equal(t, "cannot follow workspace-local action ./.github/actions/local", local[0].Detail)
			assert.Equal(t, checks.LocalAction.DocURL(), local[0].DocURL)

			assert.Contains(t, readTempLockfilePins(t), "actions/setup-go")
			after := snapshotWorkflows(t)
			assert.Contains(t, after[filepath.FromSlash(clean)], "uses: ./.github/actions/local")
			action := filepath.Join(".github", "actions", "local", "action.yml")
			assert.Equal(t, before[action], after[action])
		})
	}
}

// TestRun_RemoteCompositeLocalActionWarns: a remote composite's `./…` use
// is not followed; the composite is still locked and the warning names it.
func TestRun_RemoteCompositeLocalActionWarns(t *testing.T) {
	const sha = "76f52bc884231f62b9a034ebfe128415bbaabdfc"
	reg := &httpmock.Registry{}
	reg.Register(httpmock.GraphQLForRepo("octo", "publish"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": testRepoResponse("octo/publish", sha,
			"name: Publish\nruns:\n  using: composite\n  steps:\n    - uses: ./.github/.tmp/run-in-docker\n")},
	}))
	reg.Register(httpmock.REST("GET", `repos/octo/publish$`), httpmock.JSONResponse(map[string]any{
		"default_branch": "main", "id": 2, "owner": map[string]any{"id": 1},
	}))
	path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  publish:
    runs-on: ubuntu-latest
    steps:
      - uses: octo/publish@release/v1
`)

	p, err := runParity(t, reg, "--no-interactive", "--no-narrow", path)
	require.NoError(t, err)
	local := localActionFindings(p.Findings)
	require.Len(t, local, 1, "%+v", p.Findings)
	f := local[0]
	assert.Equal(t, "warning", f.Severity)
	assert.Equal(t, "octo/publish@release/v1", f.RequiredBy)
	assert.Contains(t, f.Detail, "cannot follow workspace-local action ./.github/.tmp/run-in-docker used by octo/publish@release/v1")
	assert.Contains(t, readTempLockfilePins(t), "octo/publish@release/v1")
}

func localActionFindings(ff []format.Finding) []format.Finding {
	return slices.DeleteFunc(slices.Clone(ff), func(f format.Finding) bool { return f.Category != "local-action" })
}

// TestRun_RefusesWorkflowSymlinkOutsideCheckout: pinning rewrites workflow
// files in place, so a workflow symlinked outside the checkout stops the run
// before any request or write, whether discovered or named.
func TestRun_RefusesWorkflowSymlinkOutsideCheckout(t *testing.T) {
	const body = "name: ci\non: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/setup-go@v5\n"
	for name, explicit := range map[string]bool{"discovered": false, "named": true} {
		t.Run(name, func(t *testing.T) {
			writeTempWorkflow(t, "name: other\non: push\n")
			require.NoError(t, os.Mkdir(".git", 0o755))
			victim := filepath.Join(t.TempDir(), "victim.yml")
			require.NoError(t, os.WriteFile(victim, []byte(body), 0o600))
			link := filepath.Join(".github", "workflows", "escape.yml")
			require.NoError(t, os.Symlink(victim, link))

			var args []string
			if explicit {
				args = []string{link}
			}
			// setup-go resolves and its v5 narrows to v5.0.0, so an unguarded run
			// would rewrite the symlink's target.
			reg := &httpmock.Registry{}
			registerSetupGo(reg)
			_, _, err := runCommandWithHTTP(t, reg, append(args, "--no-interactive")...)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "refusing to use workflow")
			got, rerr := os.ReadFile(victim)
			require.NoError(t, rerr)
			assert.Equal(t, body, string(got), "the target outside the checkout is untouched")
			assert.NoFileExists(t, filepath.Join(".github", "workflows", "actions.lock"))
		})
	}
}

// TestRun_SelfRepositoryLocalActionNamesItsParent: a `./…` use inside a
// `$/…` action names that action, once per containing action.
func TestRun_SelfRepositoryLocalActionNamesItsParent(t *testing.T) {
	writeTempWorkflow(t, "name: ci\non: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: $/actions/build\n      - uses: $/actions/test\n")
	require.NoError(t, os.Mkdir(".git", 0o755))
	for _, a := range []string{"build", "test"} {
		require.NoError(t, os.MkdirAll(filepath.Join("actions", a), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join("actions", a, "action.yml"),
			[]byte("runs:\n  using: composite\n  steps:\n    - uses: ./helper\n"), 0o600))
	}

	p, err := runParity(t, &httpmock.Registry{}, "--no-interactive")
	require.NoError(t, err)
	var parents []string
	for _, f := range localActionFindings(p.Findings) {
		assert.Equal(t, "cannot follow workspace-local action ./helper used by "+f.RequiredBy, f.Detail)
		parents = append(parents, f.RequiredBy)
	}
	assert.ElementsMatch(t, []string{"$/actions/build", "$/actions/test"}, parents)

	_, stderr, err := runCommandWithHTTP(t, &httpmock.Registry{}, "--no-interactive")
	require.NoError(t, err)
	assert.Contains(t, stderr, "./helper (in $/actions/build), ./helper (in $/actions/test)")
}

// TestRun_SharedSelfActionRewriteIsLockedForEveryWorkflow: ci.yml and
// deploy.yml share $/.github/actions/restore, which uses
// octo/cache/restore by SHA. ci.yml keeps that SHA, since octo/apt
// declares it too; deploy.yml rewrites it to v2.0.0 in the shared file.
// ci.yml then runs v2.0.0, so the lockfile must cover it. The SHA stays a
// ci.yml root only while ci.yml still uses it directly; otherwise it is
// locked only as octo/apt's child.
func TestRun_SharedSelfActionRewriteIsLockedForEveryWorkflow(t *testing.T) {
	const cacheSHA, aptSHA = "cccccccccccccccccccccccccccccccccccccccc", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	byCommit := "octo/cache/restore@" + strings.ToUpper(cacheSHA)
	for name, tc := range map[string]struct {
		steps string
		roots []string
	}{
		"used directly too": {
			steps: "      - uses: " + byCommit + "\n",
			roots: []string{"octo/apt@v1.0.0", "octo/cache@" + cacheSHA, "octo/cache@v2.0.0"},
		},
		"used only in the shared action": {
			roots: []string{"octo/apt@v1.0.0", "octo/cache@v2.0.0"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			manifests := map[string]string{"octo/apt": composite("octo/cache@" + cacheSHA)}
			commits := map[string]string{"octo/cache": cacheSHA, "octo/apt": aptSHA}
			reg := &httpmock.Registry{}
			registerLockedManifests(reg, manifests)
			for nwo, tags := range map[string]any{
				"octo/cache": httpmock.TagListResponse("v2.0.0", cacheSHA, "v2", cacheSHA),
				"octo/apt":   httpmock.TagListResponse("v1.0.0", aptSHA, "v1", aptSHA),
			} {
				reg.RegisterReusable(httpmock.REST("GET", `repos/`+nwo+`/tags`), httpmock.JSONResponse(tags))
				reg.RegisterReusable(httpmock.REST("GET", `repos/`+nwo+`$`), httpmock.JSONResponse(map[string]any{"id": 1, "owner": map[string]any{"id": 1}, "default_branch": "main"}))
				reg.RegisterReusable(httpmock.REST("GET", `repos/`+nwo+`/branches`), httpmock.JSONResponse(httpmock.BranchListResponse("main", commits[nwo])))
				owner, repo, _ := strings.Cut(nwo, "/")
				httpmock.NoTagsNamed(reg, owner, repo)
			}
			registerLive(reg, manifests, commits)
			stubGraph(t, reg, nil, commits, nil)
			useDefaultTransport(t, reg)
			steps := "on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n"
			ci := writeTempWorkflow(t, steps+"      - uses: octo/apt@v1.0.0\n"+tc.steps+"      - uses: $/.github/actions/restore\n")
			require.NoError(t, os.Mkdir(".git", 0o755))
			deploy := filepath.Join(".github", "workflows", "deploy.yml")
			require.NoError(t, os.WriteFile(deploy, []byte(steps+"      - uses: $/.github/actions/restore\n"), 0o600))
			restore := filepath.Join(".github", "actions", "restore", "action.yml")
			require.NoError(t, os.MkdirAll(filepath.Dir(restore), 0o755))
			require.NoError(t, os.WriteFile(restore, []byte(composite(byCommit)), 0o600))

			_, stderr, err := runCommandWithHTTP(t, reg, "--no-interactive")
			require.NoError(t, err, stderr)
			lock := readTempLockfilePins(t)
			action, err := os.ReadFile(restore)
			require.NoError(t, err)
			assert.Contains(t, string(action), "uses: octo/cache/restore@v2.0.0\n")
			want := "'" + filepath.ToSlash(ci) + "':\n"
			for _, root := range tc.roots {
				want += "        - '" + root + "'\n"
			}
			assert.Contains(t, lock, want+"dependencies:")
			assert.Contains(t, lock, "        uses:\n            - 'octo/cache@"+cacheSHA+"'\n")
			for _, mode := range []string{"--verify-local", "--verify"} {
				_, stderr, err := runCommandWithHTTP(t, reg, mode)
				require.NoError(t, err, "%s: %s\n%s", mode, stderr, lock)
			}
		})
	}
}

// TestRun_LocalActionUsingRemoteActionsBlocks: a `./…` composite in the
// checkout that uses remote actions fails every mode, since the runner would
// reject its unlocked uses; --ignore-local-actions (or the old
// --no-migrate-local-actions spelling) turns it back into a warning.
func TestRun_LocalActionUsingRemoteActionsBlocks(t *testing.T) {
	const want = "./.github/actions/setup runs whatever is in the workspace, so the lockfile can't cover it or the actions it uses; they'll fail when the lockfile is enforced. " +
		"Pass --migrate-local-actions to lock this repo's copy as $/, or --ignore-local-actions to keep ./."
	for _, tc := range []struct {
		args    []string
		blocked bool
	}{
		{nil, true},
		{[]string{"--no-fix"}, true},
		{[]string{"--verify"}, true},
		{[]string{"--verify-local"}, true},
		{[]string{"--ignore-local-actions"}, false},
		{[]string{"--verify", "--ignore-local-actions"}, false},
		{[]string{"--verify-local", "--ignore-local-actions"}, false},
		{[]string{"--no-migrate-local-actions"}, false},
	} {
		t.Run(strings.Join(append([]string{"args"}, tc.args...), " "), func(t *testing.T) {
			path := writeTempWorkflow(t, "name: ci\non: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n"+
				"      - uses: actions/checkout@v4.2.1\n      - uses: ./.github/actions/setup\n",
				"actions/checkout@v4.2.1=sha1-"+parityTestSHA)
			require.NoError(t, os.Mkdir(".git", 0o755))
			action := filepath.Join(".github", "actions", "setup", "action.yml")
			require.NoError(t, os.MkdirAll(filepath.Dir(action), 0o755))
			require.NoError(t, os.WriteFile(action, []byte("runs:\n  using: composite\n  steps:\n"+
				"    - uses: actions/setup-node@v4\n    - uses: octo/tool@v1\n    - run: echo hi\n      shell: bash\n"), 0o600))
			before := snapshotWorkflows(t)
			reg := &httpmock.Registry{}
			reg.Register(httpmock.GraphQL(`commit: object\(oid`), parityOK)

			p, err := runParity(t, reg, append(tc.args, "--no-interactive", path)...)
			local := localActionFindings(p.Findings)
			require.Len(t, local, 1, "%+v", p.Findings)
			assert.Equal(t, checks.LocalAction.DocURL(), local[0].DocURL)
			if tc.blocked {
				require.ErrorIs(t, err, errSilent)
				assert.False(t, p.Valid)
				assert.Equal(t, "error", local[0].Severity)
				assert.Equal(t, want, local[0].Detail)
				assert.Equal(t, before, snapshotWorkflows(t), "a blocked run writes nothing")
				return
			}
			require.NoError(t, err)
			assert.True(t, p.Valid)
			assert.Equal(t, "warning", local[0].Severity)
			assert.Equal(t, "cannot follow workspace-local action ./.github/actions/setup", local[0].Detail)
		})
	}
}

// TestRun_LocalActionErrorNamesTheFix: inside a `$/…` action the `$/` rewrite
// still applies; inside a remote composite only the flag does.
func TestRun_LocalActionErrorNamesTheFix(t *testing.T) {
	for _, tc := range []struct{ parent, want string }{
		{"", "./x runs whatever is in the workspace, so the lockfile can't cover it or the actions it uses; they'll fail when the lockfile is enforced. Pass --migrate-local-actions to lock this repo's copy as $/, or --ignore-local-actions to keep ./."},
		{"$/actions/build", "./x (in $/actions/build) runs whatever is in the workspace, so the lockfile can't cover it or the actions it uses; they'll fail when the lockfile is enforced. Pass --migrate-local-actions to lock this repo's copy as $/, or --ignore-local-actions to keep ./."},
		{"octo/tool@v2", "./x (in octo/tool@v2) runs whatever is in the workspace, so the lockfile can't cover it or the actions it uses; they'll fail when the lockfile is enforced. Pass --ignore-local-actions to keep ./."},
	} {
		report := &checks.Report{Workflows: []checks.WorkflowReport{{Findings: []checks.Finding{{
			Category: checks.LocalAction, Severity: checks.SeverityWarning, LocalPath: "./x", ParentNWO: tc.parent, LocalRemoteUses: []string{"a/b@v1"},
		}}}}}
		assert.True(t, blockLocalActions(report, false))
		assert.Equal(t, tc.want, report.Workflows[0].Findings[0].Detail)
	}
}

// TestRun_LocalActionErrorLeadsTheOutput: a blocking local action is the
// first error shown, ahead of an unrelated not-pinned in an earlier
// workflow, so the real cause and its flags aren't buried.
func TestRun_LocalActionErrorLeadsTheOutput(t *testing.T) {
	for _, mode := range []string{"--verify-local", "--no-fix"} {
		t.Run(mode, func(t *testing.T) {
			writeTempWorkflow(t, "name: ci\non: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n"+
				"      - uses: actions/checkout@v4.2.1\n      - uses: ./.github/actions/setup\n",
				"actions/checkout@v4.2.1=sha1-"+parityTestSHA)
			require.NoError(t, os.WriteFile(filepath.Join(".github", "workflows", "a.yml"),
				[]byte("name: a\non: push\njobs:\n  t:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/setup-go@v5.0.0\n"), 0o600))
			require.NoError(t, os.Mkdir(".git", 0o755))
			action := filepath.Join(".github", "actions", "setup", "action.yml")
			require.NoError(t, os.MkdirAll(filepath.Dir(action), 0o755))
			require.NoError(t, os.WriteFile(action, []byte("runs:\n  using: composite\n  steps:\n    - uses: actions/setup-node@v4\n"), 0o600))
			reg := &httpmock.Registry{}
			registerSetupGo(reg)
			reg.Register(httpmock.GraphQL(`commit: object\(oid`), parityOK)

			_, stderr, err := runCommandWithHTTP(t, reg, mode, "--no-interactive")
			require.ErrorIs(t, err, errSilent)
			local := strings.Index(stderr, "./.github/actions/setup runs whatever is in the workspace")
			require.GreaterOrEqual(t, local, 0, stderr)
			assert.Contains(t, stderr, "they'll fail when the lockfile is enforced.\n    Pass --migrate-local-actions to lock this repo's copy as $/, or --ignore-local-actions to keep ./.")
			// --verify-local lists not-pinned only in the summary count.
			if other := strings.Index(stderr, "actions/setup-go"); other >= 0 {
				assert.Less(t, local, other, "the local-action error comes first:\n%s", stderr)
			}
			first := strings.Index(stderr, "✗")
			assert.Equal(t, local, strings.Index(stderr[first:], "\n")+first+3, "the first ✗ block is the local action:\n%s", stderr)
		})
	}
}

func TestPostFixReport(t *testing.T) {
	report := &checks.Report{
		Workflows: []checks.WorkflowReport{{
			Path: ".github/workflows/ci.yml",
			Findings: []checks.Finding{
				{Category: checks.NotPinned, Severity: checks.SeverityError},
				{Category: checks.FreshTag, Severity: checks.SeverityInfo},
				{Category: checks.LocalAction, Severity: checks.SeverityWarning},
			},
		}},
		RepoFindings: []checks.Finding{
			{Category: checks.Stale, Severity: checks.SeverityError},
			{Category: checks.StaleWorkflow, Severity: checks.SeverityInfo},
		},
	}
	require.False(t, report.IsValid())

	got := postFixReport(report)

	assert.True(t, got.IsValid())
	var cats []checks.Category
	for _, f := range got.Workflows[0].Findings {
		cats = append(cats, f.Category)
	}
	assert.Equal(t, []checks.Category{checks.FreshTag, checks.LocalAction}, cats, "non-blocking findings stay")
	require.Len(t, got.RepoFindings, 1)
	assert.Equal(t, checks.StaleWorkflow, got.RepoFindings[0].Category)
	assert.Len(t, report.Workflows[0].Findings, 3, "the run log keeps the pre-fix report")
	assert.Len(t, report.RepoFindings, 2)
}

func TestRun_FixJSONReportsPostFixState(t *testing.T) {
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: example/action@main
`)
	t.Setenv("GH_ACTIONS_LOCK_CONFIG", filepath.Join(t.TempDir(), "none.yml"))
	reg := &httpmock.Registry{}
	reg.Register(httpmock.GraphQLForRepo("example", "action"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": testRepoResponse("example/action", sha, nodeActionYAML)},
	}))
	reg.Register(httpmock.REST("GET", "repos/example/action$"), httpmock.JSONResponse(map[string]any{
		"id": 2, "owner": map[string]any{"id": 1}, "default_branch": "main",
	}))

	stdout, stderr, err := runCommandWithHTTP(t, reg, "--no-interactive", "--json=valid,findings,workflows,pins", path)
	require.NoError(t, err, "stderr:\n%s", stderr)

	var payload struct {
		Valid     bool              `json:"valid"`
		Findings  []format.Finding  `json:"findings"`
		Workflows []format.Workflow `json:"workflows"`
		Pins      []format.Pin      `json:"pins"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload), stdout)
	assert.True(t, payload.Valid, stdout)
	for _, f := range payload.Findings {
		assert.NotEqual(t, "error", f.Severity, "%+v", f)
	}
	require.Len(t, payload.Workflows, 1)
	assert.True(t, payload.Workflows[0].Valid)
	require.Len(t, payload.Pins, 1)
	assert.Equal(t, "example/action", payload.Pins[0].NWO)
	assert.Equal(t, sha, payload.Pins[0].SHA)
	assert.Contains(t, readTempLockfilePins(t), sha)
	reg.Verify(t)
}
