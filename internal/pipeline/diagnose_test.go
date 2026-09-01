package pipeline

import (
	"context"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
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

func TestTransferredRepositoryFindingNamesRemoteComposite(t *testing.T) {
	original := parserlock.ActionRef{Owner: "old", Repo: "action", Ref: "v1"}
	wr := checks.WorkflowReport{Path: ".github/workflows/ci.yml"}

	appendTransferredRepositoryFindings(&wr, []dep.Dependency{{
		NWO:          "new/action",
		Ref:          "v1",
		OriginalRefs: []parserlock.ActionRef{original},
	}}, dep.ParentMap{
		"old/action@v1": {"root/composite@v2"},
	})

	require.Len(t, wr.Findings, 1)
	assert.Equal(t, checks.SeverityError, wr.Findings[0].Severity)
	assert.Contains(t, wr.Findings[0].Detail, "old/action")
	assert.Contains(t, wr.Findings[0].Detail, "new/action")
	assert.Contains(t, wr.Findings[0].Detail, "root/composite@v2")
	assert.Equal(t, "root/composite@v2", wr.Findings[0].ParentNWO)
	assert.Contains(t, wr.Findings[0].Remediation, "upstream composite")
}
