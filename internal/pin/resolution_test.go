package pin

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolution_String(t *testing.T) {
	tests := []struct {
		name string
		r    Resolution
		want string
	}{
		{"pinned", Pinned, "pinned"},
		{"verified", Verified, "verified"},
		{"investigate is needs-investigation", Investigate, "needs-investigation"},
		{"skipped", Skipped, "skipped"},
		{"unresolved", Unresolved, "unresolved"},
		{"arbitrary value passes through", Resolution("custom"), "custom"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.r.String())
		})
	}
}

func TestResolution_JSON_roundtrip(t *testing.T) {
	tests := []struct {
		name     string
		r        Resolution
		wantJSON string
	}{
		{"pinned round-trips", Pinned, `"pinned"`},
		{"verified round-trips", Verified, `"verified"`},
		{"investigate round-trips", Investigate, `"needs-investigation"`},
		{"skipped round-trips", Skipped, `"skipped"`},
		{"unresolved round-trips", Unresolved, `"unresolved"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := json.Marshal(tt.r)
			require.NoError(t, err)
			assert.Equal(t, tt.wantJSON, string(b))

			var got Resolution
			require.NoError(t, json.Unmarshal(b, &got))
			assert.Equal(t, tt.r, got)
		})
	}
}

func TestResolution_UnmarshalJSON_invalid(t *testing.T) {
	var r Resolution
	err := json.Unmarshal([]byte(`123`), &r)
	assert.Error(t, err)
}
