package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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

func TestVerifyLocalReportsLocalActionsWithoutWriting(t *testing.T) {
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

				require.NoError(t, err)
				if jsonOut {
					assert.Contains(t, stdout, `"valid": true`)
					assert.Contains(t, stdout, `"category": "local-action"`)
					assert.Contains(t, stdout, `"severity": "info"`)
				} else {
					out := stdout + stderr
					assert.Contains(t, out, "never followed or locked")
					assert.NotContains(t, out, "Run without --verify-local")
					assert.NotContains(t, out, "Migrated")
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
			require.Len(t, stale, 1, "%+v", p.Findings)
			assert.Equal(t, "actions/setup-go@v5", stale[0].Dependency)
			assert.Equal(t, string(checks.SeverityError), stale[0].Severity)
			assert.Equal(t, !hasError, p.Valid, "valid must agree with severities")

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

// Local actions do not excuse missing workflow entries or block tracked workflows.
func TestVerifyLocal_LocalActionVerdictFollowsSeverity(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		pins    []string
		other   bool // lockfile exists but covers another workflow
		wantErr bool
	}{
		{
			name:    "missing workflow entry still fails",
			body:    "      - uses: ./my-action\n",
			other:   true,
			wantErr: true,
		},
		{
			name: "onboarded local actions are allowed",
			body: "      - uses: actions/checkout@v4.2.1\n      - uses: ./my-action\n",
			pins: []string{"actions/checkout@v4.2.1=sha1-" + parityTestSHA},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeTempWorkflow(t, "name: ci\non: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n"+tc.body, tc.pins...)
			if tc.other {
				writeTempLockfile(t, ".", "other.yml", []string{"actions/checkout@v4.2.1=sha1-" + parityTestSHA})
			}
			require.NoError(t, os.Mkdir(".git", 0o755))
			require.NoError(t, os.Mkdir("my-action", 0o755))
			require.NoError(t, os.WriteFile(filepath.Join("my-action", "action.yml"), []byte("runs:\n  using: composite\n  steps: []\n"), 0o644))
			checkErr := func(err error) {
				if tc.wantErr {
					require.ErrorIs(t, err, errSilent)
				} else {
					require.NoError(t, err)
				}
			}

			p, err := runParity(t, &httpmock.Registry{}, "--verify-local", "--no-interactive")
			checkErr(err)
			assert.Equal(t, !tc.wantErr, p.Valid)
			require.NotEmpty(t, p.Findings)
			assert.Equal(t, string(checks.LocalAction), p.Findings[0].Category)
			assert.Equal(t, string(checks.SeverityInfo), p.Findings[0].Severity)
			assert.Contains(t, p.Findings[0].Detail, "uses: dependencies are not covered")
			if tc.wantErr {
				require.Len(t, p.Findings, 2)
				assert.Equal(t, string(checks.NotPinned), p.Findings[1].Category)
			}

			stdout, stderr, err := runCommandWithHTTP(t, &httpmock.Registry{}, "--verify-local", "--no-interactive")
			checkErr(err)
			assert.Contains(t, stdout+stderr, "never followed or locked")
		})
	}
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
		{name: "fix run adds entry", otherPins: []string{pin}, wantCategory: checks.NotPinned, wantEntry: "'.github/workflows/workflow.yml': []"},
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
			}
			// Fix runs report the pre-fix diagnosis, so only the read-only
			// modes tie errors to the exit code.
			if len(tc.mode) > 0 {
				assert.Equal(t, tc.wantErr, hasError, "%+v", p.Findings)
			}
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
