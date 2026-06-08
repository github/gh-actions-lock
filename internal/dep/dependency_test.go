package dep

import (
	"testing"

	"github.com/stretchr/testify/assert"
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
