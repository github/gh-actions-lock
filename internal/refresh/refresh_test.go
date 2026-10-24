package refresh

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSelector(t *testing.T) {
	tests := []struct {
		in      string
		want    Selector
		wantErr bool
	}{
		{in: "actions/checkout", want: Selector{NWO: "actions/checkout"}},
		{in: "actions/checkout@v4", want: Selector{NWO: "actions/checkout", Ref: "v4"}},
		{in: "actions/checkout@release/v1", want: Selector{NWO: "actions/checkout", Ref: "release/v1"}},
		{in: "actions", wantErr: true},
		{in: "actions/", wantErr: true},
		{in: "/checkout", wantErr: true},
		{in: "actions/checkout@", wantErr: true},
		{in: ".github/workflows/ci.yml", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseSelector(tt.in)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

const sha = "sha1-1111111111111111111111111111111111111111"

func loadState(t *testing.T) *lockfile.State {
	t.Helper()
	lock := "version: 'v0.0.3'\n" +
		"dependencies:\n" +
		"  'octo/comp@v1':\n    ref: 'v1'\n    commit: '" + sha + "'\n    owner_id: 1\n    repo_id: 1\n    uses:\n      - 'octo/child@v2'\n" +
		"  'octo/child@v2':\n    ref: 'v2'\n    commit: '" + sha + "'\n    owner_id: 1\n    repo_id: 2\n" +
		"  'octo/other@v1':\n    ref: 'v1'\n    commit: '" + sha + "'\n    owner_id: 1\n    repo_id: 3\n" +
		"  'octo/other@Main':\n    ref: 'Main'\n    commit: '" + sha + "'\n    owner_id: 1\n    repo_id: 3\n" +
		"workflows:\n" +
		"  '.github/workflows/a.yml':\n    - 'octo/comp@v1'\n" +
		"  '.github/workflows/b.yml':\n    - 'octo/other@v1'\n" +
		"  '.github/workflows/c.yml':\n    - 'octo/other@Main'\n"
	p := filepath.Join(t.TempDir(), "actions.lock")
	require.NoError(t, os.WriteFile(p, []byte(lock), 0o600))
	s, err := lockfile.LoadStateAt(p, nil)
	require.NoError(t, err)
	return s
}

func TestExpand(t *testing.T) {
	tests := []struct {
		name          string
		sels          []Selector
		wantKeys      []string
		wantWorkflows []string
	}{
		{
			name:          "a composite brings its children",
			sels:          []Selector{{NWO: "octo/comp", Ref: "v1"}},
			wantKeys:      []string{"octo/comp@v1", "octo/child@v2"},
			wantWorkflows: []string{".github/workflows/a.yml"},
		},
		{
			name:          "a child alone marks the workflow that reaches it",
			sels:          []Selector{{NWO: "octo/child", Ref: "v2"}},
			wantKeys:      []string{"octo/child@v2"},
			wantWorkflows: []string{".github/workflows/a.yml"},
		},
		{
			name:          "owner/repo matches every ref, case-insensitively",
			sels:          []Selector{{NWO: "Octo/Other"}},
			wantKeys:      []string{"octo/other@v1", "octo/other@Main"},
			wantWorkflows: []string{".github/workflows/b.yml", ".github/workflows/c.yml"},
		},
	}
	all := []string{"octo/comp@v1", "octo/child@v2", "octo/other@v1", "octo/other@Main"}
	wfs := []string{".github/workflows/a.yml", ".github/workflows/b.yml", ".github/workflows/c.yml"}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := Expand(loadState(t), tt.sels)
			require.NoError(t, err)
			for _, k := range all {
				nwo, ref, _ := strings.Cut(k, "@")
				assert.Equal(t, slices.Contains(tt.wantKeys, k), s.Has(nwo, ref), k)
			}
			for _, wf := range wfs {
				assert.Equal(t, slices.Contains(tt.wantWorkflows, wf), s.Workflow(wf), wf)
			}
		})
	}
}

func TestExpand_RefIsCaseSensitive(t *testing.T) {
	_, err := Expand(loadState(t), []Selector{{NWO: "octo/other", Ref: "main"}})
	assert.EqualError(t, err, "no lockfile entry matches octo/other@main")
}

func TestSet_NilSelectsNothing(t *testing.T) {
	var s *Set
	assert.False(t, s.Has("octo/comp", "v1"))
	assert.False(t, s.Workflow(".github/workflows/a.yml"))
}

func TestSet_HasFoldsFullSHA(t *testing.T) {
	s := &Set{keys: map[string]bool{normalize("Octo/X", "ABCDEF0123456789ABCDEF0123456789ABCDEF01"): true}}
	assert.True(t, s.Has("octo/x", "abcdef0123456789abcdef0123456789abcdef01"))
	assert.False(t, s.Has("octo/x", "v1"))
}
