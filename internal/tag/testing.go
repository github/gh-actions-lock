package tag

import (
	"testing"

	"github.com/github/gh-actions-lock/internal/ghapi"
	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
)

// NewListerForTest builds a Lister wired to a mock registry.
func NewListerForTest(t *testing.T, reg *httpmock.Registry) *Lister {
	t.Helper()
	client, err := ghapi.New("github.com", ghapi.WithClientTransport(reg))
	if err != nil {
		t.Fatalf("ghapi.New: %v", err)
	}
	return NewLister(client, CooldownConfig{DefaultDays: 3, RepoOverrides: map[string]int{}})
}
