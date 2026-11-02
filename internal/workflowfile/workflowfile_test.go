package workflowfile

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/uses"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadAndExtractActionRefs(t *testing.T) {
	f, err := Load("testdata/simple.yml")
	require.NoError(t, err)

	scan := f.ExtractActionRefs()
	refs, localPaths := scan.Refs, scan.LocalPaths
	assert.Len(t, refs, 2)
	assert.Empty(t, localPaths)

	assert.Equal(t, "actions/checkout", refs[0].NWO())
	assert.Equal(t, "v4", refs[0].Ref)
	assert.Equal(t, "actions/setup-go", refs[1].NWO())
	assert.Equal(t, "v5", refs[1].Ref)
}

func TestExtractActionRefsMixed(t *testing.T) {
	f, err := Load("testdata/mixed_refs.yml")
	require.NoError(t, err)

	scan := f.ExtractActionRefs()
	refs, localPaths := scan.Refs, scan.LocalPaths
	assert.Len(t, refs, 3)
	assert.Equal(t, "actions/checkout", refs[0].NWO())
	assert.Equal(t, "actions/cache", refs[1].NWO())
	assert.Equal(t, "save", refs[1].Path)
	assert.Equal(t, "actions/setup-node", refs[2].NWO())

	assert.Len(t, localPaths, 1)
	assert.Equal(t, "./local-action", localPaths[0])
}

func TestParseRejectsUsesExpression(t *testing.T) {
	for name, content := range map[string]string{
		"step level": `
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: ${{ format('actions/checkout@v4') }}
`,
		"job level": `
on: push
jobs:
  call:
    uses: owner/repo/.github/workflows/x.yml@${{ github.ref_name }}
`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse("wf.yml", []byte(content))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "`uses:` can't contain an expression")
		})
	}
}

func TestExtractActionRefs_SelfRepositoryClassification(t *testing.T) {
	content := []byte(`
name: ci
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: $/
      - uses: $/actions/foo
      - uses: $/actions/foo
      - uses: "$/actions/expression@${{ matrix.ref }}"
      - uses: ./local-action
  call:
    uses: $/.github/workflows/reusable.yml
  bad:
    uses: $/actions/foo@v1
  bad-expression:
    uses: "$/.github/workflows/reusable.yml@${{ inputs.ref }}"
`)
	f, err := Parse("ci.yml", content)
	require.NoError(t, err)

	scan := f.ExtractActionRefs()

	// Remote ref parsed normally.
	require.Len(t, scan.Refs, 1)
	assert.Equal(t, "actions/checkout", scan.Refs[0].NWO())

	// `./` still collected as a local path.
	assert.Equal(t, []string{"./local-action"}, scan.LocalPaths)

	// Bare `$/…` valid at both step and job level, deduplicated.
	assert.ElementsMatch(t,
		[]string{"$/", "$/actions/foo", "$/.github/workflows/reusable.yml"},
		scan.SelfRepositoryRefs,
	)
	assert.Equal(t, []string{"$/", "$/actions/foo"}, scan.SelfRepositoryActionRefs)

	// `$/…@ref` is the invalid form.
	assert.ElementsMatch(t,
		[]string{
			"$/actions/expression@${{ matrix.ref }}",
			"$/actions/foo@v1",
			"$/.github/workflows/reusable.yml@${{ inputs.ref }}",
		},
		scan.SelfRepositoryRefErrs,
	)
}

func TestExtractActionRefs_SelfRepositoryOnly(t *testing.T) {
	content := []byte(`
name: ci
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: $/actions/foo
`)
	f, err := Parse("ci.yml", content)
	require.NoError(t, err)

	scan := f.ExtractActionRefs()
	assert.Empty(t, scan.Refs)
	assert.Empty(t, scan.LocalPaths)
	assert.Equal(t, []string{"$/actions/foo"}, scan.SelfRepositoryRefs)
	assert.Empty(t, scan.SelfRepositoryRefErrs)
}

func TestExtractActionRefs_SelfRepositoryActionAfterReusableWorkflow(t *testing.T) {
	content := []byte(`
name: ci
on: push
jobs:
  call:
    uses: $/shared
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: $/shared
`)
	f, err := Parse("ci.yml", content)
	require.NoError(t, err)

	scan := f.ExtractActionRefs()

	assert.Equal(t, []string{"$/shared"}, scan.SelfRepositoryRefs)
	assert.Equal(t, []string{"$/shared"}, scan.SelfRepositoryActionRefs)
}

func TestScanSelfRepositoryActions_RecursiveClosure(t *testing.T) {
	repoRoot := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(repoRoot, ".git"), 0o755))
	workflowPath := filepath.Join(repoRoot, ".github", "workflows", "ci.yml")
	require.NoError(t, os.MkdirAll(filepath.Dir(workflowPath), 0o755))
	require.NoError(t, os.WriteFile(workflowPath, []byte("name: ci\n"), 0o644))

	writeAction := func(path, content string) {
		actionPath := filepath.Join(repoRoot, path, "action.yml")
		require.NoError(t, os.MkdirAll(filepath.Dir(actionPath), 0o755))
		require.NoError(t, os.WriteFile(actionPath, []byte(content), 0o644))
	}
	writeAction("actions/root", `runs:
  using: composite
  steps:
    - uses: $/actions/child
    - uses: vendor/tool@v1
    - uses: ./workspace-relative
`)
	writeAction("actions/child", `runs:
  using: composite
  steps:
    - uses: $/actions/root
    - uses: actions/checkout@v4
    - uses: ./workspace-relative
    - uses: docker://alpine:3.18
    - uses: docker://alpine:3.18
    - uses: docker://alpine@sha256:0123456789abcdef
    - uses: $/actions/bad@v2
    - uses: "$/actions/expression@${{ matrix.ref }}"
`)

	scan := ScanSelfRepositoryActions(workflowPath, []string{"$/actions/root"})

	assert.ElementsMatch(t, []string{"vendor/tool@v1", "actions/checkout@v4"}, actionRefStrings(scan.Refs))
	assert.ElementsMatch(t, []string{"$/actions/child", "$/actions/root"}, scan.SelfRepositoryRefs)
	assert.ElementsMatch(t,
		[]string{"$/actions/bad@v2", "$/actions/expression@${{ matrix.ref }}"},
		scan.SelfRepositoryRefErrs,
	)
	assert.Equal(t, []LocalUse{
		{Path: "./workspace-relative", Parent: "$/actions/root"},
		{Path: "./workspace-relative", Parent: "$/actions/child"},
	}, scan.LocalUses, "one entry per containing action")
	assert.Empty(t, scan.Errors)
	assert.Equal(t, []uses.Image{{Name: "alpine:3.18", Where: "$/actions/child"}}, scan.Images)
}

func TestScanSelfRepositoryActions_NoRepoRoot(t *testing.T) {
	scan := ScanSelfRepositoryActions(filepath.Join(t.TempDir(), "ci.yml"), []string{"$/actions/root"})

	require.Len(t, scan.Errors, 1)
	assert.Contains(t, scan.Errors[0], "not in a git repository")
	assert.Empty(t, scan.Refs)
}

func TestScanSelfRepositoryActions_RejectsSymlinkEscape(t *testing.T) {
	repoRoot := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(repoRoot, ".git"), 0o755))
	workflowPath := filepath.Join(repoRoot, ".github", "workflows", "ci.yml")
	require.NoError(t, os.MkdirAll(filepath.Dir(workflowPath), 0o755))
	require.NoError(t, os.WriteFile(workflowPath, []byte("name: ci\n"), 0o644))

	outsideRoot := t.TempDir()
	outsideAction := filepath.Join(outsideRoot, "action.yml")
	require.NoError(t, os.WriteFile(outsideAction, []byte(`runs:
  using: composite
  steps:
    - uses: actions/checkout@v4
`), 0o644))
	actionDir := filepath.Join(repoRoot, "actions", "escape")
	require.NoError(t, os.MkdirAll(actionDir, 0o755))
	if err := os.Symlink(outsideAction, filepath.Join(actionDir, "action.yml")); err != nil {
		t.Skipf("creating symlink: %v", err)
	}

	scan := ScanSelfRepositoryActions(workflowPath, []string{"$/actions/escape"})

	require.Len(t, scan.Errors, 1)
	assert.Contains(t, scan.Errors[0], "outside repository root")
	assert.Empty(t, scan.Refs)
}

func actionRefStrings(refs []parserlock.ActionRef) []string {
	values := make([]string, len(refs))
	for i, ref := range refs {
		values[i] = ref.FullName() + "@" + ref.Ref
	}
	return values
}

func TestDiscoverWorkflowsIn(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ci.yml"), []byte("name: ci\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "deploy.yaml"), []byte("name: deploy\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("# ignore\n"), 0o644))

	paths, err := DiscoverWorkflowsIn(dir)
	require.NoError(t, err)
	assert.Len(t, paths, 2)
	assert.Equal(t, filepath.Join(dir, "ci.yml"), paths[0])
	assert.Equal(t, filepath.Join(dir, "deploy.yaml"), paths[1])
}

func TestDiscoverWorkflowsIn_MissingDir(t *testing.T) {
	paths, err := DiscoverWorkflowsIn(filepath.Join(t.TempDir(), "nope"))
	require.NoError(t, err)
	assert.Nil(t, paths)
}

func TestParseIgnoresNonUsesKeys(t *testing.T) {
	f, err := Parse("wf.yml", []byte(`
on: push
env:
  uses: ${{ github.repository }}
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          uses: ${{ github.ref }}
`))
	require.NoError(t, err)
	assert.Len(t, f.ExtractActionRefs().Refs, 1)
}

func TestExtractActionRefs_ParallelStepGroups(t *testing.T) {
	f, err := Parse("wf.yml", []byte(`
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - parallel:
          - uses: actions/setup-java@v4
          - uses: actions/setup-node@v4
          - parallel:
              - uses: actions/setup-go@v4
`))
	require.NoError(t, err)
	var names []string
	for _, r := range f.ExtractActionRefs().Refs {
		names = append(names, r.FullName())
	}
	assert.ElementsMatch(t, []string{"actions/checkout", "actions/setup-java", "actions/setup-node"}, names, "parallel groups are one level deep")
}

func TestParseRejectsUsesExpressionInParallel(t *testing.T) {
	_, err := Parse("wf.yml", []byte(`
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - parallel:
          - uses: ${{ matrix.action }}
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "`uses:` can't contain an expression")
}

func TestExtractActionRefs_ParallelStepAliases(t *testing.T) {
	f, err := Parse("wf.yml", []byte(`
on: push
x-group: &grp
  - uses: a/one@v1
  - &step
    uses: a/two@v1
jobs:
  group-alias:
    runs-on: x
    steps:
      - parallel: *grp
  step-alias:
    runs-on: x
    steps:
      - parallel:
          - *step
          - uses: a/three@v1
  job-anchor: &job
    runs-on: x
    steps:
      - parallel:
          - uses: a/four@v1
  job-alias: *job
`))
	require.NoError(t, err)
	var keys []string
	for _, r := range f.ExtractActionRefs().Refs {
		keys = append(keys, r.FullName()+"@"+r.Ref)
	}
	assert.ElementsMatch(t, []string{"a/one@v1", "a/two@v1", "a/three@v1", "a/four@v1"}, keys)
}

func TestExtractActionRefs_ParallelStepClassification(t *testing.T) {
	f, err := Parse("wf.yml", []byte(`
on: push
jobs:
  build:
    runs-on: x
    steps:
      - uses: a/dup@v1
      - parallel:
          - uses: a/dup@v1
          - uses: a/dup/sub@v1
            with:
              uses: a/not-a-ref@v1
          - uses: docker://alpine:3
          - uses: ./local/action
          - uses: $/self/action
          - uses: $/self/versioned@main
          - run: echo hi
`))
	require.NoError(t, err)
	scan := f.ExtractActionRefs()

	var keys []string
	for _, r := range scan.Refs {
		keys = append(keys, r.FullName()+"@"+r.Ref)
	}
	assert.ElementsMatch(t, []string{"a/dup@v1", "a/dup/sub@v1"}, keys, "remote refs are deduplicated across parallel and plain steps")
	assert.Equal(t, []string{"./local/action"}, scan.LocalPaths)
	assert.Equal(t, []string{"$/self/action"}, scan.SelfRepositoryActionRefs, "parallel steps are step-level")
	assert.Equal(t, []string{"$/self/versioned@main"}, scan.SelfRepositoryRefErrs)
}

func TestExtractActionRefs_MalformedParallelValues(t *testing.T) {
	tests := map[string]string{
		"null":             "      - parallel:\n",
		"scalar":           "      - parallel: a/x@v1\n",
		"mapping":          "      - parallel:\n          uses: a/x@v1\n",
		"sequence of text": "      - parallel:\n          - a/x@v1\n",
	}
	for name, steps := range tests {
		t.Run(name, func(t *testing.T) {
			f, err := Parse("wf.yml", []byte("on: push\njobs:\n  b:\n    runs-on: x\n    steps:\n"+steps))
			require.NoError(t, err)
			assert.Empty(t, f.ExtractActionRefs().Refs)
		})
	}
}

func TestExtractActionRefs_CompositeIgnoresParallel(t *testing.T) {
	f, err := Parse("action.yml", []byte(`
runs:
  using: composite
  steps:
    - uses: actions/checkout@v4
    - parallel:
        - uses: actions/setup-node@v4
`))
	require.NoError(t, err)
	var names []string
	for _, r := range f.ExtractActionRefs().Refs {
		names = append(names, r.FullName())
	}
	assert.Equal(t, []string{"actions/checkout"}, names)
}

func TestParseRejectsAliasedUsesExpression(t *testing.T) {
	_, err := Parse("wf.yml", []byte(`
on: push
x: &step
  uses: ${{ matrix.action }}
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - *step
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "`uses:` can't contain an expression")
}

func TestScanSelfRepositoryActionsRejectsUsesExpression(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".git"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "actions", "compo"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "actions", "compo", "action.yml"), []byte(`
runs:
  using: composite
  steps:
    - uses: actions/checkout@${{ inputs.ref }}
`), 0o644))

	scan := ScanSelfRepositoryActions(filepath.Join(root, ".github", "workflows", "ci.yml"), []string{"$/actions/compo"})
	require.Len(t, scan.Errors, 1)
	assert.Contains(t, scan.Errors[0], "`uses:` can't contain an expression")
	assert.Empty(t, scan.Refs)
}

func TestFindRepoRoot_EnvOverride(t *testing.T) {
	gitRepo := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(gitRepo, ".git"), 0o755))
	partial := t.TempDir()
	outside := t.TempDir()

	tests := []struct {
		name     string
		env      string
		start    string
		wantRoot string
		wantErr  string
	}{
		{name: "unset walks up to .git", start: filepath.Join(gitRepo, ".github", "workflows", "ci.yml"), wantRoot: gitRepo},
		{name: "unset without .git returns empty", start: filepath.Join(partial, ".github", "workflows", "ci.yml")},
		{name: "set returns override without .git", env: partial, start: filepath.Join(partial, ".github", "workflows", "ci.yml"), wantRoot: partial},
		{name: "set wins over .git", env: filepath.Join(gitRepo, ".github"), start: filepath.Join(gitRepo, ".github", "workflows", "ci.yml"), wantRoot: filepath.Join(gitRepo, ".github")},
		{name: "nonexistent dir fails", env: filepath.Join(partial, "missing"), start: filepath.Join(partial, ".github", "workflows", "ci.yml"), wantErr: RepoRootEnv},
		{name: "start outside root fails", env: partial, start: filepath.Join(outside, ".github", "workflows", "ci.yml"), wantErr: "does not contain"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.NoError(t, os.MkdirAll(filepath.Dir(tt.start), 0o755))
			t.Setenv(RepoRootEnv, tt.env)

			root, err := FindRepoRoot(tt.start)

			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				assert.Empty(t, root)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantRoot, root)
		})
	}
}

// Callers run from the partial checkout with the override pointing at it,
// possibly as "." or through a symlinked path such as macOS /tmp.
func TestFindRepoRoot_EnvOverrideMatchesCwd(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(real, link))
	require.NoError(t, os.MkdirAll(filepath.Join(real, ".github", "workflows"), 0o755))

	tests := []struct {
		name  string
		cwd   string
		env   string
		start string
	}{
		{name: "relative dot root", cwd: real, env: ".", start: filepath.Join(".github", "workflows", "ci.yml")},
		{name: "symlinked root, real start", cwd: real, env: link, start: filepath.Join(".github", "workflows", "ci.yml")},
		{name: "real root, symlinked start", cwd: link, env: real, start: filepath.Join(".github", "workflows", "ci.yml")},
		{name: "symlinked root, absolute real start", cwd: real, env: link, start: filepath.Join(real, ".github", "workflows", "ci.yml")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Chdir(tt.cwd)
			t.Setenv(RepoRootEnv, tt.env)

			root, err := FindRepoRoot(tt.start)

			require.NoError(t, err)
			want, err := filepath.EvalSymlinks(real)
			require.NoError(t, err)
			got, err := filepath.EvalSymlinks(root)
			require.NoError(t, err)
			assert.Equal(t, want, got)
		})
	}
}

func TestScanSelfRepositoryActions_EnvRootBoundsEscape(t *testing.T) {
	parent := t.TempDir()
	repoRoot := filepath.Join(parent, "repo")
	workflowPath := filepath.Join(repoRoot, ".github", "workflows", "ci.yml")
	require.NoError(t, os.MkdirAll(filepath.Dir(workflowPath), 0o755))
	sibling := filepath.Join(parent, "sibling", "action.yml")
	require.NoError(t, os.MkdirAll(filepath.Dir(sibling), 0o755))
	require.NoError(t, os.WriteFile(sibling, []byte("runs:\n  using: composite\n  steps:\n    - uses: evil/x@v1\n"), 0o644))
	t.Setenv(RepoRootEnv, repoRoot)

	scan := ScanSelfRepositoryActions(workflowPath, []string{"$/../sibling"})

	require.Len(t, scan.Errors, 1)
	assert.Contains(t, scan.Errors[0], "outside repo root")
	assert.Empty(t, scan.Refs)
}

// TestExtractActionRefs_ReusableWorkflowOnlyAtJobLevel: a reusable workflow
// referenced from a step is malformed; at jobs.<id>.uses it is not an action
// and is left alone.
func TestExtractActionRefs_ReusableWorkflowOnlyAtJobLevel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ci.yml")
	body := "on: push\njobs:\n" +
		"  call:\n    uses: octo/workflows/.github/workflows/release.yml@v1\n" +
		"  build:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout/.github/workflows/ci.yml@v4\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	f, err := Load(path)
	require.NoError(t, err)

	scan := f.ExtractActionRefs()
	require.Len(t, scan.MalformedRefs, 1)
	assert.Equal(t, "uses: actions/checkout/.github/workflows/ci.yml@v4 is a reusable workflow; reusable workflows should be referenced at the top-level `jobs.<job_id>.uses` key, not within steps", scan.MalformedRefs[0])
	assert.Empty(t, scan.Refs)
}

func TestValidateInRepo(t *testing.T) {
	repoRoot := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(repoRoot, ".git"), 0o755))
	dir := filepath.Join(repoRoot, ".github", "workflows")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	inside := filepath.Join(dir, "ci.yml")
	require.NoError(t, os.WriteFile(inside, []byte("name: ci\n"), 0o644))
	outside := filepath.Join(t.TempDir(), "victim.yml")
	require.NoError(t, os.WriteFile(outside, []byte("name: victim\n"), 0o644))
	escape := filepath.Join(dir, "escape.yml")
	require.NoError(t, os.Symlink(outside, escape))
	internal := filepath.Join(dir, "alias.yml")
	require.NoError(t, os.Symlink(inside, internal))

	assert.NoError(t, ValidateInRepo(inside))
	assert.NoError(t, ValidateInRepo(internal), "a symlink within the checkout is fine")
	assert.NoError(t, ValidateInRepo(filepath.Join(dir, "missing.yml")), "nothing to escape yet")
	assert.ErrorIs(t, ValidateInRepo(escape), errPathOutsideRoot)
}

func TestLocalActionRemoteUses(t *testing.T) {
	repoRoot := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(repoRoot, ".git"), 0o755))
	workflowPath := filepath.Join(repoRoot, ".github", "workflows", "ci.yml")
	require.NoError(t, os.MkdirAll(filepath.Dir(workflowPath), 0o755))
	write := func(path, content string) {
		p := filepath.Join(repoRoot, path, "action.yml")
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
	}
	write("remote", "runs:\n  using: composite\n  steps:\n    - uses: actions/setup-node@v4\n    - uses: ./other\n    - uses: $/self\n    - uses: docker://alpine:3\n    - uses: actions/setup-node@v4\n    - uses: octo/tool/sub@v1\n")
	write("runonly", "runs:\n  using: composite\n  steps:\n    - run: echo hi\n      shell: bash\n")
	write("node", "runs:\n  using: node20\n  main: index.js\n")

	for _, tc := range []struct {
		name, path string
		want       []string
	}{
		{"remote uses, deduplicated", "./remote", []string{"actions/setup-node@v4", "octo/tool/sub@v1"}},
		{"run steps only", "./runonly", nil},
		{"not a composite", "./node", nil},
		{"missing from the checkout", "./generated", nil},
		{"outside the checkout", "./../escape", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, LocalActionRemoteUses(workflowPath, tc.path))
		})
	}
}

func TestMigrateLocalActionsToSelfRepository(t *testing.T) {
	repoRoot := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(repoRoot, ".git"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(repoRoot, ".github", "workflows"), 0o755))

	// An in-repo composite action exists → eligible for migration.
	require.NoError(t, os.Mkdir(filepath.Join(repoRoot, "local-action"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "local-action", "action.yml"),
		[]byte("runs:\n  using: composite\n"), 0o644))

	workflowPath := filepath.Join(repoRoot, ".github", "workflows", "ci.yml")
	content := []byte("jobs:\n  build:\n    steps:\n" +
		"      - uses: ./local-action\n" +
		"      - uses: ./missing-action\n")
	require.NoError(t, os.WriteFile(workflowPath, content, 0o644))

	f, err := Load(workflowPath)
	require.NoError(t, err)

	out, changed, err := f.MigrateLocalActionsToSelfRepository()
	require.NoError(t, err)

	// Only the path with an in-repo action file is rewritten.
	assert.Equal(t, 1, changed)
	assert.Contains(t, string(out), "uses: $/local-action")
	assert.Contains(t, string(out), "uses: ./missing-action")
}

func TestMigrateLocalActionsToSelfRepository_NestedCheckout(t *testing.T) {
	tests := []struct {
		name  string
		uses  string
		setup func(t *testing.T, repoRoot string)
	}{
		{name: "leaves action inside submodule", uses: "./vendor/sub/action"},
		{
			name: "leaves symlinked dir pointing into submodule",
			uses: "./alias",
			setup: func(t *testing.T, repoRoot string) {
				require.NoError(t, os.Symlink(filepath.Join("vendor", "sub", "action"), filepath.Join(repoRoot, "alias")))
			},
		},
		{
			name: "leaves action.yml symlinked into submodule",
			uses: "./shim",
			setup: func(t *testing.T, repoRoot string) {
				shim := filepath.Join(repoRoot, "shim")
				require.NoError(t, os.Mkdir(shim, 0o755))
				require.NoError(t, os.Symlink(filepath.Join("..", "vendor", "sub", "action", "action.yml"), filepath.Join(shim, "action.yml")))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoRoot := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(repoRoot, ".git"), 0o755))
			require.NoError(t, os.MkdirAll(filepath.Join(repoRoot, ".github", "workflows"), 0o755))
			subAction := filepath.Join(repoRoot, "vendor", "sub", "action")
			require.NoError(t, os.MkdirAll(subAction, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "vendor", "sub", ".git"), []byte("gitdir: ../../.git/modules/sub\n"), 0o644))
			require.NoError(t, os.WriteFile(filepath.Join(subAction, "action.yml"), []byte("runs:\n  using: composite\n"), 0o644))
			if tt.setup != nil {
				tt.setup(t, repoRoot)
			}

			workflowPath := filepath.Join(repoRoot, ".github", "workflows", "ci.yml")
			content := []byte("jobs:\n  build:\n    steps:\n      - uses: " + tt.uses + "\n")
			require.NoError(t, os.WriteFile(workflowPath, content, 0o644))
			f, err := Load(workflowPath)
			require.NoError(t, err)

			out, changed, err := f.MigrateLocalActionsToSelfRepository()

			require.NoError(t, err)
			assert.Zero(t, changed, "a submodule action isn't part of this repository")
			assert.Equal(t, content, out)
		})
	}
}

func TestMigrateLocalActionsToSelfRepository_NoLocalPaths(t *testing.T) {
	content := []byte("jobs:\n  build:\n    steps:\n      - uses: actions/checkout@v4\n")
	f, err := Parse("ci.yml", content)
	require.NoError(t, err)

	out, changed, err := f.MigrateLocalActionsToSelfRepository()
	require.NoError(t, err)
	assert.Equal(t, 0, changed)
	assert.Equal(t, content, out)
}

func TestMigrateLocalActionsToSelfRepository_RejectsStructuralErrors(t *testing.T) {
	repoRoot := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(repoRoot, ".git"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(repoRoot, "local-action"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "local-action", "action.yml"),
		[]byte("runs:\n  using: composite\n"), 0o644))

	tests := []struct {
		name    string
		invalid string
	}{
		{"forbidden ref", "$/actions/bad@v1"},
		{"expression ref", "$/actions/bad@${{ matrix.ref }}"},
		{"missing action", "$/actions/missing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workflowPath := filepath.Join(repoRoot, ".github", "workflows", strings.ReplaceAll(tt.name, " ", "-")+".yml")
			content := []byte("jobs:\n  build:\n    steps:\n" +
				"      - uses: ./local-action\n" +
				"      - uses: '" + tt.invalid + "'\n")
			f, err := Parse(workflowPath, content)
			require.NoError(t, err)

			out, changed, err := f.MigrateLocalActionsToSelfRepository()

			require.Error(t, err)
			assert.Zero(t, changed)
			assert.Equal(t, content, out)
		})
	}
}

func TestDiscoverCompositeActionFiles(t *testing.T) {
	root := t.TempDir()
	write := func(rel string) {
		full := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte("runs:\n  using: composite\n"), 0o644))
	}
	write("action.yml")
	write("nested/deep/action.yaml")
	write(".github/actions/thing/action.yml")
	write("not-an-action.yml")
	// A stray file inside .git must be skipped.
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".git", "action.yml"), []byte("x"), 0o644))

	got, err := DiscoverCompositeActionFiles(root)
	require.NoError(t, err)

	want := []string{
		filepath.Join(root, ".github/actions/thing/action.yml"),
		filepath.Join(root, "action.yml"),
		filepath.Join(root, "nested/deep/action.yaml"),
	}
	assert.Equal(t, want, got)
}

func TestDiscoverCompositeActionFiles_NestedCheckouts(t *testing.T) {
	gitFile := func(t *testing.T, dir string) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: /elsewhere\n"), 0o644))
	}
	gitDir := func(t *testing.T, dir string) {
		require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o755))
	}
	danglingGit := func(t *testing.T, dir string) {
		require.NoError(t, os.Symlink("missing", filepath.Join(dir, ".git")))
	}
	tests := []struct {
		name     string
		markRoot func(*testing.T, string)
		nested   string
		markDir  func(*testing.T, string)
		wantSeen bool
	}{
		{name: "skips nested worktree", markRoot: gitDir, nested: ".gwt/some-wt", markDir: gitFile},
		{name: "skips nested clone", markRoot: gitDir, nested: "vendor/clone", markDir: gitDir},
		{name: "walks root that is itself a worktree", markRoot: gitFile, nested: "sub", wantSeen: true},
		{name: "walks checkout below a root without .git", nested: "src", markDir: gitDir, wantSeen: true},
		{name: "walks dir whose .git is a dangling symlink", markRoot: gitDir, nested: "fixture", markDir: danglingGit, wantSeen: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if tt.markRoot != nil {
				tt.markRoot(t, root)
			}
			dir := filepath.Join(root, tt.nested)
			require.NoError(t, os.MkdirAll(dir, 0o755))
			if tt.markDir != nil {
				tt.markDir(t, dir)
			}
			action := filepath.Join(dir, "action.yml")
			require.NoError(t, os.WriteFile(action, []byte("runs:\n  using: composite\n"), 0o644))

			got, err := DiscoverCompositeActionFiles(root)

			require.NoError(t, err)
			assert.Equal(t, tt.wantSeen, slices.Contains(got, action))
		})
	}
}

func TestDiscoverCompositeActionFiles_SymlinkIntoNestedCheckout(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, ".git"), 0o755))
	sub := filepath.Join(root, "vendor", "sub")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sub, ".git"), []byte("gitdir: /elsewhere\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(sub, "action.yml"), []byte("runs:\n  using: composite\n"), 0o644))
	own := filepath.Join(root, "own")
	require.NoError(t, os.Mkdir(own, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(own, "action.yml"), []byte("runs:\n  using: composite\n"), 0o644))
	shim := filepath.Join(root, "shim")
	require.NoError(t, os.Mkdir(shim, 0o755))
	require.NoError(t, os.Symlink(filepath.Join("..", "vendor", "sub", "action.yml"), filepath.Join(shim, "action.yml")))
	inRepo := filepath.Join(root, "alias")
	require.NoError(t, os.Mkdir(inRepo, 0o755))
	require.NoError(t, os.Symlink(filepath.Join("..", "own", "action.yml"), filepath.Join(inRepo, "action.yml")))

	got, err := DiscoverCompositeActionFiles(root)

	require.NoError(t, err)
	assert.NotContains(t, got, filepath.Join(shim, "action.yml"), "symlink into a nested checkout")
	assert.Contains(t, got, filepath.Join(inRepo, "action.yml"), "symlink within this checkout")
}

func TestDiscoverCompositeActionFiles_EmptyRoot(t *testing.T) {
	got, err := DiscoverCompositeActionFiles("")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestDiscoverCompositeActionFiles_RejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "action.yml")
	require.NoError(t, os.WriteFile(outside, []byte("runs:\n  using: composite\n"), 0o644))
	actionDir := filepath.Join(root, "actions", "escape")
	require.NoError(t, os.MkdirAll(actionDir, 0o755))
	if err := os.Symlink(outside, filepath.Join(actionDir, "action.yml")); err != nil {
		t.Skipf("creating symlink: %v", err)
	}

	paths, err := DiscoverCompositeActionFiles(root)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "outside repository root")
	assert.Empty(t, paths)
}

func TestParseErrorKinds(t *testing.T) {
	tests := []struct {
		name       string
		content    string
		wantSyntax bool
	}{
		{name: "invalid YAML is a syntax error", content: "name: [\n", wantSyntax: true},
		{name: "rejected uses expression is not a syntax error", content: "jobs:\n  b:\n    steps:\n      - uses: ${{ x }}\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse("wf.yml", []byte(tt.content))
			require.Error(t, err)
			var syntaxErr *SyntaxError
			assert.Equal(t, tt.wantSyntax, errors.As(err, &syntaxErr))
		})
	}
}

func TestLoadReadErrorIsNotASyntaxError(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "missing.yml"))
	require.Error(t, err)
	var syntaxErr *SyntaxError
	assert.False(t, errors.As(err, &syntaxErr))
}
