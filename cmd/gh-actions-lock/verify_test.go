package main

import (
	"fmt"
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

func TestVerifyLocalNormalizesHostname(t *testing.T) {
	for _, source := range []string{"flag", "environment", "repository"} {
		t.Run(source, func(t *testing.T) {
			path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: tenant/internal@v1
`, "tenant/internal@v1=sha1-"+tenantSHA)
			t.Setenv("GH_HOST", "")
			t.Setenv("GH_REPO", "")
			args := []string{"--verify-local", "--no-interactive", "--json", path}
			switch source {
			case "flag":
				args = append(args, "--hostname", "Tenant.GHE.com")
			case "environment":
				t.Setenv("GH_HOST", "Tenant.GHE.com")
			case "repository":
				t.Setenv("GH_REPO", "Tenant.GHE.com/tenant/repo")
			}
			before, err := os.ReadFile(path)
			require.NoError(t, err)
			lockBefore := readTempLockfilePins(t)
			reg := &httpmock.Registry{}
			defer reg.Verify(t)
			stdout, _, err := runCommandWithHTTP(t, reg, args...)
			require.NoError(t, err)
			assert.Contains(t, stdout, `"valid": true`)
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, before, after)
			assert.Equal(t, lockBefore, readTempLockfilePins(t))
		})
	}
}

func TestApplyVerifyFlags(t *testing.T) {
	opts := checkOptions{verify: true}
	applyVerifyFlags(&opts)
	assert.True(t, opts.noFix, "verify implies no-fix")

	opts = checkOptions{}
	applyVerifyFlags(&opts)
	assert.False(t, opts.noFix)
}

func TestValidateOutputFlags_VerifyConflicts(t *testing.T) {
	tests := []struct {
		name    string
		opts    checkOptions
		wantErr string
	}{
		{
			name:    "verify and verify-local are mutually exclusive",
			opts:    checkOptions{verify: true, verifyLocal: true},
			wantErr: "mutually exclusive",
		},
		{
			name:    "verify-local and accept-moved conflict",
			opts:    checkOptions{verifyLocal: true, acceptMoved: true},
			wantErr: "offline",
		},
		{
			name:    "verify-local and relock conflict",
			opts:    checkOptions{verifyLocal: true, relock: true},
			wantErr: "offline",
		},
		{
			name: "verify alone is valid",
			opts: checkOptions{verify: true},
		},
		{
			name: "verify-local alone is valid",
			opts: checkOptions{verifyLocal: true},
		},
		{
			name: "neither is valid",
			opts: checkOptions{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.opts.validateOutputFlags()
			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestVerifyLocalWarnsAboutLocalActions(t *testing.T) {
	for _, extra := range [][]string{nil, {"--no-migrate-local-actions"}, {"--no-fix"}} {
		for _, jsonOut := range []bool{true, false} {
			t.Run(strings.Join(append(extra, fmt.Sprintf("json=%v", jsonOut)), " "), func(t *testing.T) {
				path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: tenant/internal@v1
      - uses: ./my-action
  call:
    uses: ./.github/workflows/reusable.yml
`, "tenant/internal@v1=sha1-"+tenantSHA)
				require.NoError(t, os.Mkdir(".git", 0o755))
				require.NoError(t, os.Mkdir("my-action", 0o755))
				require.NoError(t, os.WriteFile(filepath.Join("my-action", "action.yml"), []byte("runs:\n  using: composite\n  steps: []\n"), 0o644))
				require.NoError(t, os.WriteFile(filepath.Join(".github", "workflows", "reusable.yml"), []byte("on: workflow_call\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo\n"), 0o644))
				before, err := os.ReadFile(path)
				require.NoError(t, err)
				lockBefore := readTempLockfilePins(t)

				args := append([]string{"--verify-local", "--no-interactive"}, extra...)
				if jsonOut {
					args = append(args, "--json=valid,findings")
				}
				stdout, stderr, err := runCommandWithHTTP(t, &httpmock.Registry{}, append(args, path)...)

				require.NoError(t, err, stderr)
				if jsonOut {
					assert.Contains(t, stdout, `"valid": true`)
					assert.Contains(t, stdout, `"category": "local-action"`)
					assert.Contains(t, stdout, `"severity": "warning"`)
				} else {
					assert.Contains(t, stdout+stderr, "./my-action")
					assert.Contains(t, stdout+stderr, "Cannot follow 1 workspace-local action")
					assert.Contains(t, stdout+stderr, checks.LocalAction.DocURL())
				}
				after, err := os.ReadFile(path)
				require.NoError(t, err)
				assert.Equal(t, string(before), string(after))
				action, err := os.ReadFile(filepath.Join("my-action", "action.yml"))
				require.NoError(t, err)
				assert.NotContains(t, string(action), "$/")
				assert.Equal(t, lockBefore, readTempLockfilePins(t))
			})
		}
	}
}

// TestStaleEntry_SeverityMatchesVerdict pins the contract consumers rely on:
// `valid` is false, and read-only modes exit non-zero, exactly when a finding
// has error severity. An unused lockfile entry is an error in every mode; a
// fix run prunes it and succeeds.
func TestStaleEntry_SeverityMatchesVerdict(t *testing.T) {
	const workflow = `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4.2.1
`
	for _, tc := range []struct {
		name    string
		mode    []string
		wantErr bool
	}{
		{name: "verify fails", mode: []string{"--verify"}, wantErr: true},
		{name: "verify-local fails", mode: []string{"--verify-local"}, wantErr: true},
		{name: "fix run prunes", wantErr: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempWorkflow(t, workflow,
				"actions/checkout@v4.2.1=sha1-"+parityTestSHA,
				"actions/setup-go@v5=sha1-"+parityTestSHA,
			)
			reg := &httpmock.Registry{}
			args := append(slices.Clone(tc.mode), "--no-interactive", path)

			p, err := runParity(t, reg, args...)
			if tc.wantErr {
				require.ErrorIs(t, err, errSilent)
			} else {
				require.NoError(t, err)
			}

			var stale []format.Finding
			hasError := false
			for _, f := range p.Findings {
				hasError = hasError || f.Severity == string(checks.SeverityError)
				if f.Category == string(checks.Stale) {
					stale = append(stale, f)
				}
			}
			assert.Equal(t, !hasError, p.Valid, "valid must agree with severities")
			if !tc.wantErr {
				assert.Empty(t, stale, "a fix run reports the state it left")
				assert.True(t, p.Valid)
			} else {
				require.Len(t, stale, 1, "%+v", p.Findings)
				assert.Equal(t, "actions/setup-go@v5", stale[0].Dependency)
				assert.Equal(t, string(checks.SeverityError), stale[0].Severity)
			}

			pins := readTempLockfilePins(t)
			if tc.wantErr {
				assert.Contains(t, pins, "actions/setup-go@v5", "read-only modes must not prune")
			} else {
				assert.NotContains(t, pins, "actions/setup-go@v5", "fix run prunes the unused entry")
			}
		})
	}
}

// TestStaleEntry_TerminalShowsRemediation checks a human sees the finding
// and how to clear it in both read-only modes.
func TestStaleEntry_TerminalShowsRemediation(t *testing.T) {
	for _, mode := range []string{"--verify", "--verify-local"} {
		t.Run(mode, func(t *testing.T) {
			path := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4.2.1
`,
				"actions/checkout@v4.2.1=sha1-"+parityTestSHA,
				"actions/setup-go@v5=sha1-"+parityTestSHA,
			)
			_, stderr, err := runCommandWithHTTP(t, &httpmock.Registry{}, mode, "--no-interactive", path)
			require.ErrorIs(t, err, errSilent)
			assert.Contains(t, stderr, "Unused lockfile entry actions/setup-go@v5")
			assert.Contains(t, stderr, "without "+mode)
		})
	}
}

// TestVerifyLocal_LocalActionDoesNotExcuseMissingPins: a local action is
// only a warning; the workflow's remote refs are still enforced.
func TestVerifyLocal_LocalActionDoesNotExcuseMissingPins(t *testing.T) {
	writeTempWorkflow(t, "name: ci\non: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n"+
		"      - uses: actions/checkout@v4.2.1\n      - uses: actions/setup-go@v5\n      - uses: ./my-action\n",
		"actions/checkout@v4.2.1=sha1-"+parityTestSHA)

	p, err := runParity(t, &httpmock.Registry{}, "--verify-local", "--no-interactive")
	require.ErrorIs(t, err, errSilent)
	assert.False(t, p.Valid)
	categories := map[string]string{}
	for _, f := range p.Findings {
		categories[f.Category] = f.Severity
	}
	assert.Equal(t, string(checks.SeverityWarning), categories[string(checks.LocalAction)])
	assert.Equal(t, string(checks.SeverityError), categories[string(checks.NotPinned)])
}

// TestEmptyWorkflow_LockfileEntryRequired: enforcement requires every
// workflow to be listed, so a workflow with no uses: still needs an entry
// and that entry must not carry pins.
func TestEmptyWorkflow_LockfileEntryRequired(t *testing.T) {
	const workflow = `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: echo ok
`
	const pin = "actions/checkout@v4=sha1-" + parityTestSHA
	for _, tc := range []struct {
		name         string
		mode         []string
		otherPins    []string // lockfile covers only other.yml
		ownPins      []string // lockfile covers workflow.yml
		wantCategory checks.Category
		wantErr      bool
		wantEntry    string
	}{
		{name: "verify untracked", mode: []string{"--verify"}, otherPins: []string{pin}, wantCategory: checks.NotPinned, wantErr: true},
		{name: "verify-local untracked", mode: []string{"--verify-local"}, otherPins: []string{pin}, wantCategory: checks.NotPinned, wantErr: true},
		{name: "fix run adds entry", otherPins: []string{pin}, wantEntry: "'.github/workflows/workflow.yml': []"},
		{name: "no-onboard leaves it", mode: []string{"--no-onboard"}, otherPins: []string{pin}, wantCategory: checks.OnboardingRequired},
		{name: "verify no-onboard opts out", mode: []string{"--verify", "--no-onboard"}, otherPins: []string{pin}, wantCategory: checks.OnboardingRequired},
		{name: "verify unused pin", mode: []string{"--verify"}, ownPins: []string{pin}, wantCategory: checks.Stale, wantErr: true},
		{name: "verify-local unused pin", mode: []string{"--verify-local"}, ownPins: []string{pin}, wantCategory: checks.Stale, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempWorkflow(t, workflow, tc.ownPins...)
			if len(tc.otherPins) > 0 {
				writeTempLockfile(t, ".", "other.yml", tc.otherPins)
			}
			args := append(slices.Clone(tc.mode), "--no-interactive", path)

			p, err := runParity(t, &httpmock.Registry{}, args...)
			if tc.wantErr {
				require.ErrorIs(t, err, errSilent)
			} else {
				require.NoError(t, err)
			}

			hasError := false
			var cats []string
			for _, f := range p.Findings {
				hasError = hasError || f.Severity == string(checks.SeverityError)
				cats = append(cats, f.Category)
				assert.Equal(t, checks.Category(f.Category).DocURL(), f.DocURL, "%s links its own section", f.Category)
			}
			assert.Equal(t, tc.wantErr, hasError, "%+v", p.Findings)
			assert.Equal(t, !hasError, p.Valid, "valid must agree with severities")
			if tc.wantCategory != "" {
				assert.Contains(t, cats, string(tc.wantCategory))
			}
			if tc.wantEntry != "" {
				assert.Contains(t, readTempLockfilePins(t), tc.wantEntry)
			}
		})
	}
}

// TestVerifyModes_SameLockfileError: --verify and --verify-local report an
// invalid lockfile with the same words.
func TestVerifyModes_SameLockfileError(t *testing.T) {
	writeFullSHAWorkflow(t, "refs/heads/main")
	var msgs []string
	for _, mode := range []string{"--verify", "--verify-local"} {
		_, _, err := runCommandWithHTTP(t, &httpmock.Registry{}, mode, "--no-interactive")
		require.Error(t, err, mode)
		assert.NotErrorIs(t, err, errSilent, mode)
		msgs = append(msgs, err.Error())
	}
	assert.Equal(t, msgs[0], msgs[1])
	assert.Contains(t, msgs[1], "restore it from version control")
}

// TestSHARecordedRef_OutOfScopeBlocks: a path-scoped read-only run still
// fails on an own-SHA entry of a workflow it didn't scan, pointing at a
// full-scan run, and writes nothing.
func TestSHARecordedRef_OutOfScopeBlocks(t *testing.T) {
	for _, mode := range []string{"--verify", "--no-fix", "--verify-local"} {
		t.Run(mode, func(t *testing.T) {
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
			require.NoError(t, os.WriteFile(other, []byte("on: push\njobs:\n  t:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@"+parityTestSHA+"\n"), 0o600))
			lock := "version: 'v0.0.3'\nworkflows:\n" +
				"    '.github/workflows/other.yml':\n        - 'actions/checkout@" + parityTestSHA + "'\n" +
				"    '.github/workflows/workflow.yml':\n        - 'actions/checkout@v6.1.0'\n" +
				"dependencies:\n" +
				"    'actions/checkout@" + parityTestSHA + "':\n        ref: '" + parityTestSHA + "'\n        commit: 'sha1-" + parityTestSHA + "'\n        owner_id: 1\n        repo_id: 1\n" +
				"    'actions/checkout@v6.1.0':\n        ref: 'v6.1.0'\n        commit: 'sha1-" + parityTestSHA + "'\n        owner_id: 1\n        repo_id: 1\n"
			lockPath := filepath.Join(".github", "workflows", "actions.lock")
			require.NoError(t, os.WriteFile(lockPath, []byte(lock), 0o600))

			_, stderr, err := runCommandWithHTTP(t, parityFallback{&httpmock.Registry{}}, mode, "--no-interactive", ci)
			require.ErrorIs(t, err, errSilent)
			assert.Contains(t, stderr, "run `gh actions-lock` without workflow paths", "the terminal names the fix")

			p, err := runParity(t, parityFallback{&httpmock.Registry{}}, mode, "--no-interactive", ci)
			require.ErrorIs(t, err, errSilent)
			assert.False(t, p.Valid)
			var blocking []format.Finding
			for _, f := range p.Findings {
				if f.Severity == "error" {
					blocking = append(blocking, f)
				}
			}
			require.Len(t, blocking, 1, "findings: %+v", p.Findings)
			assert.Equal(t, "not-pinned", blocking[0].Category)
			assert.Contains(t, blocking[0].Detail, "lock entry for actions/checkout@"+parityTestSHA+", in .github/workflows/other.yml, records ref "+parserlock.ShortSHA(parityTestSHA))
			assert.Equal(t, "run `gh actions-lock` without workflow paths to record the tag or branch this commit came from", blocking[0].Remediation)
			got, err := os.ReadFile(lockPath)
			require.NoError(t, err)
			assert.Equal(t, lock, string(got))
		})
	}
}

// TestSHARecordedRef_DeletedWorkflowBlocks: a full-scan read-only run fails
// on an own-SHA entry of a workflow that no longer exists, like one it
// didn't select, and writes nothing.
func TestSHARecordedRef_DeletedWorkflowBlocks(t *testing.T) {
	for _, mode := range []string{"--verify", "--no-fix", "--verify-local"} {
		t.Run(mode, func(t *testing.T) {
			writeTempWorkflow(t, "name: ci\non: push\njobs:\n  t:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo\n")
			lock := "version: 'v0.0.3'\nworkflows:\n" +
				"    '.github/workflows/gone.yml':\n        - 'actions/checkout@" + parityTestSHA + "'\n" +
				"    '.github/workflows/workflow.yml': []\n" +
				"dependencies:\n" +
				"    'actions/checkout@" + parityTestSHA + "':\n        ref: '" + parityTestSHA + "'\n        commit: 'sha1-" + parityTestSHA + "'\n        owner_id: 1\n        repo_id: 1\n"
			lockPath := filepath.Join(".github", "workflows", "actions.lock")
			require.NoError(t, os.WriteFile(lockPath, []byte(lock), 0o600))

			p, err := runParity(t, parityFallback{&httpmock.Registry{}}, mode, "--no-interactive")
			require.ErrorIs(t, err, errSilent)
			assert.False(t, p.Valid)
			var blocking []format.Finding
			for _, f := range p.Findings {
				if f.Severity == "error" {
					blocking = append(blocking, f)
				}
			}
			require.Len(t, blocking, 1, "findings: %+v", p.Findings)
			assert.Contains(t, blocking[0].Detail, "lock entry for actions/checkout@"+parityTestSHA+", in .github/workflows/gone.yml, records ref")
			got, err := os.ReadFile(lockPath)
			require.NoError(t, err)
			assert.Equal(t, lock, string(got))
		})
	}
}

// TestVerify_PassesWithLocalActions: an onboarded workflow that also uses a
// `./…` action passes --verify; the local action is only a warning.
func TestVerify_PassesWithLocalActions(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(httpmock.GraphQL(`commit: object\(oid`), parityOK)
	path := writeTempWorkflow(t, "name: ci\non: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n"+
		"      - uses: actions/checkout@v4.2.1\n      - uses: ./my-action\n",
		"actions/checkout@v4.2.1=sha1-"+parityTestSHA)

	p, err := runParity(t, reg, "--verify", path)
	require.NoError(t, err)
	assert.True(t, p.Valid)
	require.Len(t, p.Findings, 1, "%+v", p.Findings)
	assert.Equal(t, string(checks.LocalAction), p.Findings[0].Category)
	assert.Equal(t, string(checks.SeverityWarning), p.Findings[0].Severity)
}
