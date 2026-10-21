package checks

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCategoryDocURL(t *testing.T) {
	const base = "https://docs.github.com/actions/reference/security/workflow-dependency-lockfile-troubleshooting"
	tests := []struct {
		category Category
		want     string
	}{
		{NotPinned, base + "#not-pinned"},
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

func TestFindingDocURLFollowsCategory(t *testing.T) {
	f := Finding{Category: NotPinned}
	f.Category = OnboardingRequired
	assert.Equal(t, OnboardingRequired.DocURL(), f.DocURL(), "a rewritten category links its own section")
}
