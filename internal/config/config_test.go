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

func TestDependabotCooldown_RealWorldFixture(t *testing.T) {
	// Reads an actual .github/dependabot.yml off disk (testdata/repo) whose
	// shape mirrors GitHub's documented cooldown schema: multiple ecosystems,
	// github-actions NOT first, and every documented cooldown key set.
	cfg, _, warnings := DependabotCooldown("testdata/repo")
	if cfg.DefaultDays != 5 {
		t.Errorf("DefaultDays = %d, want 5 (github-actions entry, not the npm 7)", cfg.DefaultDays)
	}
	if len(warnings) != 2 {
		t.Fatalf("warnings = %v, want 2 (semver-* and include/exclude)", warnings)
	}
}

func TestDependabotCooldown_Table(t *testing.T) {
	tests := []struct {
		name     string
		filename string // default dependabot.yml; empty means write no file
		yaml     string
		wantDays int
		wantOK   bool
		wantWarn int
	}{
		{
			name:     "present with cooldown",
			yaml:     "version: 2\nupdates:\n  - package-ecosystem: \"github-actions\"\n    directory: \"/\"\n    schedule:\n      interval: \"weekly\"\n    cooldown:\n      default-days: 5\n",
			wantDays: 5, wantOK: true,
		},
		{
			name:     "yaml extension is read",
			filename: "dependabot.yaml",
			yaml:     "version: 2\nupdates:\n  - package-ecosystem: \"github-actions\"\n    cooldown:\n      default-days: 2\n",
			wantDays: 2, wantOK: true,
		},
		{
			name: "present without cooldown block",
			yaml: "version: 2\nupdates:\n  - package-ecosystem: \"github-actions\"\n    directory: \"/\"\n    schedule:\n      interval: \"weekly\"\n",
		},
		{
			name: "other ecosystem ignored",
			yaml: "version: 2\nupdates:\n  - package-ecosystem: \"npm\"\n    cooldown:\n      default-days: 9\n",
		},
		{
			name: "absent config file",
		},
		{
			name:     "malformed does not block",
			yaml:     "updates: [this: is: not: valid",
			wantWarn: 1,
		},
		{
			name:     "unsupported keys warn",
			yaml:     "version: 2\nupdates:\n  - package-ecosystem: \"github-actions\"\n    cooldown:\n      default-days: 3\n      semver-major-days: 30\n      include:\n        - \"actions/*\"\n",
			wantDays: 3, wantOK: true, wantWarn: 2,
		},
		{
			name: "explicit zero is not configured",
			yaml: "version: 2\nupdates:\n  - package-ecosystem: \"github-actions\"\n    cooldown:\n      default-days: 0\n",
		},
		{
			name: "empty cooldown block",
			yaml: "version: 2\nupdates:\n  - package-ecosystem: \"github-actions\"\n    cooldown:\n",
		},
		{
			name:     "first actions entry with a cooldown wins",
			yaml:     "version: 2\nupdates:\n  - package-ecosystem: \"github-actions\"\n    directory: \"/\"\n  - package-ecosystem: \"github-actions\"\n    directory: \"/nested\"\n    cooldown:\n      default-days: 4\n",
			wantDays: 4, wantOK: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.yaml != "" {
				name := tt.filename
				if name == "" {
					name = "dependabot.yml"
				}
				dir = writeDependabot(t, name, tt.yaml)
			}
			cfg, ok, warnings := DependabotCooldown(dir)
			if cfg.DefaultDays != tt.wantDays {
				t.Errorf("DefaultDays = %d, want %d", cfg.DefaultDays, tt.wantDays)
			}
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v", ok, tt.wantOK)
			}
			if len(warnings) != tt.wantWarn {
				t.Errorf("warnings = %v, want %d", warnings, tt.wantWarn)
			}
		})
	}
}

// TestDependabotCooldown_KeyTagsAreDistinct pins each YAML tag independently so a
// single mistyped struct tag can't hide behind a sibling key. semver-minor-days
// alone must trip the semver warning; exclude alone must trip the filter
// warning.
func TestDependabotCooldown_KeyTagsAreDistinct(t *testing.T) {
	semverOnly := writeDependabot(t, "dependabot.yml", `
version: 2
updates:
  - package-ecosystem: "github-actions"
    cooldown:
      semver-minor-days: 7
`)
	if _, _, w := DependabotCooldown(semverOnly); len(w) != 1 {
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
	if _, _, w := DependabotCooldown(excludeOnly); len(w) != 1 {
		t.Errorf("exclude alone: warnings = %v, want 1", w)
	}
}
