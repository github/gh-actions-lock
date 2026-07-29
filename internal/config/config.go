// Package config loads the release-cooldown policy for a repository from two
// sources: the repo's Dependabot config (authoritative) and the user's
// ~/.config/gh-actions-lock/config.yml (fallback). Precedence is composed by
// the command; see ResolveCooldown in cmd/gh-actions-lock/fresh_tag.go.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/github/gh-actions-lock/internal/tag"
	"gopkg.in/yaml.v3"
)

// DependabotCooldown reads the github-actions cooldown policy from repoRoot's
// Dependabot config (.github/dependabot.yml, falling back to .yaml). ok is true
// only when a github-actions entry configures a positive default-days; a block
// with default-days <= 0 (or only unsupported keys) is treated as not
// configured so it can't silently override a stricter policy from another
// source. warnings names configured keys the tool does not yet honor, so config
// is never silently ignored. An absent or malformed config yields ok=false and
// never blocks.
func DependabotCooldown(repoRoot string) (cfg tag.CooldownConfig, ok bool, warnings []string) {
	data, found := readDependabotFile(repoRoot)
	if !found {
		return tag.CooldownConfig{}, false, nil
	}
	var file dependabotConfig
	if err := yaml.Unmarshal(data, &file); err != nil {
		return tag.CooldownConfig{}, false, []string{
			fmt.Sprintf("ignoring malformed Dependabot config: %v", err),
		}
	}
	cd, has := file.actionsCooldown()
	if !has {
		return tag.CooldownConfig{}, false, nil
	}
	return tag.CooldownConfig{DefaultDays: cd.DefaultDays}, cd.DefaultDays > 0, cd.unsupportedWarnings()
}

func readDependabotFile(repoRoot string) ([]byte, bool) {
	for _, name := range []string{"dependabot.yml", "dependabot.yaml"} {
		if data, err := os.ReadFile(filepath.Join(repoRoot, ".github", name)); err == nil {
			return data, true
		}
	}
	return nil, false
}

// FileCooldown reads the user's ~/.config/gh-actions-lock/config.yml cooldown
// settings (top-level cooldown_days plus per-repo repos: overrides). ok is true
// only when the file supplies at least one of those. A missing or malformed
// file yields ok=false and never blocks.
func FileCooldown() (cfg tag.CooldownConfig, ok bool) {
	path := configPath()
	if path == "" {
		return tag.CooldownConfig{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return tag.CooldownConfig{}, false
	}
	var file struct {
		CooldownDays int `yaml:"cooldown_days"`
		Repos        map[string]struct {
			CooldownDays int `yaml:"cooldown_days"`
		} `yaml:"repos"`
	}
	if err := yaml.Unmarshal(data, &file); err != nil {
		return tag.CooldownConfig{}, false
	}
	if file.CooldownDays <= 0 && len(file.Repos) == 0 {
		return tag.CooldownConfig{}, false
	}
	cfg = tag.CooldownConfig{DefaultDays: file.CooldownDays}
	if len(file.Repos) > 0 {
		cfg.RepoOverrides = make(map[string]int, len(file.Repos))
		for nwo, rc := range file.Repos {
			if rc.CooldownDays >= 0 {
				cfg.RepoOverrides[nwo] = rc.CooldownDays
			}
		}
	}
	return cfg, true
}

// configPath returns the config file path, respecting GH_ACTIONS_LOCK_CONFIG
// for testing and demos.
func configPath() string {
	if p := os.Getenv("GH_ACTIONS_LOCK_CONFIG"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "gh-actions-lock", "config.yml")
}
