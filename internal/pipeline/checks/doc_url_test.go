package checks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCategoryDocURL(t *testing.T) {
	const base = "https://github.com/github/gh-actions-lock/blob/main/docs/troubleshooting.md"
	tests := []struct {
		category Category
		want     string
	}{
		{NotPinned, base + "#not-pinned"},
		{MissingRef, base + "#ref-missing"},
		{InvalidWorkflow, base + "#invalid-workflow"},
		{ShaAsRef, base + "#sha-as-ref"},
		{RefChanged, base + "#ref-changed"},
		{RefMoved, base + "#ref-moved"},
		{TagCommitMismatch, base + "#tag-commit-mismatch"},
		{Stale, base + "#stale"},
		{MisleadingSHA, base + "#misleading-sha"},
		{UnreachablePin, base + "#unreachable-pin"},
		{RepoRenamed, base + "#repo-renamed"},
		{RepoReplaced, base + "#repo-replaced"},
		{RepoUnavailable, base + "#repo-unavailable"},
		{AncestryUnknown, base + "#ancestry-unknown"},
		{ReachabilityUnknown, base + "#reachability-unknown"},
		{OnboardingRequired, base + "#onboarding-required"},
		{VersionRef, base + "#version-ref"},
		{LocalAction, base + "#local-action"},
		{StaleWorkflow, base + "#stale-workflow"},
		{InvalidSelfRepositoryRef, base + "#invalid-self-repository-ref"},
		{FreshTag, base + "#fresh-tag"},
		{CooldownConfigIgnored, base + "#cooldown-config-ignored"},
		{IncompleteLockfile, base + "#incomplete-lockfile"},
		{ContainerImage, base + "#container-image"},
		{Valid, ""},
		{RunOnly, ""},
		{SelfRepositoryAction, ""},
		{Category("unknown"), ""},
	}
	for _, tt := range tests {
		t.Run(string(tt.category), func(t *testing.T) {
			assert.Equal(t, tt.want, tt.category.DocURL())
		})
	}
}

// githubAnchor approximates GitHub's heading slug: lowercase, drop
// punctuation other than hyphens and underscores, and turn spaces into
// hyphens.
func githubAnchor(heading string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(heading) {
		switch {
		case r == ' ':
			b.WriteRune('-')
		case r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		}
	}
	return b.String()
}

func TestTroubleshootingDocHasEveryAnchor(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "troubleshooting.md"))
	require.NoError(t, err)
	anchors := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		if heading, ok := strings.CutPrefix(line, "#"); ok {
			anchors[githubAnchor(strings.TrimLeft(heading, "# "))] = true
		}
	}
	for c := range documented {
		t.Run(string(c), func(t *testing.T) {
			url := c.DocURL()
			_, anchor, ok := strings.Cut(url, "#")
			require.True(t, ok, url)
			assert.True(t, anchors[anchor], "docs/troubleshooting.md has no heading for #%s", anchor)
		})
	}
}

func TestFindingDocURLFollowsCategory(t *testing.T) {
	f := Finding{Category: NotPinned}
	f.Category = OnboardingRequired
	assert.Equal(t, OnboardingRequired.DocURL(), f.DocURL(), "a rewritten category links its own section")
}
