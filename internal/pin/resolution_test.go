package pin

import (
	"testing"

	"github.com/stretchr/testify/assert"
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
