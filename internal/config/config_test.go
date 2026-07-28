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

func TestLoadCooldown_RealWorldFixture(t *testing.T) {
	// Reads an actual .github/dependabot.yml off disk (testdata/repo) whose
	// shape mirrors GitHub's documented cooldown schema: multiple ecosystems,
	// github-actions NOT first, and every documented cooldown key set.
	cfg, warnings := LoadCooldown("testdata/repo")
	if cfg.DefaultDays != 5 {
		t.Errorf("DefaultDays = %d, want 5 (github-actions entry, not the npm 7)", cfg.DefaultDays)
	}
	if len(warnings) != 2 {
		t.Fatalf("warnings = %v, want 2 (semver-* and include/exclude)", warnings)
	}
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

func TestLoadCooldown_ExplicitZeroDays(t *testing.T) {
	dir := writeDependabot(t, "dependabot.yml", `
version: 2
updates:
  - package-ecosystem: "github-actions"
    cooldown:
      default-days: 0
`)
	cfg, warnings := LoadCooldown(dir)
	if cfg.DefaultDays != 0 {
		t.Errorf("DefaultDays = %d, want 0", cfg.DefaultDays)
	}
	if len(warnings) != 0 {
		t.Errorf("explicit 0 is not an unsupported key; warnings = %v", warnings)
	}
}

func TestLoadCooldown_EmptyCooldownBlock(t *testing.T) {
	// `cooldown:` with no mapping parses to a nil pointer, i.e. no block.
	dir := writeDependabot(t, "dependabot.yml", `
version: 2
updates:
  - package-ecosystem: "github-actions"
    cooldown:
`)
	cfg, warnings := LoadCooldown(dir)
	if cfg.DefaultDays != 0 || len(warnings) != 0 {
		t.Errorf("empty cooldown: got DefaultDays=%d warnings=%v, want 0 / none", cfg.DefaultDays, warnings)
	}
}

func TestLoadCooldown_FirstActionsEntryWithCooldownWins(t *testing.T) {
	// Multi-directory setups can list github-actions twice; take the first
	// entry that actually carries a cooldown block.
	dir := writeDependabot(t, "dependabot.yml", `
version: 2
updates:
  - package-ecosystem: "github-actions"
    directory: "/"
  - package-ecosystem: "github-actions"
    directory: "/nested"
    cooldown:
      default-days: 4
`)
	cfg, _ := LoadCooldown(dir)
	if cfg.DefaultDays != 4 {
		t.Errorf("DefaultDays = %d, want 4 (first entry with a cooldown block)", cfg.DefaultDays)
	}
}

// TestLoadCooldown_KeyTagsAreDistinct pins each YAML tag independently so a
// single mistyped struct tag can't hide behind a sibling key. semver-minor-days
// alone must trip the semver warning; exclude alone must trip the filter
// warning.
func TestLoadCooldown_KeyTagsAreDistinct(t *testing.T) {
	semverOnly := writeDependabot(t, "dependabot.yml", `
version: 2
updates:
  - package-ecosystem: "github-actions"
    cooldown:
      semver-minor-days: 7
`)
	if _, w := LoadCooldown(semverOnly); len(w) != 1 {
		t.Errorf("semver-minor-days alone: warnings = %v, want 1", w)
	}

	excludeOnly := writeDependabot(t, "dependabot.yml", `
version: 2
updates:
  - package-ecosystem: "github-actions"
    cooldown:
      exclude:
        - "actions/checkout"
`)
	if _, w := LoadCooldown(excludeOnly); len(w) != 1 {
		t.Errorf("exclude alone: warnings = %v, want 1", w)
	}
}
