package tag

import "testing"

func TestCooldownDays_CaseInsensitiveOverride(t *testing.T) {
	cfg := CooldownConfig{
		DefaultDays:   7,
		RepoOverrides: map[string]int{"Actions/Checkout": 30},
	}

	// Exact-case match.
	if got := cfg.CooldownDays("Actions", "Checkout"); got != 30 {
		t.Errorf("exact case: want 30, got %d", got)
	}
	// Canonicalized (lowercased) lookup must still hit the override.
	if got := cfg.CooldownDays("actions", "checkout"); got != 30 {
		t.Errorf("lowercased: want 30, got %d", got)
	}
	// Unknown repo falls back to the default.
	if got := cfg.CooldownDays("other", "repo"); got != 7 {
		t.Errorf("default: want 7, got %d", got)
	}
}
