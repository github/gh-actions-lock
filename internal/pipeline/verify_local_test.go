package pipeline

import (
	"os"
	"path/filepath"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testLockfileContent = `version: 'v0.0.2'
dependencies:
  'actions/checkout@v4':
    ref: 'v4'
    commit: 'sha1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
    owner_id: 1
    repo_id: 1
workflows:
  '.github/workflows/ci.yml':
    - 'actions/checkout@v4'
  '.github/workflows/self.yml': []
  '.github/workflows/empty.yml': []
`

func writeTestLockfile(t *testing.T, dir, content string) *lockfile.State {
	t.Helper()
	lockDir := filepath.Join(dir, ".github", "workflows")
	require.NoError(t, os.MkdirAll(lockDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(lockDir, "actions.lock"), []byte(content), 0o644))
	store, err := lockfile.LoadState(dir, nil)
	require.NoError(t, err)
	return store
}

func TestVerifyLocalCoverage_AllPinned(t *testing.T) {
	dir := t.TempDir()
	store := writeTestLockfile(t, dir, testLockfileContent)

	parsed := []checks.ParsedWorkflow{
		{
			Path: ".github/workflows/ci.yml",
			Refs: []parserlock.ActionRef{
				{Owner: "actions", Repo: "checkout", Ref: "v4"},
			},
			ExistingDeps: []dep.Dependency{
				{NWO: "actions/checkout", Ref: "v4", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			},
		},
	}

	report := VerifyLocalCoverage(parsed, store)
	assert.True(t, report.IsValid(), "all pinned should be valid")
	assert.Empty(t, report.Workflows[0].Findings)
}

func TestVerifyLocalCoverage_SelfRepositoryOnlyIsValid(t *testing.T) {
	dir := t.TempDir()
	store := writeTestLockfile(t, dir, testLockfileContent)

	parsed := []checks.ParsedWorkflow{
		{
			Path:               ".github/workflows/self.yml",
			SelfRepositoryRefs: []string{"$/actions/foo"},
		},
	}

	report := VerifyLocalCoverage(parsed, store)
	assert.True(t, report.IsValid(), "self repository refs are inherently pinned")
	assert.Empty(t, report.Workflows[0].Findings)
}

func TestVerifyLocalCoverage_InvalidSelfRepositoryRef(t *testing.T) {
	dir := t.TempDir()
	store := writeTestLockfile(t, dir, testLockfileContent)

	parsed := []checks.ParsedWorkflow{
		{
			Path:                  ".github/workflows/self.yml",
			SelfRepositoryRefErrs: []string{"$/actions/foo@v1"},
		},
	}

	report := VerifyLocalCoverage(parsed, store)
	assert.False(t, report.IsValid(), "`$/…@ref` must fail offline verification")
	require.Len(t, report.Workflows[0].Findings, 1)
	assert.Equal(t, checks.InvalidSelfRepositoryRef, report.Workflows[0].Findings[0].Category)
	assert.Equal(t, checks.SeverityError, report.Workflows[0].Findings[0].Severity)
}

func TestVerifyLocalCoverage_InvalidSelfRepositoryRefWithPinnedRemote(t *testing.T) {
	dir := t.TempDir()
	store := writeTestLockfile(t, dir, testLockfileContent)

	parsed := []checks.ParsedWorkflow{
		{
			Path: ".github/workflows/ci.yml",
			Refs: []parserlock.ActionRef{
				{Owner: "actions", Repo: "checkout", Ref: "v4"},
			},
			SelfRepositoryRefErrs: []string{"$/actions/foo@v1"},
			ExistingDeps: []dep.Dependency{
				{NWO: "actions/checkout", Ref: "v4", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			},
		},
	}

	report := VerifyLocalCoverage(parsed, store)

	assert.False(t, report.IsValid())
	require.Len(t, report.Workflows[0].Findings, 1)
	assert.Equal(t, checks.InvalidSelfRepositoryRef, report.Workflows[0].Findings[0].Category)
}

func TestVerifyLocalCoverage_SelfRepositoryResolutionError(t *testing.T) {
	dir := t.TempDir()
	store := writeTestLockfile(t, dir, testLockfileContent)

	parsed := []checks.ParsedWorkflow{{
		Path:                         ".github/workflows/self.yml",
		SelfRepositoryResolutionErrs: []string{"can't inspect self repository action $/missing"},
	}}

	report := VerifyLocalCoverage(parsed, store)

	assert.False(t, report.IsValid())
	require.Len(t, report.Workflows[0].Findings, 1)
	assert.Equal(t, checks.InvalidSelfRepositoryRef, report.Workflows[0].Findings[0].Category)
}

func TestVerifyLocalCoverage_MissingPin(t *testing.T) {
	dir := t.TempDir()
	store := writeTestLockfile(t, dir, testLockfileContent)

	parsed := []checks.ParsedWorkflow{
		{
			Path: ".github/workflows/ci.yml",
			Refs: []parserlock.ActionRef{
				{Owner: "actions", Repo: "checkout", Ref: "v4"},
				{Owner: "actions", Repo: "setup-go", Ref: "v5"},
			},
			ExistingDeps: []dep.Dependency{
				{NWO: "actions/checkout", Ref: "v4", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			},
		},
	}

	report := VerifyLocalCoverage(parsed, store)
	assert.False(t, report.IsValid(), "missing pin should fail")

	var notPinned []checks.Finding
	for _, f := range report.Workflows[0].Findings {
		if f.Category == checks.NotPinned {
			notPinned = append(notPinned, f)
		}
	}
	assert.Len(t, notPinned, 1)
	assert.Contains(t, notPinned[0].Detail, "setup-go")
}

// TestVerifyLocalCoverage_EmptyWorkflow: lockfile enforcement covers every
// workflow, so one with no uses: still needs an entry.
func TestVerifyLocalCoverage_EmptyWorkflow(t *testing.T) {
	store := writeTestLockfile(t, t.TempDir(), testLockfileContent)

	t.Run("tracked is valid", func(t *testing.T) {
		report := VerifyLocalCoverage([]checks.ParsedWorkflow{{Path: ".github/workflows/empty.yml"}}, store)
		assert.True(t, report.IsValid())
		assert.Empty(t, report.Workflows[0].Findings)
	})

	t.Run("untracked is a fixable error", func(t *testing.T) {
		report := VerifyLocalCoverage([]checks.ParsedWorkflow{{Path: ".github/workflows/new.yml"}}, store)
		assert.False(t, report.IsValid())
		require.Len(t, report.Workflows[0].Findings, 1)
		f := report.Workflows[0].Findings[0]
		assert.Equal(t, checks.NotPinned, f.Category)
		assert.Equal(t, checks.SeverityError, f.Severity)
		assert.True(t, f.IsRemediableNotPinned())
	})

	t.Run("unused pin is stale", func(t *testing.T) {
		report := VerifyLocalCoverage([]checks.ParsedWorkflow{{Path: ".github/workflows/ci.yml"}}, store)
		assert.False(t, report.IsValid())
		require.Len(t, report.Workflows[0].Findings, 1)
		assert.Equal(t, checks.Stale, report.Workflows[0].Findings[0].Category)
	})
}

func TestVerifyLocalCoverage_LoadError(t *testing.T) {
	dir := t.TempDir()
	store := writeTestLockfile(t, dir, testLockfileContent)

	parsed := []checks.ParsedWorkflow{
		{
			Path:    ".github/workflows/bad.yml",
			LoadErr: assert.AnError,
		},
	}

	report := VerifyLocalCoverage(parsed, store)
	assert.False(t, report.IsValid())
	assert.Equal(t, checks.NotPinned, report.Workflows[0].Findings[0].Category)
	assert.False(t, report.Workflows[0].Findings[0].IsRemediableNotPinned())
}

func TestVerifyLocalCoverage_DepsError(t *testing.T) {
	dir := t.TempDir()
	store := writeTestLockfile(t, dir, testLockfileContent)

	parsed := []checks.ParsedWorkflow{
		{
			Path:    ".github/workflows/ci.yml",
			DepsErr: assert.AnError,
		},
	}

	report := VerifyLocalCoverage(parsed, store)
	assert.False(t, report.IsValid())
	assert.Equal(t, checks.NotPinned, report.Workflows[0].Findings[0].Category)
	assert.True(t, report.Workflows[0].Findings[0].IsRemediableNotPinned())
	assert.Contains(t, report.Workflows[0].Findings[0].Detail, "failed to read dependencies")
}

func TestVerifyLocalCoverage_LocalPathsMatchVerify(t *testing.T) {
	store := writeTestLockfile(t, t.TempDir(), testLockfileContent)

	for _, tc := range []struct {
		path     string
		severity checks.Severity
	}{
		{".github/workflows/ci.yml", checks.SeverityInfo},
		{".github/workflows/new.yml", checks.SeverityInfo},
	} {
		t.Run(tc.path, func(t *testing.T) {
			pw := checks.ParsedWorkflow{
				Path:       tc.path,
				Refs:       []parserlock.ActionRef{{Owner: "actions", Repo: "checkout", Ref: "v4"}},
				LocalPaths: []string{"./my-action"},
			}
			report := VerifyLocalCoverage([]checks.ParsedWorkflow{pw}, store)
			require.NotEmpty(t, report.Workflows[0].Findings)
			f := report.Workflows[0].Findings[0]
			assert.Equal(t, checks.LocalAction, f.Category)
			assert.Equal(t, tc.severity, f.Severity)
			assert.Equal(t, localActionFinding(pw), f)
		})
	}
}

func TestVerifyLocalCoverage_ValidFollowsSeverity(t *testing.T) {
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	lock := `version: 'v0.0.2'
dependencies:
  'actions/checkout@` + sha + `':
    ref: 'main'
    commit: 'sha1-` + sha + `'
    owner_id: 1
    repo_id: 1
  'actions/setup-go@v5':
    ref: 'v5'
    commit: 'sha1-` + sha + `'
    owner_id: 1
    repo_id: 2
workflows:
  '.github/workflows/ci.yml':
    - 'actions/checkout@` + sha + `'
`
	for _, tc := range []struct {
		name      string
		extraPins string
		wantCats  []checks.Category
		wantValid bool
	}{
		{name: "sha-as-ref warning only is valid", wantCats: []checks.Category{checks.ShaAsRef}, wantValid: true},
		{name: "unused entry is an error", extraPins: "    - 'actions/setup-go@v5'\n", wantCats: []checks.Category{checks.ShaAsRef, checks.Stale}, wantValid: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := writeTestLockfile(t, t.TempDir(), lock+tc.extraPins)
			parsed := []checks.ParsedWorkflow{{
				Path: ".github/workflows/ci.yml",
				Refs: []parserlock.ActionRef{{Owner: "actions", Repo: "checkout", Ref: sha}},
			}}

			report := VerifyLocalCoverage(parsed, store)

			var cats []checks.Category
			hasError := false
			for _, f := range report.Workflows[0].Findings {
				cats = append(cats, f.Category)
				hasError = hasError || f.Severity == checks.SeverityError
				if f.Category == checks.Stale {
					assert.Equal(t, checks.SeverityError, f.Severity)
				}
			}
			assert.ElementsMatch(t, tc.wantCats, cats)
			assert.Equal(t, tc.wantValid, report.IsValid())
			assert.Equal(t, !hasError, report.IsValid(), "valid must agree with severities")
		})
	}
}
