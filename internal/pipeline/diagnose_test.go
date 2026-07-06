package pipeline

import (
	"context"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type noopMeta struct{}

func (noopMeta) RepoIDs(context.Context, string, string) (int64, int64, error) {
	return 0, 0, nil
}

func TestDiagnoseOneParsed_LocalAction_NotOnboarded(t *testing.T) {
	pw := checks.ParsedWorkflow{
		Path:       ".github/workflows/ci.yml",
		LocalPaths: []string{"./my-local-action"},
	}
	wr := diagnoseOneParsed(context.Background(), pw, nil, nil, nil)

	assert.Len(t, wr.Findings, 1)
	assert.Equal(t, checks.LocalAction, wr.Findings[0].Category)
	assert.Equal(t, checks.SeverityWarning, wr.Findings[0].Severity)
}

func TestDiagnoseOneParsed_LocalAction_AlreadyOnboarded(t *testing.T) {
	dir := t.TempDir()
	store, err := lockfile.LoadState(dir, noopMeta{})
	require.NoError(t, err)

	wfKey := ".github/workflows/ci.yml"
	require.NoError(t, store.Set(context.Background(), wfKey, nil, nil, nil))

	pw := checks.ParsedWorkflow{
		Path:       wfKey,
		LocalPaths: []string{"./my-local-action"},
	}
	wr := diagnoseOneParsed(context.Background(), pw, nil, store, nil)

	assert.Len(t, wr.Findings, 1)
	assert.Equal(t, checks.LocalAction, wr.Findings[0].Category)
	assert.Equal(t, checks.SeverityError, wr.Findings[0].Severity)
	assert.Contains(t, wr.Findings[0].Remediation, "remove")
}

func TestDiagnoseOneParsed_SelfRepoOnly(t *testing.T) {
	pw := checks.ParsedWorkflow{
		Path:         ".github/workflows/ci.yml",
		SelfRepoRefs: []string{"$/actions/foo"},
	}
	wr := diagnoseOneParsed(context.Background(), pw, nil, nil, nil)

	require.Len(t, wr.Findings, 1)
	assert.Equal(t, checks.SelfRepoAction, wr.Findings[0].Category)
	assert.Equal(t, checks.SeverityInfo, wr.Findings[0].Severity)
	assert.True(t, wr.IsValid())
}

func TestDiagnoseOneParsed_InvalidSelfRepoRef(t *testing.T) {
	pw := checks.ParsedWorkflow{
		Path:            ".github/workflows/ci.yml",
		SelfRepoRefErrs: []string{"$/actions/foo@v1"},
	}
	wr := diagnoseOneParsed(context.Background(), pw, nil, nil, nil)

	require.Len(t, wr.Findings, 1)
	assert.Equal(t, checks.InvalidSelfRepoRef, wr.Findings[0].Category)
	assert.Equal(t, checks.SeverityError, wr.Findings[0].Severity)
	assert.False(t, wr.IsValid())
}

func TestDiagnoseOneParsed_JobLevelSelfRepoValid(t *testing.T) {
	pw := checks.ParsedWorkflow{
		Path:         ".github/workflows/ci.yml",
		SelfRepoRefs: []string{"$/.github/workflows/reusable.yml"},
	}
	wr := diagnoseOneParsed(context.Background(), pw, nil, nil, nil)

	require.Len(t, wr.Findings, 1)
	assert.Equal(t, checks.SelfRepoAction, wr.Findings[0].Category)
	assert.Equal(t, checks.SeverityInfo, wr.Findings[0].Severity)
	assert.True(t, wr.IsValid())
}

func TestDiagnoseOneParsed_JobLevelSelfRepoAtRefRejected(t *testing.T) {
	pw := checks.ParsedWorkflow{
		Path:            ".github/workflows/ci.yml",
		SelfRepoRefErrs: []string{"$/.github/workflows/reusable.yml@v1"},
	}
	wr := diagnoseOneParsed(context.Background(), pw, nil, nil, nil)

	require.Len(t, wr.Findings, 1)
	assert.Equal(t, checks.InvalidSelfRepoRef, wr.Findings[0].Category)
	assert.Equal(t, checks.SeverityError, wr.Findings[0].Severity)
	assert.False(t, wr.IsValid())
}

func TestDiagnoseOneParsed_SelfRepoMixedWithRefs(t *testing.T) {
	dir := t.TempDir()
	store, err := lockfile.LoadState(dir, noopMeta{})
	require.NoError(t, err)

	pw := checks.ParsedWorkflow{
		Path:         ".github/workflows/ci.yml",
		Refs:         []parserlock.ActionRef{{Owner: "actions", Repo: "checkout", Ref: "v4"}},
		SelfRepoRefs: []string{"$/actions/foo"},
	}
	// nil resolver → structural-only path; no lockfile entry means the
	// checkout ref is not-pinned, but the self-repo finding still lands.
	wr := diagnoseOneParsed(context.Background(), pw, nil, store, nil)

	var sawSelfRepo bool
	for _, f := range wr.Findings {
		if f.Category == checks.SelfRepoAction {
			sawSelfRepo = true
			assert.Equal(t, checks.SeverityInfo, f.Severity)
		}
	}
	assert.True(t, sawSelfRepo, "expected a self-repo-action finding")
}
