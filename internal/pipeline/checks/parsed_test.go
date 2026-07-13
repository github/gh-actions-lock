package checks

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsImmutableRef(t *testing.T) {
	tests := []struct {
		ref  string
		want bool
	}{
		{"v4.2.1", true},      // full semver: one commit for life
		{"4.2.1", true},       // full semver without leading v
		{"v4", false},         // major-only: moves as patches land
		{"v4.2", false},       // major.minor: moves
		{"v4.2.1-rc1", false}, // prerelease: not a full stable tag
		{"main", false},       // branch
		{"master", false},     // branch
		{"de0fac2e4500dabe0009e67214ff5f5447ce83dd", false}, // bare SHA
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			assert.Equal(t, tt.want, IsImmutableRef(tt.ref))
		})
	}
}
