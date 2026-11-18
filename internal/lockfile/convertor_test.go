package lockfile

import (
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/stretchr/testify/assert"
)

func TestUsedRoot(t *testing.T) {
	const sha = "9ddf35b71a482be7d8922b28e8d00df16b77e315"
	used := UsedRoot([]parserlock.ActionRef{
		{Owner: "Actions", Repo: "Checkout", Ref: "v4"},
		{Owner: "actions", Repo: "cache", Ref: sha},
	})
	tests := []struct {
		name string
		pin  parserlock.Pin
		want bool
	}{
		{"used by key, owner and repo fold", parserlock.Pin{Owner: "actions", Repo: "checkout", Ref: "v4"}, true},
		{"ref is case-sensitive", parserlock.Pin{Owner: "actions", Repo: "checkout", Ref: "V4"}, false},
		{"SHA key used by its SHA", parserlock.Pin{Owner: "actions", Repo: "cache", Ref: sha}, true},
		{"tag key not used by its commit", parserlock.Pin{Owner: "actions", Repo: "cache", Ref: "v1"}, false},
		{"other repo", parserlock.Pin{Owner: "actions", Repo: "setup-go", Ref: "v4"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, used(tt.pin))
		})
	}
}
