package main

import (
	"os"
	"path/filepath"
	"testing"
)

// writeRepoDependabot writes a .github/dependabot.yml under a fresh temp dir
// and returns the dir (a stand-in repo root).
func writeRepoDependabot(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	gh := filepath.Join(dir, ".github")
	if err := os.MkdirAll(gh, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gh, "dependabot.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestResolveCooldown_DependabotWins(t *testing.T) {
	// Both sources present: Dependabot is gospel, the config file is ignored.
	writeUserFileConfig(t, "cooldown_days: 9\n")
	dir := writeRepoDependabot(t, `
version: 2
updates:
  - package-ecosystem: "github-actions"
    cooldown:
      default-days: 5
`)
	cfg, configured, _ := ResolveCooldown(dir)
	if !configured || cfg.DefaultDays != 5 {
		t.Fatalf("got cfg=%+v configured=%v, want DefaultDays 5 (Dependabot wins)", cfg, configured)
	}
}

func TestResolveCooldown_FileFallback(t *testing.T) {
	// No Dependabot cooldown -> fall back to the user config file.
	writeUserFileConfig(t, "cooldown_days: 9\n")
	cfg, configured, _ := ResolveCooldown(t.TempDir())
	if !configured || cfg.DefaultDays != 9 {
		t.Fatalf("got cfg=%+v configured=%v, want DefaultDays 9 (file fallback)", cfg, configured)
	}
}

func TestResolveCooldown_NeitherIsUnconfigured(t *testing.T) {
	// No Dependabot, no config file -> unconfigured, so the caller applies the
	// global fresh-tag warn rather than a silent filter.
	t.Setenv("GH_ACTIONS_LOCK_CONFIG", filepath.Join(t.TempDir(), "none.yml"))
	cfg, configured, _ := ResolveCooldown(t.TempDir())
	if configured || cfg.DefaultDays != 0 {
		t.Fatalf("got cfg=%+v configured=%v, want empty / false", cfg, configured)
	}
}

func TestResolveCooldown_MalformedDependabotFallsBackAndWarns(t *testing.T) {
	// Malformed Dependabot still surfaces a warning but falls through to the
	// config file rather than blocking.
	writeUserFileConfig(t, "cooldown_days: 6\n")
	dir := writeRepoDependabot(t, "updates: [ this is : broken\n")
	cfg, configured, warnings := ResolveCooldown(dir)
	if !configured || cfg.DefaultDays != 6 {
		t.Errorf("got cfg=%+v configured=%v, want DefaultDays 6", cfg, configured)
	}
	if len(warnings) != 1 {
		t.Errorf("warnings = %v, want 1 (malformed dependabot)", warnings)
	}
}

func TestResolveCooldown_DependabotZeroCannotDowngradeFile(t *testing.T) {
	// A repo's default-days: 0 must NOT override the operator's stricter file
	// policy — it falls through instead of silently disabling cooldown.
	writeUserFileConfig(t, "cooldown_days: 14\n")
	dir := writeRepoDependabot(t, `
version: 2
updates:
  - package-ecosystem: "github-actions"
    cooldown:
      default-days: 0
`)
	cfg, configured, _ := ResolveCooldown(dir)
	if !configured || cfg.DefaultDays != 14 {
		t.Fatalf("got cfg=%+v configured=%v, want DefaultDays 14 (file, not the repo's 0)", cfg, configured)
	}
}

func TestResolveCooldown_DependabotZeroAloneIsUnconfigured(t *testing.T) {
	// default-days: 0 with no file policy resolves to unconfigured, so the
	// fresh-tag warn fires rather than a silent no-filter.
	t.Setenv("GH_ACTIONS_LOCK_CONFIG", filepath.Join(t.TempDir(), "none.yml"))
	dir := writeRepoDependabot(t, `
version: 2
updates:
  - package-ecosystem: "github-actions"
    cooldown:
      default-days: 0
`)
	if _, configured, _ := ResolveCooldown(dir); configured {
		t.Error("configured = true, want false (default-days 0 is not a policy)")
	}
}

func TestResolveCooldown_UnsupportedKeysOnlyFallsThroughButWarns(t *testing.T) {
	// A cooldown block with only unsupported keys (no default-days) applies no
	// filter, falls through to the file policy, and still surfaces the warning.
	writeUserFileConfig(t, "cooldown_days: 8\n")
	dir := writeRepoDependabot(t, `
version: 2
updates:
  - package-ecosystem: "github-actions"
    cooldown:
      semver-major-days: 30
`)
	cfg, configured, warnings := ResolveCooldown(dir)
	if !configured || cfg.DefaultDays != 8 {
		t.Errorf("got cfg=%+v configured=%v, want DefaultDays 8 (file)", cfg, configured)
	}
	if len(warnings) != 1 {
		t.Errorf("warnings = %v, want 1 (unsupported semver key)", warnings)
	}
}

// writeUserFileConfig points GH_ACTIONS_LOCK_CONFIG at a temp config.yml.
func writeUserFileConfig(t *testing.T, body string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_ACTIONS_LOCK_CONFIG", path)
}
