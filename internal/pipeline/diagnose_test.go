package pipeline

import (
	"context"
	"testing"

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
