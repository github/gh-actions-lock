package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCooldownFlagPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name       string
		dependabot int
		userConfig string
		flag       string
		ageDays    int
		reject     bool
	}{
		{name: "flag alone is enforced", flag: "7", ageDays: 2, reject: true},
		{name: "flag beats Dependabot default", dependabot: 30, flag: "1", ageDays: 2},
		{name: "flag can tighten Dependabot", dependabot: 1, flag: "7", ageDays: 2, reject: true},
		{name: "flag beats user default", userConfig: "cooldown_days: 30\n", flag: "1", ageDays: 2},
		{name: "positive flag keeps per-repo policy", userConfig: "cooldown_days: 30\nrepos:\n  actions/checkout:\n    cooldown_days: 14\n", flag: "1", ageDays: 2, reject: true},
		{name: "positive flag keeps per-repo opt out", userConfig: "cooldown_days: 30\nrepos:\n  actions/checkout:\n    cooldown_days: 0\n", flag: "7"},
		{name: "zero disables Dependabot", dependabot: 30, flag: "0"},
		{name: "zero disables per-repo override", userConfig: "cooldown_days: 30\nrepos:\n  actions/checkout:\n    cooldown_days: 14\n", flag: "0"},
		{name: "omitting flag retains Dependabot", dependabot: 7, ageDays: 2, reject: true},
		{name: "omitting flag retains user config", userConfig: "cooldown_days: 7\n", ageDays: 2, reject: true},
		{name: "Dependabot does not import user overrides", dependabot: 30, userConfig: "cooldown_days: 30\nrepos:\n  actions/checkout:\n    cooldown_days: 14\n", flag: "1", ageDays: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempWorkflow(t, "jobs:\n  build:\n    steps:\n      - uses: actions/checkout@main\n")
			configureDependabotCooldown(t, tc.dependabot)
			if tc.userConfig != "" {
				writeUserFileConfig(t, tc.userConfig)
			}
			rt := &cooldownTransport{commitDate: time.Now().Add(-time.Duration(tc.ageDays) * 24 * time.Hour).Format(time.RFC3339)}
			args := []string{"--no-interactive", "--no-narrow", path}
			if tc.flag != "" {
				args = append(args, "--cooldown-days", tc.flag)
			}
			out, err := runParity(t, rt, args...)
			if tc.reject {
				require.ErrorIs(t, err, errSilent)
				assert.False(t, out.Valid)
				assert.Contains(t, fmt.Sprint(out.Findings), "release-cooldown")
				assert.NoFileExists(t, ".github/workflows/actions.lock")
			} else {
				require.NoError(t, err)
				assert.Contains(t, readTempLockfilePins(t), parityTestSHA)
				if tc.ageDays == 0 {
					assert.Zero(t, rt.commitReads, "disabled cooldown must not fetch candidate dates")
				}
			}
		})
	}
}

func TestCooldownFlagRejectsNegativeBeforeWork(t *testing.T) {
	rt := &requestCountingTransport{}
	stdout, _, err := runCommandWithHTTP(t, rt, "--cooldown-days=-1", "--verify-local")
	require.ErrorContains(t, err, "--cooldown-days must be non-negative")
	assert.Empty(t, stdout)
	assert.Zero(t, rt.calls.Load())
}

func TestCooldownFlagHelpExplainsOverrides(t *testing.T) {
	cmd := newRootCmd(nil)
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--help"})
	require.NoError(t, cmd.Execute())
	assert.Contains(t, out.String(), "--cooldown-days")
	assert.Contains(t, out.String(), "Positive flag values preserve")
	assert.Contains(t, out.String(), "--cooldown-days 0 disables all")
	assert.Contains(t, out.String(), "not runtime age enforcement")
}
