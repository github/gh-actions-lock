package workflowfile

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnsureSentinel(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "adds sentinel to plain workflow",
			input: "name: ci\non: push\n",
			want:  SentinelComment + "\n\nname: ci\non: push\n",
		},
		{
			name:  "adds sentinel before existing comment",
			input: "# my workflow\nname: ci\n",
			want:  SentinelComment + "\n# my workflow\nname: ci\n",
		},
		{
			name:  "idempotent when sentinel already present",
			input: SentinelComment + "\n\nname: ci\n",
			want:  SentinelComment + "\n\nname: ci\n",
		},
		{
			name:  "empty content",
			input: "",
			want:  SentinelComment + "\n",
		},
		{
			name:  "sentinel buried in file still prepends",
			input: "name: ci\n# This workflow is managed by gh actions-lock.\non: push\n",
			want:  SentinelComment + "\n\nname: ci\n# This workflow is managed by gh actions-lock.\non: push\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EnsureSentinel([]byte(tt.input))
			assert.Equal(t, tt.want, string(got))
		})
	}
}

func TestSubpathRewriteLookup(t *testing.T) {
	replacements := map[string]string{
		"actions/cache@27d5ce7": "actions/cache@v5.0.5",
	}

	tests := []struct {
		name   string
		input  string
		want   string
		wantOK bool
	}{
		{
			name:   "sub-path restore",
			input:  "actions/cache/restore@27d5ce7",
			want:   "actions/cache/restore@v5.0.5",
			wantOK: true,
		},
		{
			name:   "sub-path save",
			input:  "actions/cache/save@27d5ce7",
			want:   "actions/cache/save@v5.0.5",
			wantOK: true,
		},
		{
			name:   "no sub-path (exact match, not handled here)",
			input:  "actions/cache@27d5ce7",
			want:   "",
			wantOK: false,
		},
		{
			name:   "no match in replacements",
			input:  "actions/checkout/sub@abc1234",
			want:   "",
			wantOK: false,
		},
		{
			name:   "no @ separator",
			input:  "actions/cache/restore",
			want:   "",
			wantOK: false,
		},
		{
			name:   "deeply nested sub-path",
			input:  "actions/cache/restore/deep/nested@27d5ce7",
			want:   "actions/cache/restore/deep/nested@v5.0.5",
			wantOK: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := subpathRewriteLookup(tt.input, replacements)
			assert.Equal(t, tt.wantOK, ok)
			if ok {
				assert.Equal(t, tt.want, got)
			}
		})
	}
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

func TestRewriteActionRefs_DropsTrailingComments(t *testing.T) {
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
	assert.Contains(t, s, "uses: actions/checkout@v4.2.1\n")
	assert.NotContains(t, s, "pinned for stability")
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
	assert.Contains(t, s, "uses: actions/checkout@v4.2.1")
	assert.Contains(t, s, "# DO NOT USE actions/checkout@v4 - see docs")
}

func TestRewriteActionRefs_AnchoredAtColumn(t *testing.T) {
	content := []byte("jobs:\n  a:\n    steps:\n      # bumped from actions/checkout@v3 — do not revert\n      - uses: actions/checkout@v3\n")
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
	assert.Contains(t, s, "# bumped from actions/checkout@v3 — do not revert")
}

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

func TestRewriteActionRefs_SubPathActions(t *testing.T) {
	content := []byte(`jobs:
  build:
    steps:
      - uses: actions/cache/restore@27d5ce7 # restore cache
      - uses: actions/cache/save@27d5ce7
      - uses: actions/cache@27d5ce7
      - uses: actions/checkout@v3
`)
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "subpath.yml")
	require.NoError(t, os.WriteFile(path, content, 0o644))

	f, err := Load(path)
	require.NoError(t, err)

	output, changed, err := f.RewriteActionRefs(map[string]string{
		"actions/cache@27d5ce7": "actions/cache@v5.0.5",
	})
	require.NoError(t, err)
	assert.Equal(t, 3, changed)

	out := string(output)
	assert.Contains(t, out, "actions/cache/restore@v5.0.5\n")
	assert.Contains(t, out, "actions/cache/save@v5.0.5")
	assert.Contains(t, out, "actions/cache@v5.0.5")
	assert.Contains(t, out, "actions/checkout@v3")
}
