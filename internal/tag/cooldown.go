// Package tag lists tags, classifies versions, and applies release
// cooldown.
package tag

import "strings"

// CooldownConfig controls the minimum age a tag must have before it's
// suggested or pinned. DefaultDays applies to every action; RepoOverrides sets
// a per-repo (owner/name) value that wins over the default. Zero DefaultDays
// with no matching override disables cooldown filtering.
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
