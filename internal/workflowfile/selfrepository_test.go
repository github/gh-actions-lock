package workflowfile

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsSelfRepositoryAction(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{"repository root action", "$/", true},
		{"bare self repository action", "$/actions/foo", true},
		{"self repository reusable workflow", "$/.github/workflows/x.yml", true},
		{"self repository with ref", "$/actions/foo@v1", true},
		{"leading whitespace", "  $/actions/foo", true},
		{"local action", "./actions/foo", false},
		{"remote action", "actions/checkout@v4", false},
		{"expression", "${{ matrix.action }}", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsSelfRepositoryAction(tt.value))
		})
	}
}

func TestSelfRepositoryRefHasVersion(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{"repository root action no ref", "$/", false},
		{"bare action no ref", "$/actions/foo", false},
		{"reusable workflow no ref", "$/.github/workflows/x.yml", false},
		{"action with ref", "$/actions/foo@v1", true},
		{"reusable workflow with ref", "$/.github/workflows/x.yml@v1", true},
		{"local action with at (not self repository)", "./actions/foo@v1", false},
		{"remote action with ref (not self repository)", "actions/checkout@v4", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, SelfRepositoryRefHasVersion(tt.value))
		})
	}
}
