package scenarios

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoad(t *testing.T) {
	cat, err := Load()
	require.NoError(t, err)
	assert.Greater(t, len(cat.Scenarios), 20, "expected at least 20 scenarios")
	assert.Greater(t, len(cat.Categories), 5, "expected at least 5 categories")
}

func TestCatalogNames(t *testing.T) {
	cat, err := Load()
	require.NoError(t, err)

	names := cat.Names()
	assert.Greater(t, len(names), 0)

	seen := map[string]bool{}
	for _, n := range names {
		assert.NotEmpty(t, n, "scenario name must not be empty")
		assert.False(t, seen[n], "duplicate scenario name: %s", n)
		seen[n] = true
	}
}

func TestCatalogByCategory(t *testing.T) {
	cat, err := Load()
	require.NoError(t, err)

	for _, c := range cat.Categories {
		scenarios := cat.ByCategory(c.Name)
		assert.NotEmpty(t, scenarios, "category %q has no scenarios", c.Name)
	}
}

func TestCatalogByTag(t *testing.T) {
	cat, err := Load()
	require.NoError(t, err)

	smoke := cat.ByTag("smoke")
	assert.Greater(t, len(smoke), 0, "expected at least one 'smoke' scenario")

	live := cat.ByTag("live")
	assert.Greater(t, len(live), 0, "expected at least one 'live' scenario")

	stub := cat.ByTag("stub")
	assert.Greater(t, len(stub), 0, "expected at least one 'stub' scenario")
}

func TestEveryScenarioHasCategory(t *testing.T) {
	cat, err := Load()
	require.NoError(t, err)

	validCats := map[string]bool{}
	for _, c := range cat.Categories {
		validCats[c.Name] = true
	}

	for _, s := range cat.Scenarios {
		assert.True(t, validCats[s.Category],
			"scenario %q references unknown category %q", s.Name, s.Category)
	}
}

func TestEveryScenarioHasExpectation(t *testing.T) {
	cat, err := Load()
	require.NoError(t, err)

	for _, s := range cat.Scenarios {
		hasExit := s.Expect.Exit != nil || len(s.Expect.ExitAny) > 0
		hasOutput := len(s.Expect.OutputContains) > 0 || len(s.Expect.OutputExcludes) > 0
		hasStdout := len(s.Expect.StdoutContains) > 0 || s.Expect.StdoutIsJSON
		hasCustom := s.Expect.Custom != ""
		hasLockfile := s.Expect.LockfileExists

		assert.True(t, hasExit || hasOutput || hasStdout || hasCustom || hasLockfile,
			"scenario %q has no expectations defined", s.Name)
	}
}

func TestLiveScenariosRequireToken(t *testing.T) {
	cat, err := Load()
	require.NoError(t, err)

	for _, s := range cat.Scenarios {
		if s.HasTag("real_repo") {
			assert.True(t, s.NeedsToken,
				"live repo scenario %q must set needs_token: true", s.Name)
			assert.NotEmpty(t, s.LiveRepo,
				"live repo scenario %q must set live_repo", s.Name)
		}
	}
}

func TestScenarioMatrix(t *testing.T) {
	cat, err := Load()
	require.NoError(t, err)

	t.Logf("Scenario matrix: %d scenarios across %d categories",
		len(cat.Scenarios), len(cat.Categories))

	for _, c := range cat.Categories {
		scenarios := cat.ByCategory(c.Name)
		t.Logf("  %-20s %d scenarios  %s", c.Name, len(scenarios), c.Description)
	}
}
