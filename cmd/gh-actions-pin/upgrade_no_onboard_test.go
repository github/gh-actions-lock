package main

// Tests for `upgrade --no-onboard` strict mode. The flag prevents a
// dependency-update run from silently adding new entries to
// lockfile.workflows{}; refused workflows surface as `onboarding-required`
// findings and the run exits non-zero. Workflows already tracked in the
// lockfile upgrade normally.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/github/gh-actions-pin/internal/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeMultiWorkflowFixture writes a scratch repo containing the named
// workflows (each populated with the shared single-uses body) and a
// lockfile that tracks only `trackedWorkflows`. The lockfile pre-pins
// actions/checkout@v5 to a known SHA so the upgrade exercise has
// something to flip. chdirs into the scratch repo.
func writeMultiWorkflowFixture(t *testing.T, workflowNames []string, trackedWorkflows []string) {
	t.Helper()

	dir := t.TempDir()
	wfDir := filepath.Join(dir, ".github", "workflows")
	require.NoError(t, os.MkdirAll(wfDir, 0o755))

	body := strings.TrimSpace(`
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
`) + "\n"

	for _, name := range workflowNames {
		require.NoError(t, os.WriteFile(filepath.Join(wfDir, name), []byte(body), 0o600))
	}

	var sb strings.Builder
	sb.WriteString("version: 'v0.0.1'\ndependencies:\n")
	sb.WriteString("  'actions/checkout@v5:sha1-93cb6efe18208431cddfb8368fd83d5badbf9bfd':\n    branch: 'main'\n    commit: 'sha1-93cb6efe18208431cddfb8368fd83d5badbf9bfd'\n    owner_id: 1\n    repo_id: 1\n")
	sb.WriteString("workflows:\n")
	for _, wf := range trackedWorkflows {
		sb.WriteString("  '.github/workflows/" + wf + "':\n    - 'actions/checkout@v5:sha1-93cb6efe18208431cddfb8368fd83d5badbf9bfd'\n")
	}
	require.NoError(t, os.WriteFile(filepath.Join(wfDir, "actions.lock"), []byte(sb.String()), 0o600))

	t.Chdir(dir)
}

// upgradeMocks wires the GraphQL+REST mocks needed to flip
// actions/checkout from v5 to v6. The mocks are permissive — they
// satisfy every call shape the upgrade path performs (tag list,
// per-ref blobs, branch list, compare, tag list for normalize) so a
// successful upgrade run does not 404. callsPerWorkflow is how many
// times each per-call stub must be available; httpmock consumes each
// stub once so a 3-workflow run needs 3 copies of every per-workflow
// matcher.
func upgradeMocks(reg *httpmock.Registry, callsPerWorkflow int) {
	for i := 0; i < callsPerWorkflow; i++ {
		reg.Register(
			httpmock.GraphQL(`refs\(refPrefix: "refs/tags/"`),
			httpmock.JSONResponse(map[string]any{
				"data": map[string]any{
					"repository": map[string]any{
						"refs": map[string]any{
							"nodes": []map[string]string{
								{"name": "v5"},
								{"name": "v6"},
							},
						},
					},
				},
			}),
		)
		reg.Register(
			httpmock.GraphQLForRepo("actions", "checkout"),
			httpmock.JSONResponse(map[string]any{
				"data": map[string]any{
					"a0": testRepoResponse("actions/checkout", "de0fac2e4500dabe0009e67214ff5f5447ce83dd", nodeActionYAML),
				},
			}),
		)
		reg.Register(
			httpmock.REST("GET", `repos/actions/checkout/branches`),
			httpmock.JSONResponse(httpmock.BranchListResponse("main", "de0fac2e4500dabe0009e67214ff5f5447ce83dd")),
		)
		reg.Register(
			httpmock.REST("GET", `repos/actions/checkout/tags`),
			httpmock.JSONResponse(httpmock.TagListResponse("v6", "de0fac2e4500dabe0009e67214ff5f5447ce83dd")),
		)
	}
}

// readLockfile returns the raw bytes of the test repo's lockfile,
// or fails the test on read error. Used to assert that a refused-only
// run leaves the lockfile byte-identical to its pre-run state.
func readLockfile(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(".github", "workflows", "actions.lock"))
	require.NoError(t, err)
	return b
}

// TestUpgrade_NoOnboard_RefusesMissingWorkflow asserts that targeting
// an untracked workflow with --no-onboard produces an onboarding-required
// finding, leaves the lockfile bytes untouched, and exits non-zero. This
// is the core contract the Dependabot CLI engine flip depends on.
func TestUpgrade_NoOnboard_RefusesMissingWorkflow(t *testing.T) {
	reg := &httpmock.Registry{}
	// Refused workflow short-circuits before any resolver calls — so we
	// don't register or Verify here, but we still assert no HTTP traffic
	// happened by leaving the registry empty.

	writeMultiWorkflowFixture(t,
		[]string{"a.yml", "b.yml"},
		[]string{"a.yml"}, // only a.yml tracked
	)

	lockBefore := readLockfile(t)

	stdout, stderr, err := runCommandWithHTTP(t, reg,
		"upgrade",
		"--action", "actions/checkout@v6",
		"--from", "v5",
		"--write",
		"--no-onboard",
		"--json=findings",
		".github/workflows/b.yml",
	)
	require.ErrorIs(t, err, errSilent, "refused upgrade must exit non-zero")

	var payload struct {
		Findings []struct {
			Workflow string `json:"workflow"`
			Category string `json:"category"`
			Severity string `json:"severity"`
		} `json:"findings"`
		Workflows []struct {
			Path string `json:"path"`
		} `json:"workflows"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload), "stdout=%q stderr=%q", stdout, stderr)
	require.Len(t, payload.Findings, 1, "expected one onboarding-required finding")
	assert.Equal(t, "onboarding-required", payload.Findings[0].Category)
	assert.Equal(t, "error", payload.Findings[0].Severity)
	assert.Equal(t, ".github/workflows/b.yml", payload.Findings[0].Workflow)
	assert.Empty(t, payload.Workflows, "workflows[] must omit refused workflow")

	lockAfter := readLockfile(t)
	assert.Equal(t, string(lockBefore), string(lockAfter), "lockfile must be byte-identical when every workflow is refused")
}

// TestUpgrade_NoOnboard_PartialSuccess asserts that a multi-workflow
// run upgrades tracked workflows while emitting onboarding-required
// findings for untracked ones, persists the partial result atomically,
// and still exits non-zero so the caller sees the blocking finding.
func TestUpgrade_NoOnboard_PartialSuccess(t *testing.T) {
	reg := &httpmock.Registry{}
	upgradeMocks(reg, 8)

	writeMultiWorkflowFixture(t,
		[]string{"a.yml", "b.yml", "c.yml"},
		[]string{"a.yml", "c.yml"}, // b.yml untracked
	)

	stdout, stderr, err := runCommandWithHTTP(t, reg,
		"upgrade",
		"--action", "actions/checkout@v6",
		"--from", "v5",
		"--write",
		"--no-onboard",
		"--json=findings",
		".github/workflows/a.yml",
		".github/workflows/b.yml",
		".github/workflows/c.yml",
	)
	require.ErrorIs(t, err, errSilent, "partial run must exit non-zero when any workflow is refused")

	var payload struct {
		Findings []struct {
			Workflow string `json:"workflow"`
			Category string `json:"category"`
		} `json:"findings"`
		Workflows []struct {
			Path string `json:"path"`
		} `json:"workflows"`
		Updated []struct {
			NWO    string `json:"nwo"`
			NewRef string `json:"new_ref"`
		} `json:"updated"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload), "stdout=%q stderr=%q", stdout, stderr)

	require.Len(t, payload.Findings, 1)
	assert.Equal(t, ".github/workflows/b.yml", payload.Findings[0].Workflow)
	assert.Equal(t, "onboarding-required", payload.Findings[0].Category)

	savedPaths := make([]string, 0, len(payload.Workflows))
	for _, w := range payload.Workflows {
		savedPaths = append(savedPaths, w.Path)
	}
	assert.ElementsMatch(t,
		[]string{".github/workflows/a.yml", ".github/workflows/c.yml"},
		savedPaths,
		"workflows[] must list only successfully-upgraded workflows",
	)

	lock := string(readLockfile(t))
	assert.Contains(t, lock, "'.github/workflows/a.yml'")
	assert.Contains(t, lock, "'.github/workflows/c.yml'")
	assert.NotContains(t, lock, "'.github/workflows/b.yml'", "refused workflow must not appear in lockfile.workflows{}")
}

// TestUpgrade_NoOnboard_AllPresent_Succeeds asserts that --no-onboard is
// transparent when every targeted workflow is already tracked — the
// strict-mode flag must not perturb the happy path Dependabot will hit
// for fully-onboarded repositories.
func TestUpgrade_NoOnboard_AllPresent_Succeeds(t *testing.T) {
	reg := &httpmock.Registry{}
	upgradeMocks(reg, 8)

	writeMultiWorkflowFixture(t,
		[]string{"a.yml", "b.yml"},
		[]string{"a.yml", "b.yml"},
	)

	stdout, _, err := runCommandWithHTTP(t, reg,
		"upgrade",
		"--action", "actions/checkout@v6",
		"--from", "v5",
		"--write",
		"--no-onboard",
		"--json=findings",
		".github/workflows/a.yml",
		".github/workflows/b.yml",
	)
	require.NoError(t, err, "fully-onboarded run must exit zero")

	var payload struct {
		Findings []map[string]any `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.Empty(t, payload.Findings, "no findings on the happy path")
}

// TestUpgrade_WithoutNoOnboard_StillOnboards locks in the legacy
// interactive behavior: a developer running `gh actions-pin upgrade`
// without --no-onboard against an untracked workflow continues to onboard
// it. The flag is opt-in strict mode for tooling (Dependabot) — humans
// keep the convenience default.
//
// This also exercises the onboarding exit-code fix: when the only effect
// of a run is to add a previously-untracked workflow to lockfile.workflows{}
// (so the diff is purely Added and contributes 0 entries to the
// updated[] JSON), the command must still exit zero. Previously this
// path emitted "No matching actions found" and exited non-zero, which
// broke scripts that treat exit code as truth.
func TestUpgrade_WithoutNoOnboard_StillOnboards(t *testing.T) {
	reg := &httpmock.Registry{}
	upgradeMocks(reg, 8)

	writeMultiWorkflowFixture(t,
		[]string{"a.yml", "b.yml"},
		[]string{"a.yml"}, // b.yml untracked
	)

	stdout, stderr, err := runCommandWithHTTP(t, reg,
		"upgrade",
		"--action", "actions/checkout@v6",
		"--from", "v5",
		"--write",
		".github/workflows/b.yml",
	)
	require.NoError(t, err, "onboarding-only upgrade must exit zero; stdout=%q stderr=%q", stdout, stderr)

	lock := string(readLockfile(t))
	assert.Contains(t, lock, "'.github/workflows/b.yml'", "default behavior must onboard the previously-untracked workflow")
}

// TestUpgrade_WithoutNoOnboard_OnboardingExitCodeJSON asserts the same
// onboarding-exit-zero contract under --json mode: when the only effect
// is onboarding (purely-Added diff), the updated[] array can legitimately
// be empty, workflows[] must include the onboarded workflow, and the
// command must exit zero.
func TestUpgrade_WithoutNoOnboard_OnboardingExitCodeJSON(t *testing.T) {
	reg := &httpmock.Registry{}
	upgradeMocks(reg, 8)

	writeMultiWorkflowFixture(t,
		[]string{"a.yml", "b.yml"},
		[]string{"a.yml"}, // b.yml untracked
	)

	stdout, stderr, err := runCommandWithHTTP(t, reg,
		"upgrade",
		"--action", "actions/checkout@v6",
		"--from", "v5",
		"--write",
		"--json=updated,workflows",
		".github/workflows/b.yml",
	)
	require.NoError(t, err, "onboarding-only upgrade must exit zero; stdout=%q stderr=%q", stdout, stderr)

	var payload struct {
		Updated []struct {
			NWO string `json:"nwo"`
		} `json:"updated"`
		Workflows []struct {
			Path string `json:"path"`
		} `json:"workflows"`
		Findings []map[string]any `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload), "stdout=%q", stdout)

	// updated[] is legitimately empty: the diff is purely Added (onboarding),
	// and we don't dump transitive deps into updated[] to preserve the
	// downstream JSON contract.
	assert.Empty(t, payload.Updated, "onboarding-only run yields empty updated[]")
	require.Len(t, payload.Workflows, 1, "onboarded workflow must appear in workflows[]")
	assert.Equal(t, ".github/workflows/b.yml", payload.Workflows[0].Path)
	assert.Empty(t, payload.Findings, "no findings on a clean onboarding run")

	lock := string(readLockfile(t))
	assert.Contains(t, lock, "'.github/workflows/b.yml'")
}
