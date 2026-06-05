package lockfile

import (
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

func TestActionRefNWO(t *testing.T) {
	assert.Equal(t, "actions/checkout", ActionRef{Owner: "actions", Repo: "checkout"}.NWO())
	assert.Equal(t, "", ActionRef{}.NWO())
}

func TestActionRefFullName(t *testing.T) {
	assert.Equal(t, "actions/checkout", ActionRef{Owner: "actions", Repo: "checkout"}.FullName())
	assert.Equal(t, "actions/cache/save", ActionRef{Owner: "actions", Repo: "cache", Path: "save"}.FullName())
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

func TestIsFullSha(t *testing.T) {
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
			assert.Equal(t, tt.want, IsFullSha(tt.input))
		})
	}
}
