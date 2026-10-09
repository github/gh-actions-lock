package lockfile

import (
	"strings"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/stretchr/testify/assert"
)

func TestNewDirectTracker_Matching(t *testing.T) {
	sha1 := "11bd71901bbe5b1630ceea73d27597364c9af683"
	sha256 := strings.Repeat("ab", 32)

	tests := []struct {
		name       string
		useOwner   string
		useRepo    string
		useRef     string
		depNWO     string
		depRef     string
		wantDirect bool
	}{
		{"exact match", "actions", "checkout", "v4", "actions/checkout", "v4", true},
		{"owner case differs", "Actions", "checkout", "v4", "actions/checkout", "v4", true},
		{"repo case differs", "actions", "Checkout", "v4", "actions/checkout", "v4", true},
		{"dep spelled mixed case", "actions", "checkout", "v4", "Actions/Checkout", "v4", true},
		{"symbolic ref case differs", "actions", "checkout", "V4", "actions/checkout", "v4", false},
		{"branch ref case differs", "actions", "checkout", "Main", "actions/checkout", "main", false},
		{"sha1 case differs", "actions", "checkout", strings.ToUpper(sha1), "actions/checkout", sha1, true},
		{"sha256 case differs", "actions", "checkout", strings.ToUpper(sha256), "actions/checkout", sha256, true},
		{"sha-length non-hex ref stays case-sensitive", "actions", "checkout", strings.Repeat("G", 40), "actions/checkout", strings.Repeat("g", 40), false},
		{"different repo", "actions", "cache", "v4", "actions/checkout", "v4", false},
		{"different ref", "actions", "checkout", "v3", "actions/checkout", "v4", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			refs := []parserlock.ActionRef{{Owner: tt.useOwner, Repo: tt.useRepo, Ref: tt.useRef}}
			deps := []dep.Dependency{{NWO: tt.depNWO, Ref: tt.depRef}}

			tracker := NewDirectTracker(refs, deps)

			assert.Equal(t, tt.wantDirect, tracker.IsDirect(0))
		})
	}
}

func TestDirectTracker_KeysUseDepSpelling(t *testing.T) {
	refs := []parserlock.ActionRef{{Owner: "Actions", Repo: "Checkout", Ref: "v4"}}
	deps := []dep.Dependency{
		{NWO: "actions/checkout", Ref: "v4"},
		{NWO: "actions/cache", Ref: "v4"},
	}

	keys := NewDirectTracker(refs, deps).Keys(deps)

	assert.Equal(t, map[string]bool{"actions/checkout@v4": true}, keys)
}
