// Package tag lists tags, classifies versions, and applies release
// cooldown.
package tag

// CooldownConfig controls the minimum age a tag must have before it's
// suggested or pinned. It mirrors the `github-actions` cooldown block in a
// repository's Dependabot config. There is no built-in default: an absent
// config means DefaultDays is 0 and no cooldown filtering applies, matching
// Dependabot's own ReleaseCooldownOptions (every day value initializes to 0).
type CooldownConfig struct {
	// DefaultDays is Dependabot's cooldown `default-days`. Zero disables
	// cooldown filtering.
	DefaultDays int
}
