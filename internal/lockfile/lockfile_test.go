package lockfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseActionRef(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantNil  bool
		wantNWO  string
		wantPath string
		wantRef  string
	}{
		{name: "simple action", input: "actions/checkout@v4", wantNWO: "actions/checkout", wantRef: "v4"},
		{name: "path action", input: "actions/cache/save@v4", wantNWO: "actions/cache", wantPath: "save", wantRef: "v4"},
		{name: "SHA ref", input: "actions/checkout@11bd71901bbe5b1630ceea73d27597364c9af683", wantNWO: "actions/checkout", wantRef: "11bd71901bbe5b1630ceea73d27597364c9af683"},
		{name: "local path action", input: "./local-action", wantNil: true},
		{name: "docker action", input: "docker://alpine:3.18", wantNil: true},
		{name: "expression-based ref", input: "${{ matrix.action }}", wantNil: true},
		{name: "no ref", input: "actions/checkout", wantNil: true},
		{name: "empty ref", input: "actions/checkout@", wantNil: true},
		{name: "single segment", input: "checkout@v4", wantNil: true},
		{name: "reusable workflow yml", input: "owner/repo/.github/workflows/called.yml@v1", wantNil: true},
		{name: "reusable workflow yaml", input: "owner/repo/.github/workflows/called.yaml@main", wantNil: true},
		{name: "path action that is not a reusable workflow", input: "owner/repo/some/path@v1", wantNWO: "owner/repo", wantPath: "some/path", wantRef: "v1"},
		{name: "whitespace trimmed", input: "  actions/checkout@v4  ", wantNWO: "actions/checkout", wantRef: "v4"},
		{name: "empty owner segment", input: "/repo@v1", wantNil: true},
		{name: "empty name segment", input: "owner/@v1", wantNil: true},
		{name: "leading slash both empty", input: "/@v1", wantNil: true},
		{name: "owner with newline injection", input: "actions\n/checkout@v1", wantNil: true},
		{name: "owner with quote", input: `actions"/checkout@v1`, wantNil: true},
		{name: "owner with space", input: "actions /checkout@v1", wantNil: true},
		{name: "control char tab embedded", input: "actions/check\tout@v1", wantNil: true},
		{name: "nested folder containing reusable workflow path is not reusable", input: "owner/repo/tools/.github/workflows/x.yml@v1", wantNWO: "owner/repo", wantPath: "tools/.github/workflows/x.yml", wantRef: "v1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseActionRef(tt.input)
			if tt.wantNil {
				require.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, tt.wantNWO, got.NWO())
			assert.Equal(t, tt.wantPath, got.Path)
			assert.Equal(t, tt.wantRef, got.Ref)
		})
	}
}

func TestDependencyStringRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		dep  Dependency
		want string
	}{
		{
			name: "sha1 dependency",
			dep: Dependency{
				NWO:      "actions/checkout",
				Ref:      "v4",
				SHA:      "11bd71901bbe5b1630ceea73d27597364c9af683",
				HashAlgo: "sha1",
			},
			want: "actions/checkout@v4:sha1-11bd71901bbe5b1630ceea73d27597364c9af683",
		},
		{
			name: "sha256 dependency",
			dep: Dependency{
				NWO:      "actions/checkout",
				Ref:      "v4",
				SHA:      "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
				HashAlgo: "sha256",
			},
			want: "actions/checkout@v4:sha256-a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
		},
		{
			name: "auto-detect sha1 from length",
			dep: Dependency{
				NWO: "actions/checkout",
				Ref: "v4",
				SHA: "11bd71901bbe5b1630ceea73d27597364c9af683",
			},
			want: "actions/checkout@v4:sha1-11bd71901bbe5b1630ceea73d27597364c9af683",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := tt.dep.String()
			assert.Equal(t, tt.want, s)

			parsed, err := ParseDependencyString(s)
			require.NoError(t, err)
			assert.Equal(t, tt.dep.NWO, parsed.NWO)
			assert.Equal(t, tt.dep.Ref, parsed.Ref)
			assert.Equal(t, tt.dep.SHA, parsed.SHA)
		})
	}
}

func TestParseDependencyStringErrors(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{name: "no hash prefix", input: "actions/checkout@v4:abc123"},
		{name: "no @ separator", input: "actions/checkout:sha1-abc123"},
		{name: "empty string", input: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseDependencyString(tt.input)
			require.Error(t, err)
		})
	}
}

func TestLoadAndExtractActionRefs(t *testing.T) {
	f, err := Load("testdata/simple.yml")
	require.NoError(t, err)

	refs, localPaths, warnings := f.ExtractActionRefs()
	assert.Len(t, refs, 2)
	assert.Empty(t, localPaths)
	assert.Empty(t, warnings)

	assert.Equal(t, "actions/checkout", refs[0].NWO())
	assert.Equal(t, "v4", refs[0].Ref)
	assert.Equal(t, "actions/setup-go", refs[1].NWO())
	assert.Equal(t, "v5", refs[1].Ref)
}

func TestExtractActionRefsMixed(t *testing.T) {
	f, err := Load("testdata/mixed_refs.yml")
	require.NoError(t, err)

	refs, localPaths, warnings := f.ExtractActionRefs()
	assert.Len(t, refs, 3)
	assert.Equal(t, "actions/checkout", refs[0].NWO())
	assert.Equal(t, "actions/cache", refs[1].NWO())
	assert.Equal(t, "save", refs[1].Path)
	assert.Equal(t, "actions/setup-node", refs[2].NWO())

	assert.Len(t, localPaths, 1)
	assert.Equal(t, "./local-action", localPaths[0])

	assert.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "expression-based")
}

func TestRewriteActionRefs(t *testing.T) {
	f, err := Load("testdata/simple.yml")
	require.NoError(t, err)

	output, changed, err := f.RewriteActionRefs(map[string]string{
		"actions/checkout@v4": "actions/checkout@v5",
		"actions/setup-go@v5": "actions/setup-go@v6",
	})
	require.NoError(t, err)
	assert.Equal(t, 2, changed)

	s := string(output)
	assert.Contains(t, s, "uses: actions/checkout@v5")
	assert.Contains(t, s, "uses: actions/setup-go@v6")
	assert.NotContains(t, s, "uses: actions/checkout@v4")
	assert.NotContains(t, s, "uses: actions/setup-go@v5")
}

func TestRewriteActionRefs_PreservesTrailingComments(t *testing.T) {
	content := []byte(`name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4 # pinned for stability
      - uses: actions/setup-go@v5
`)
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "with_comment.yml")
	require.NoError(t, os.WriteFile(path, content, 0o644))

	f, err := Load(path)
	require.NoError(t, err)

	output, changed, err := f.RewriteActionRefs(map[string]string{
		"actions/checkout@v4": "actions/checkout@v4.2.1",
	})
	require.NoError(t, err)
	assert.Equal(t, 1, changed)

	s := string(output)
	assert.Contains(t, s, "uses: actions/checkout@v4.2.1 # pinned for stability")
	assert.NotContains(t, s, "uses: actions/checkout@v4 #")
}

func TestRewriteActionRefs_OnlyMatchesYAMLUses(t *testing.T) {
	content := []byte(`name: ci
on: push
# DO NOT USE actions/checkout@v4 - see docs
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
`)
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "comment_first.yml")
	require.NoError(t, os.WriteFile(path, content, 0o644))

	f, err := Load(path)
	require.NoError(t, err)

	output, changed, err := f.RewriteActionRefs(map[string]string{
		"actions/checkout@v4": "actions/checkout@v4.2.1",
	})
	require.NoError(t, err)
	assert.Equal(t, 1, changed)

	s := string(output)
	// The uses: line should be rewritten
	assert.Contains(t, s, "uses: actions/checkout@v4.2.1")
	// The comment should NOT be rewritten
	assert.Contains(t, s, "# DO NOT USE actions/checkout@v4 - see docs")
}

// TestRewriteActionRefs_AnchoredAtColumn verifies the rewrite uses the YAML
// node's reported column rather than scanning the line for the first
// occurrence of the old value. Without column anchoring, a same-line
// in-document comment that mentions the old ref would be rewritten in
// place of the actual value.
func TestRewriteActionRefs_AnchoredAtColumn(t *testing.T) {
	// Note: the in-line comment textually precedes the value on the
	// same line by referring to it. yaml.v3 reports the value's column
	// after the comment.
	content := []byte("jobs:\n  a:\n    steps:\n      # bumped from actions/checkout@v3 \u2014 do not revert\n      - uses: actions/checkout@v3\n")
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "anchor.yml")
	require.NoError(t, os.WriteFile(path, content, 0o644))

	f, err := Load(path)
	require.NoError(t, err)

	output, changed, err := f.RewriteActionRefs(map[string]string{
		"actions/checkout@v3": "actions/checkout@v4",
	})
	require.NoError(t, err)
	assert.Equal(t, 1, changed)

	s := string(output)
	assert.Contains(t, s, "      - uses: actions/checkout@v4\n")
	assert.Contains(t, s, "# bumped from actions/checkout@v3 \u2014 do not revert")
}

// TestRewriteActionRefs_SkipsAnchorsAndAliases ensures we never rewrite a
// scalar that is part of an anchor/alias relationship: replacing one site
// would silently affect every other use of the same anchor.
func TestRewriteActionRefs_SkipsAnchorsAndAliases(t *testing.T) {
	content := []byte("jobs:\n  a:\n    steps:\n      - uses: &pinned actions/checkout@v3\n  b:\n    steps:\n      - uses: *pinned\n")
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "anchored.yml")
	require.NoError(t, os.WriteFile(path, content, 0o644))

	f, err := Load(path)
	require.NoError(t, err)

	_, changed, err := f.RewriteActionRefs(map[string]string{
		"actions/checkout@v3": "actions/checkout@v4",
	})
	require.NoError(t, err)
	assert.Equal(t, 0, changed)
}

// TestExtractLocalCompositeRefs_RejectsPathTraversal confirms that a
// `uses: ./../../etc/passwd` style local path is refused rather than
// reading outside the discovered repo root.
func TestExtractLocalCompositeRefs_RejectsPathTraversal(t *testing.T) {
	repoRoot := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(repoRoot, ".git"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(repoRoot, ".github", "workflows"), 0o755))
	workflowPath := filepath.Join(repoRoot, ".github", "workflows", "ci.yml")
	require.NoError(t, os.WriteFile(workflowPath, []byte("name: ci\n"), 0o644))

	_, warnings := ExtractLocalCompositeRefs(workflowPath, []string{"./../../etc"})

	var sawRefusal bool
	for _, w := range warnings {
		if strings.Contains(w, "refusing to read action file outside repo root") {
			sawRefusal = true
		}
	}
	assert.True(t, sawRefusal, "expected refusal warning, got: %#v", warnings)
}

func TestParseActionMeta(t *testing.T) {
	tests := []struct {
		name       string
		file       string
		wantExec   ExecutionType
		wantNested int
	}{
		{name: "composite action", file: "testdata/composite_action.yml", wantExec: ExecComposite, wantNested: 2},
		{name: "node action", file: "testdata/node_action.yml", wantExec: ExecNode, wantNested: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content, err := os.ReadFile(tt.file)
			require.NoError(t, err)

			meta, err := ParseActionMeta(string(content))
			require.NoError(t, err)
			assert.Equal(t, tt.wantExec, meta.Execution)
			assert.Len(t, meta.NestedUses, tt.wantNested)
		})
	}
}

func TestIsReusableWorkflow(t *testing.T) {
	tests := []struct {
		name string
		ref  ActionRef
		want bool
	}{
		{name: "reusable workflow yml", ref: ActionRef{Owner: "owner", Repo: "repo", Path: ".github/workflows/called.yml", Ref: "v1"}, want: true},
		{name: "reusable workflow yaml", ref: ActionRef{Owner: "owner", Repo: "repo", Path: ".github/workflows/called.yaml", Ref: "main"}, want: true},
		{name: "regular path action", ref: ActionRef{Owner: "actions", Repo: "cache", Path: "save", Ref: "v4"}, want: false},
		{name: "no path", ref: ActionRef{Owner: "actions", Repo: "checkout", Ref: "v4"}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isReusableWorkflow(&tt.ref))
		})
	}
}

func TestDependencyKey(t *testing.T) {
	d := Dependency{NWO: "actions/checkout", Ref: "v4", SHA: "abc"}
	assert.Equal(t, "actions/checkout@v4", d.Key())
}

func TestActionRefNWO(t *testing.T) {
	assert.Equal(t, "actions/checkout", ActionRef{Owner: "actions", Repo: "checkout"}.NWO())
	assert.Equal(t, "", ActionRef{}.NWO())
}

func TestActionRefFullName(t *testing.T) {
	assert.Equal(t, "actions/checkout", ActionRef{Owner: "actions", Repo: "checkout"}.FullName())
	assert.Equal(t, "actions/cache/save", ActionRef{Owner: "actions", Repo: "cache", Path: "save"}.FullName())
}

func TestIsFullSHA(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{name: "valid lowercase sha", input: "11bd71901bbe5b1630ceea73d27597364c9af683", want: true},
		{name: "valid uppercase sha", input: "11BD71901BBE5B1630CEEA73D27597364C9AF683", want: true},
		{name: "valid mixed case sha", input: "11bd71901BBE5b1630ceea73d27597364C9AF683", want: true},
		{name: "too short", input: "11bd71901bbe5b1630ceea73d2759736", want: false},
		{name: "too long", input: "11bd71901bbe5b1630ceea73d27597364c9af683aa", want: false},
		{name: "tag ref", input: "v4", want: false},
		{name: "branch ref", input: "main", want: false},
		{name: "empty", input: "", want: false},
		{name: "non-hex chars", input: "ggbd71901bbe5b1630ceea73d27597364c9af683", want: false},
		{name: "sha256 length", input: "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2", want: true},
		{name: "41 chars not valid", input: "11bd71901bbe5b1630ceea73d27597364c9af683a", want: false},
		{name: "63 chars not valid", input: "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsFullSHA(tt.input))
		})
	}
}

func TestCheckSHARefMismatches(t *testing.T) {
	tests := []struct {
		name         string
		deps         []Dependency
		wantCount    int
		wantMismatch string
	}{
		{name: "no mismatch when ref equals sha", deps: []Dependency{{NWO: "foo/bar", Ref: "04d248b84655b509d8c44dc1d6f990c879747487", SHA: "04d248b84655b509d8c44dc1d6f990c879747487"}}, wantCount: 0},
		{name: "no mismatch for tag refs", deps: []Dependency{{NWO: "actions/checkout", Ref: "v4", SHA: "aaaa0000aaaa0000aaaa0000aaaa0000aaaa0000"}}, wantCount: 0},
		{name: "mismatch when sha-like ref resolves differently", deps: []Dependency{{NWO: "evil/repo", Ref: "aaaa0000aaaa0000aaaa0000aaaa0000aaaa0000", SHA: "bbbb0000bbbb0000bbbb0000bbbb0000bbbb0000"}}, wantCount: 1, wantMismatch: "evil/repo"},
		{name: "case insensitive match is not a mismatch", deps: []Dependency{{NWO: "foo/bar", Ref: "AAAA0000AAAA0000AAAA0000AAAA0000AAAA0000", SHA: "aaaa0000aaaa0000aaaa0000aaaa0000aaaa0000"}}, wantCount: 0},
		{name: "mixed deps only flags sha-like mismatches", deps: []Dependency{{NWO: "actions/checkout", Ref: "v4", SHA: "aaaa0000aaaa0000aaaa0000aaaa0000aaaa0000"}, {NWO: "evil/repo", Ref: "cccc0000cccc0000cccc0000cccc0000cccc0000", SHA: "dddd0000dddd0000dddd0000dddd0000dddd0000"}, {NWO: "good/repo", Ref: "eeee0000eeee0000eeee0000eeee0000eeee0000", SHA: "eeee0000eeee0000eeee0000eeee0000eeee0000"}}, wantCount: 1, wantMismatch: "evil/repo"},
		{name: "sha256 mismatch detected", deps: []Dependency{{NWO: "evil/repo256", Ref: "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2", SHA: "ffff0000ffff0000ffff0000ffff0000ffff0000ffff0000ffff0000ffff0000"}}, wantCount: 1, wantMismatch: "evil/repo256"},
		{name: "sha256 ref matches resolved", deps: []Dependency{{NWO: "good/repo256", Ref: "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2", SHA: "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"}}, wantCount: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mismatches := CheckSHARefMismatches(tt.deps)
			assert.Len(t, mismatches, tt.wantCount)
			if tt.wantMismatch != "" && len(mismatches) > 0 {
				assert.Equal(t, tt.wantMismatch, mismatches[0].Dep.NWO)
			}
		})
	}
}
