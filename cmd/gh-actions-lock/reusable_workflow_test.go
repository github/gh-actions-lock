package main

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReusableWorkflowCoverage(t *testing.T) {
	for _, remote := range []bool{false, true} {
		for _, mode := range []string{"--verify-local", "--verify", "--no-fix", ""} {
			if remote && mode != "--verify" {
				continue
			}
			for _, ref := range []string{"main", "v1.2.3", "0123456", "", remoteSHA} {
				t.Run(fmt.Sprintf("remote=%v/%s/%s", remote, mode, ref), func(t *testing.T) {
					const callee = "acme/templates/.github/workflows/build.yml"
					workflow := "jobs:\n  call:\n    uses: " + callee + "@" + ref + "\n"
					wf := writeTempWorkflow(t, workflow)
					lock := "version: '" + parserlock.Version + "'\nworkflows:\n  '" + wf + "': []\n"
					require.NoError(t, os.WriteFile(parserlock.Path, []byte(lock), 0o600))
					before := snapshotWorkflows(t)
					calls := 0
					var rt http.RoundTripper = roundTripFunc(func(req *http.Request) (*http.Response, error) {
						calls++
						return nil, fmt.Errorf("reusable workflow must not be resolved as a caller action: %s", req.URL)
					})
					args := []string{}
					if mode != "" {
						args = append(args, mode)
					}
					if remote {
						rr := newRemoteRepo(map[string]string{wf: workflow, parserlock.Path: lock}, nil)
						rr.fallback = rt
						rt = rr
						isolateRemoteWrites(t)
						args = append(args, "-R", "octo/api")
					} else {
						args = append(args, wf)
					}
					p, err := runParity(t, rt, args...)
					if ref == remoteSHA {
						require.NoError(t, err)
						assert.True(t, p.Valid)
					} else {
						require.ErrorIs(t, err, errSilent)
						assert.False(t, p.Valid)
					}
					require.Len(t, p.Findings, 1)
					assert.Equal(t, "not-pinned", p.Findings[0].Category)
					if ref == remoteSHA {
						assert.Equal(t, "warning", p.Findings[0].Severity)
						assert.Contains(t, p.Findings[0].Detail, "own lockfile and dependencies are not verified")
					} else {
						assert.Equal(t, "error", p.Findings[0].Severity)
						assert.Contains(t, p.Findings[0].Remediation, "pin jobs.<id>.uses to a full SHA")
					}
					assert.Zero(t, calls, "caller lockfile enforcement must not be claimed for a callee")
					if !remote {
						after := snapshotWorkflows(t)
						assert.Equal(t, before[wf], after[wf])
						if mode != "" || ref != remoteSHA {
							assert.Equal(t, before, after)
						} else {
							file, err := parserlock.Parse([]byte(after[parserlock.Path]))
							require.NoError(t, err)
							assert.Empty(t, file.Dependencies)
						}
					}
				})
			}
		}
	}
}

func TestReusableWorkflowCallerLockCannotExcuseMutableRef(t *testing.T) {
	for _, mode := range []string{"--verify-local", "--verify", ""} {
		t.Run(mode, func(t *testing.T) {
			wf := writeTempWorkflow(t, "jobs:\n  call:\n    uses: acme/templates/.github/workflows/build.yml@main\n",
				"acme/templates@main=sha1-"+remoteSHA)
			before := snapshotWorkflows(t)
			args := []string{wf}
			if mode != "" {
				args = append([]string{mode}, args...)
			}
			p, err := runParity(t, &httpmock.Registry{}, args...)
			require.ErrorIs(t, err, errSilent)
			assert.False(t, p.Valid)
			require.NotEmpty(t, p.Findings)
			assert.Contains(t, p.Findings[0].Remediation, "pin jobs.<id>.uses to a full SHA")
			assert.Equal(t, before, snapshotWorkflows(t))
		})
	}
}

func TestReusableWorkflowWarningReachesTerminal(t *testing.T) {
	wf := writeTempWorkflow(t, "jobs:\n  call:\n    uses: acme/templates/.github/workflows/build.yml@"+remoteSHA+"\n")
	lock := "version: '" + parserlock.Version + "'\nworkflows:\n  '" + wf + "': []\n"
	require.NoError(t, os.WriteFile(parserlock.Path, []byte(lock), 0o600))
	_, stderr, err := runCommandWithHTTP(t, &httpmock.Registry{}, "--verify-local", wf)
	require.NoError(t, err)
	assert.Contains(t, stderr, "own lockfile and dependencies are not verified")
	assert.NotContains(t, stderr, "not yet pinned")
}

func TestReusableWorkflowRemediationDoesNotPromiseAutomaticFix(t *testing.T) {
	wf := writeTempWorkflow(t, "jobs:\n  call:\n    uses: acme/templates/.github/workflows/build.yml@main\n")
	_, stderr, err := runCommandWithHTTP(t, &httpmock.Registry{}, "--verify", wf)
	require.ErrorIs(t, err, errSilent)
	assert.Contains(t, stderr, "pin jobs.<id>.uses to a full SHA")
	assert.False(t, strings.Contains(stderr, "re-run without"), stderr)
}
