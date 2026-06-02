package lockfile

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEntry_EncodeDecodeRoundTrip(t *testing.T) {
	in := Entry{Pin: "actions/checkout@v4:sha1-11bd71901bbe5b1630ceea73d27597364c9af683", OwnerID: 1234, RepoID: 5678}
	s, err := EncodeEntry(in)
	require.NoError(t, err)
	out, err := DecodeEntry(s)
	require.NoError(t, err)
	assert.Equal(t, in, out)
}

func TestDecodeEntry_Invalid(t *testing.T) {
	_, err := DecodeEntry("!!!not-base64!!!")
	assert.Error(t, err)
	_, err = DecodeEntry("dGhpcyBpcyBub3QganNvbg==") // "this is not json"
	assert.Error(t, err)
}

func TestEntry_IndexKey(t *testing.T) {
	tests := []struct {
		name string
		pin  string
		want string
	}{
		{
			name: "valid pin",
			pin:  "actions/checkout@v4:sha1-11bd71901bbe5b1630ceea73d27597364c9af683",
			want: "actions/checkout@v4",
		},
		{
			name: "valid pin (casing normalized)",
			pin:  "Actions/Setup-Go@v5:sha256-a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
			want: "actions/setup-go@v5",
		},
		{
			name: "unparseable pin returns empty string",
			pin:  "not-a-valid-pin",
			want: "",
		},
		{
			name: "empty pin returns empty string",
			pin:  "",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, Entry{Pin: tt.pin}.IndexKey())
		})
	}
}
