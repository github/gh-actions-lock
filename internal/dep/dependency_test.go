package dep

import (
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
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

func TestDependencyKey(t *testing.T) {
	d := Dependency{NWO: "actions/checkout", Ref: "v4", SHA: "abc"}
	assert.Equal(t, "actions/checkout@v4", d.Key())
}

func TestDedupMergesTransferredSources(t *testing.T) {
	live := Dependency{
		NWO:  "new/action",
		Ref:  "v1",
		SHA:  "live",
		Path: "live-path",
		OriginalRefs: []parserlock.ActionRef{
			{Owner: "old", Repo: "action", Ref: "v1"},
		},
	}
	seeded := Dependency{NWO: "new/action", Ref: "v1", SHA: "seeded"}
	for _, tt := range []struct {
		name string
		deps []Dependency
	}{
		{
			name: "seeded before live redirect",
			deps: []Dependency{seeded, live},
		},
		{
			name: "live redirect before seeded",
			deps: []Dependency{live, seeded},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := Dedup(tt.deps)

			require.Len(t, got, 1)
			assert.Equal(t, "live", got[0].SHA)
			assert.Equal(t, "live-path", got[0].Path)
			require.Len(t, got[0].OriginalRefs, 1)
			assert.Equal(t, "old/action", got[0].OriginalRefs[0].NWO())
		})
	}
}
