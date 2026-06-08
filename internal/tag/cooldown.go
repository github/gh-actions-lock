// Package tag lists tags, classifies versions, and applies release
// cooldown.
package tag

import (
	"strings"
	"time"
)

// CooldownConfig controls the minimum age threshold for tag upgrade suggestions.
type CooldownConfig struct {
	DefaultDays   int
	RepoOverrides map[string]int
}

// CooldownDays returns the cooldown period for a repo, falling back to the default.
func (c CooldownConfig) CooldownDays(owner, repo string) int {
	if days, ok := c.RepoOverrides[owner+"/"+repo]; ok {
		return days
	}
	// Override keys may be written with different owner/repo casing than the
	// canonicalized lookup; fall back to a case-insensitive match.
	want := strings.ToLower(owner + "/" + repo)
	for k, days := range c.RepoOverrides {
		if strings.ToLower(k) == want {
			return days
		}
	}
	return c.DefaultDays
}

// CooldownDuration returns the cooldown as a time.Duration.
func (c CooldownConfig) CooldownDuration(owner, repo string) time.Duration {
	return time.Duration(c.CooldownDays(owner, repo)) * 24 * time.Hour
}
