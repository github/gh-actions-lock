// Package tag lists tags, classifies versions, and applies release
// cooldown.
package tag

import "time"

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
	return c.DefaultDays
}

// CooldownDuration returns the cooldown as a time.Duration.
func (c CooldownConfig) CooldownDuration(owner, repo string) time.Duration {
	return time.Duration(c.CooldownDays(owner, repo)) * 24 * time.Hour
}
