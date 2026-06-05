package lockfile

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParsePin(t *testing.T) {
	tests := []struct {
		name  string
		entry string
		want  Pin
	}{
		{
			name:  "owner repo",
			entry: "actions/checkout@v4:sha1-11bd71901bbe5b1630ceea73d27597364c9af683",
			want: Pin{
				NWO:   "actions/checkout",
				Owner: "actions",
				Repo:  "checkout",
				Ref:   "v4",
				Algo:  "sha1",
				Hex:   "11bd71901bbe5b1630ceea73d27597364c9af683",
			},
		},
		{
			name:  "sha256",
			entry: "actions/checkout@v4:sha256-a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
			want: Pin{
				NWO:   "actions/checkout",
				Owner: "actions",
				Repo:  "checkout",
				Ref:   "v4",
				Algo:  "sha256",
				Hex:   "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
			},
		},
		{
			// Monorepo sub-action tags (e.g. attest-build-provenance's
			// predicate/) embed an '@' in the ref, producing a double-'@'
			// key. The first '@' bounds the NWO and the last ':' bounds the
			// digest, so the ref survives intact.
			name:  "ref containing at (monorepo sub-action tag)",
			entry: "actions/attest-build-provenance@predicate@1.1.4:sha1-36fa7d009e22618ca7cd599486979b8150596c74",
			want: Pin{
				NWO:   "actions/attest-build-provenance",
				Owner: "actions",
				Repo:  "attest-build-provenance",
				Ref:   "predicate@1.1.4",
				Algo:  "sha1",
				Hex:   "36fa7d009e22618ca7cd599486979b8150596c74",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParsePin(tt.entry)
			require.True(t, ok)
			assert.Equal(t, tt.want, got)
			assert.True(t, isValidPin(tt.entry))
			// Round-trip: serializing the parsed pin reproduces the entry.
			assert.Equal(t, tt.entry, got.String())
		})
	}
}

func TestParsePin_Invalid(t *testing.T) {
	tests := []struct {
		name  string
		entry string
	}{
		{"empty", ""},
		{"missing at", "actions/checkout:sha1-abc123"},
		{"missing colon", "actions/checkout@v4sha1-abc123"},
		{"missing algo dash", "actions/checkout@v4:sha1abc123"},
		{"ref contains colon", "actions/checkout@refs/tags/v1:prod:sha1-11bd71901bbe5b1630ceea73d27597364c9af683"},
		{"unsupported algo", "actions/checkout@v4:md5-098f6bcd4621d373cade4e832627b4f6"},
		{"sha1 too short", "actions/checkout@v4:sha1-abc123"},
		{"empty owner", "/checkout@v4:sha1-abc123"},
		{"empty repo", "actions/@v4:sha1-abc123"},
		{"sub-action path rejected", "actions/cache/save@v4:sha1-11bd71901bbe5b1630ceea73d27597364c9af683"},
		{"deep sub-action path rejected", "owner/repo/a/b@v1:sha1-11bd71901bbe5b1630ceea73d27597364c9af683"},
		{"with metadata suffix (pins are pure identity)", "actions/checkout@v4:sha1-11bd71901bbe5b1630ceea73d27597364c9af683;owner_id=1234"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParsePin(tt.entry)
			assert.False(t, ok)
			assert.Equal(t, Pin{}, got)
			assert.False(t, isValidPin(tt.entry))
		})
	}
}

func TestIndexKey(t *testing.T) {
	assert.Equal(t, "actions/checkout@v4", IndexKey("actions", "checkout", "v4"))
}

func TestPin_IndexKey(t *testing.T) {
	tests := []struct {
		name string
		pin  Pin
		want string
	}{
		{
			name: "owner repo",
			pin:  Pin{Owner: "actions", Repo: "checkout", Ref: "v4"},
			want: "actions/checkout@v4",
		},
		{
			name: "mixed case is lowercased",
			pin:  Pin{Owner: "Actions", Repo: "Checkout", Ref: "v4"},
			want: "actions/checkout@v4",
		},
		{
			name: "empty ref",
			pin:  Pin{Owner: "actions", Repo: "checkout"},
			want: "actions/checkout@",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.pin.IndexKey())
		})
	}
}
