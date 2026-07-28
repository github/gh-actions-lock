package config

import (
	"os"
	"path/filepath"
	"testing"
)

// writeDependabot writes body to <dir>/.github/<name> and returns dir.
func writeDependabot(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	gh := filepath.Join(dir, ".github")
	if err := os.MkdirAll(gh, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gh, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadCooldown_PresentWithCooldown(t *testing.T) {
	dir := writeDependabot(t, "dependabot.yml", `
version: 2
updates:
  - package-ecosystem: "github-actions"
    directory: "/"
    schedule:
      interval: "weekly"
    cooldown:
      default-days: 5
`)
	cfg, warnings := LoadCooldown(dir)
	if cfg.DefaultDays != 5 {
		t.Errorf("DefaultDays = %d, want 5", cfg.DefaultDays)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
}

func TestLoadCooldown_YAMLExtension(t *testing.T) {
	dir := writeDependabot(t, "dependabot.yaml", `
version: 2
updates:
  - package-ecosystem: "github-actions"
    cooldown:
      default-days: 2
`)
	cfg, _ := LoadCooldown(dir)
	if cfg.DefaultDays != 2 {
		t.Errorf("DefaultDays = %d, want 2 (.yaml must be read)", cfg.DefaultDays)
	}
}

func TestLoadCooldown_PresentWithoutCooldownBlock(t *testing.T) {
	dir := writeDependabot(t, "dependabot.yml", `
version: 2
updates:
  - package-ecosystem: "github-actions"
    directory: "/"
    schedule:
      interval: "weekly"
`)
	cfg, warnings := LoadCooldown(dir)
	if cfg.DefaultDays != 0 {
		t.Errorf("DefaultDays = %d, want 0 (no cooldown block)", cfg.DefaultDays)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
}

func TestLoadCooldown_OtherEcosystemIgnored(t *testing.T) {
	dir := writeDependabot(t, "dependabot.yml", `
version: 2
updates:
  - package-ecosystem: "npm"
    cooldown:
      default-days: 9
`)
	cfg, _ := LoadCooldown(dir)
	if cfg.DefaultDays != 0 {
		t.Errorf("DefaultDays = %d, want 0 (only github-actions counts)", cfg.DefaultDays)
	}
}

func TestLoadCooldown_Absent(t *testing.T) {
	cfg, warnings := LoadCooldown(t.TempDir())
	if cfg.DefaultDays != 0 {
		t.Errorf("DefaultDays = %d, want 0 (no config file)", cfg.DefaultDays)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
}

func TestLoadCooldown_Malformed(t *testing.T) {
	dir := writeDependabot(t, "dependabot.yml", "updates: [this: is: not: valid")
	cfg, warnings := LoadCooldown(dir)
	if cfg.DefaultDays != 0 {
		t.Errorf("DefaultDays = %d, want 0 (malformed must not block)", cfg.DefaultDays)
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want one malformed-config warning", warnings)
	}
}

func TestLoadCooldown_UnsupportedKeysWarn(t *testing.T) {
	dir := writeDependabot(t, "dependabot.yml", `
version: 2
updates:
  - package-ecosystem: "github-actions"
    cooldown:
      default-days: 3
      semver-major-days: 30
      include:
        - "actions/*"
`)
	cfg, warnings := LoadCooldown(dir)
	if cfg.DefaultDays != 3 {
		t.Errorf("DefaultDays = %d, want 3", cfg.DefaultDays)
	}
	if len(warnings) != 2 {
		t.Fatalf("warnings = %v, want 2 (semver-* and include/exclude)", warnings)
	}
}
