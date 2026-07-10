package workflowfile

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsSelfRepoAction(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{"bare self-repo action", "$/actions/foo", true},
		{"self-repo reusable workflow", "$/.github/workflows/x.yml", true},
		{"self-repo with ref", "$/actions/foo@v1", true},
		{"leading whitespace", "  $/actions/foo", true},
		{"local action", "./actions/foo", false},
		{"remote action", "actions/checkout@v4", false},
		{"expression", "${{ matrix.action }}", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsSelfRepoAction(tt.value))
		})
	}
}

func TestSelfRepoRefHasVersion(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{"bare action no ref", "$/actions/foo", false},
		{"reusable workflow no ref", "$/.github/workflows/x.yml", false},
		{"action with ref", "$/actions/foo@v1", true},
		{"reusable workflow with ref", "$/.github/workflows/x.yml@v1", true},
		{"local action with at (not self-repo)", "./actions/foo@v1", false},
		{"remote action with ref (not self-repo)", "actions/checkout@v4", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, SelfRepoRefHasVersion(tt.value))
		})
	}
}
