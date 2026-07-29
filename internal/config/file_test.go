package config

import (
	"os"
	"path/filepath"
	"testing"
)

// writeUserConfig writes body to a temp config.yml and points
// GH_ACTIONS_LOCK_CONFIG at it for the duration of the test.
func writeUserConfig(t *testing.T, body string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_ACTIONS_LOCK_CONFIG", path)
}

func TestFileCooldown_DefaultDays(t *testing.T) {
	writeUserConfig(t, "cooldown_days: 4\n")
	cfg, ok := FileCooldown()
	if !ok || cfg.DefaultDays != 4 {
		t.Fatalf("got cfg=%+v ok=%v, want DefaultDays 4 / true", cfg, ok)
	}
}

func TestFileCooldown_RepoOverrides(t *testing.T) {
	writeUserConfig(t, `
cooldown_days: 2
repos:
  actions/checkout:
    cooldown_days: 10
`)
	cfg, ok := FileCooldown()
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if got := cfg.CooldownDays("actions", "checkout"); got != 10 {
		t.Errorf("override CooldownDays = %d, want 10", got)
	}
	if got := cfg.CooldownDays("actions", "setup-go"); got != 2 {
		t.Errorf("fallback CooldownDays = %d, want 2 (default)", got)
	}
}

func TestFileCooldown_Absent(t *testing.T) {
	t.Setenv("GH_ACTIONS_LOCK_CONFIG", filepath.Join(t.TempDir(), "does-not-exist.yml"))
	if cfg, ok := FileCooldown(); ok {
		t.Errorf("absent file: got cfg=%+v ok=true, want false", cfg)
	}
}

func TestFileCooldown_EmptyIsNotConfigured(t *testing.T) {
	writeUserConfig(t, "cooldown_days: 0\n")
	if cfg, ok := FileCooldown(); ok {
		t.Errorf("cooldown_days 0 with no repos: got cfg=%+v ok=true, want false", cfg)
	}
}

func TestFileCooldown_Malformed(t *testing.T) {
	writeUserConfig(t, "cooldown_days: [not, an, int\n")
	if _, ok := FileCooldown(); ok {
		t.Error("malformed file: ok = true, want false")
	}
}
