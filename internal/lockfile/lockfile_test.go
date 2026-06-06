package lockfile

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
			assert.Equal(t, tt.want, tt.dep.String())
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

func TestDependencyKey(t *testing.T) {
	d := Dependency{NWO: "actions/checkout", Ref: "v4", SHA: "abc"}
	assert.Equal(t, "actions/checkout@v4", d.Key())
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
			mismatches := CheckSHARefMismatches(context.Background(), tt.deps, nil)
			assert.Len(t, mismatches, tt.wantCount)
			if tt.wantMismatch != "" && len(mismatches) > 0 {
				assert.Equal(t, tt.wantMismatch, mismatches[0].Dep.NWO)
			}
		})
	}
}

// stubPeeler simulates a TagObjectPeeler: the configured map records, per
// (owner/repo|sha), the commit that sha peels to. Absent keys peel to ("", false).
type stubPeeler map[string]string

func (s stubPeeler) PeelTagObject(_ context.Context, owner, repo, sha string) (string, bool) {
	commit, ok := s[owner+"/"+repo+"|"+sha]
	return commit, ok
}

func TestCheckSHARefMismatches_HonorsTagObjectPeeler(t *testing.T) {
	const tagObjSHA = "d746ffe35508b1917358783b479e04febd2b8f71"
	const peeledCommit = "3a2844b7e9c422d3c10d287c895573f7108da1b3"
	const branchHead = "ddddccccbbbbaaaa0000111122223333eeeeffff"

	deps := []Dependency{
		// Legitimate annotated-tag-object pin (immutable release pattern).
		{NWO: "actions/github-script", Ref: tagObjSHA, SHA: peeledCommit},
		// Branch named after a SHA — the real forgery shape.
		{NWO: "evil/repo", Ref: "aaaa0000aaaa0000aaaa0000aaaa0000aaaa0000", SHA: branchHead},
	}
	peeler := stubPeeler{
		"actions/github-script|" + tagObjSHA: peeledCommit,
	}

	mismatches := CheckSHARefMismatches(context.Background(), deps, peeler)
	if assert.Len(t, mismatches, 1) {
		assert.Equal(t, "evil/repo", mismatches[0].Dep.NWO)
	}

	// Without the peeler the legitimate tag-object pin false-positives.
	mismatches = CheckSHARefMismatches(context.Background(), deps, nil)
	assert.Len(t, mismatches, 2)
}
