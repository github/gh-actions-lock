package pipeline

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/github/gh-actions-lock/internal/resolve"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type noopMeta struct{}

func (noopMeta) RepoIDs(context.Context, string, string, string) (int64, int64, error) {
	return 0, 0, nil
}

func TestDiagnoseOneParsed_LocalActionWarnsWithoutBlocking(t *testing.T) {
	const wfKey = ".github/workflows/ci.yml"
	onboarded, err := lockfile.LoadState(t.TempDir(), noopMeta{})
	require.NoError(t, err)
	require.NoError(t, onboarded.Set(context.Background(), wfKey, nil, nil, nil))

	for name, store := range map[string]*lockfile.State{"not onboarded": nil, "onboarded": onboarded} {
		t.Run(name, func(t *testing.T) {
			pw := checks.ParsedWorkflow{Path: wfKey, LocalPaths: []string{"./a", "./b"}}
			wr := diagnoseOneParsed(context.Background(), pw, nil, store, nil)

			var local []checks.Finding
			for _, f := range wr.Findings {
				if f.Category == checks.LocalAction {
					local = append(local, f)
				}
			}
			require.Len(t, local, 2)
			assert.Equal(t, checks.SeverityWarning, local[0].Severity)
			assert.Contains(t, local[0].Detail, "cannot follow workspace-local action ./a")
			assert.Contains(t, local[1].Detail, "./b")
			assert.True(t, wr.IsValid())
			assert.False(t, wr.NeedsAttention())
		})
	}
}

func TestNestedLocalActionFindings(t *testing.T) {
	l := &resolve.CompositeLocalPathError{Parent: "octo/tool@v2", LocalPath: "./helper"}
	got := nestedLocalActionFindings("ci.yml", []*resolve.CompositeLocalPathError{l, l})

	require.Len(t, got, 1, "one warning per composite and path")
	assert.Equal(t, checks.LocalAction, got[0].Category)
	assert.Equal(t, checks.SeverityWarning, got[0].Severity)
	assert.Equal(t, "octo/tool@v2", got[0].ParentNWO)
	assert.Contains(t, got[0].Detail, "cannot follow workspace-local action ./helper used by octo/tool@v2")
}

func TestDiagnoseOneParsed_SelfRepositoryOnly(t *testing.T) {
	pw := checks.ParsedWorkflow{
		Path:               ".github/workflows/ci.yml",
		SelfRepositoryRefs: []string{"$/actions/foo"},
	}
	wr := diagnoseOneParsed(context.Background(), pw, nil, nil, nil)

	require.Len(t, wr.Findings, 1)
	assert.Equal(t, checks.SelfRepositoryAction, wr.Findings[0].Category)
	assert.Equal(t, checks.SeverityInfo, wr.Findings[0].Severity)
}

func TestDiagnoseOneParsed_SelfRepositoryReusableWorkflow(t *testing.T) {
	pw := checks.ParsedWorkflow{
		Path:               ".github/workflows/ci.yml",
		SelfRepositoryRefs: []string{"$/.github/workflows/reusable.yml"},
	}
	wr := diagnoseOneParsed(context.Background(), pw, nil, nil, nil)

	require.Len(t, wr.Findings, 1)
	assert.Equal(t, checks.SelfRepositoryAction, wr.Findings[0].Category)
	assert.Equal(t, checks.SeverityInfo, wr.Findings[0].Severity)
}

func TestDiagnoseOneParsed_InvalidSelfRepositoryRef(t *testing.T) {
	pw := checks.ParsedWorkflow{
		Path:                  ".github/workflows/ci.yml",
		SelfRepositoryRefErrs: []string{"$/actions/foo@v1"},
	}
	wr := diagnoseOneParsed(context.Background(), pw, nil, nil, nil)

	require.Len(t, wr.Findings, 1)
	assert.Equal(t, checks.InvalidSelfRepositoryRef, wr.Findings[0].Category)
	assert.Equal(t, checks.SeverityError, wr.Findings[0].Severity)
	assert.Contains(t, wr.Findings[0].Remediation, "@ref")
}

func TestDiagnoseOneParsed_InvalidSelfRepositoryRefWithLocalAction(t *testing.T) {
	pw := checks.ParsedWorkflow{
		Path:                  ".github/workflows/ci.yml",
		LocalPaths:            []string{"./local"},
		SelfRepositoryRefErrs: []string{"$/actions/foo@v1"},
	}
	wr := diagnoseOneParsed(context.Background(), pw, nil, nil, nil)

	require.Len(t, wr.Findings, 2)
	assert.Equal(t, checks.InvalidSelfRepositoryRef, wr.Findings[0].Category)
	assert.Equal(t, checks.LocalAction, wr.Findings[1].Category)
	assert.False(t, wr.IsValid())
}

func TestDiagnoseOneParsed_SelfRepositoryResolutionError(t *testing.T) {
	pw := checks.ParsedWorkflow{
		Path:                         ".github/workflows/ci.yml",
		SelfRepositoryResolutionErrs: []string{"can't inspect self repository action $/missing"},
	}
	wr := diagnoseOneParsed(context.Background(), pw, nil, nil, nil)

	require.Len(t, wr.Findings, 1)
	assert.Equal(t, checks.InvalidSelfRepositoryRef, wr.Findings[0].Category)
	assert.Equal(t, checks.SeverityError, wr.Findings[0].Severity)
	assert.False(t, wr.IsValid())
}

func TestDiagnoseOneParsed_LoadErrorIsInvalidWorkflow(t *testing.T) {
	pw := checks.ParsedWorkflow{
		Path:    ".github/workflows/ci.yml",
		LoadErr: fmt.Errorf("yaml: line 3: did not find expected node content"),
	}
	wr := diagnoseOneParsed(context.Background(), pw, nil, nil, nil)

	require.Len(t, wr.Findings, 1)
	f := wr.Findings[0]
	assert.Equal(t, checks.InvalidWorkflow, f.Category)
	assert.Equal(t, checks.SeverityError, f.Severity)
	assert.Contains(t, f.Detail, "failed to load workflow")
	assert.Equal(t, "https://github.com/github/gh-actions-lock/blob/main/docs/troubleshooting.md#invalid-workflow", f.DocURL())
	assert.True(t, wr.SkipCommit)
	assert.False(t, wr.IsValid())
}

func TestResolveErrFinding(t *testing.T) {
	outage := &api.HTTPError{StatusCode: http.StatusServiceUnavailable}
	tests := []struct {
		name          string
		err           error
		wantCategory  checks.Category
		wantRetryable bool
	}{
		{"an outage is retryable", fmt.Errorf("actions/checkout@v4: %w", outage), checks.ReachabilityUnknown, true},
		{"an answer is not", fmt.Errorf("actions/checkout@v4: %w", &api.HTTPError{StatusCode: http.StatusForbidden}), checks.RepoUnavailable, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := resolveErrFinding(".github/workflows/ci.yml", tt.err)
			assert.Equal(t, tt.wantCategory, f.Category)
			assert.Equal(t, checks.SeverityError, f.Severity)
			assert.Equal(t, tt.wantRetryable, f.Retryable)
		})
	}
}
