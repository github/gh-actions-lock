// Package config loads CLI configuration from file and environment.
package config

import (
	"os"
	"path/filepath"
	"strconv"

	"github.com/github/gh-actions-lock/internal/tag"
	"gopkg.in/yaml.v3"
)

// Config holds process-wide settings loaded from the config file and
// environment variables. Create one via LoadConfig at startup and pass
// it through the pipeline.
type Config struct {
	// Path is the resolved config file path (empty if none found).
	Path string
	// Cooldown controls how recently a tag must have been released to
	// be excluded from upgrade suggestions.
	Cooldown tag.CooldownConfig
	// Workers is the concurrency limit for pool-parallelized phases.
	// Defaults to 8; overridden by GH_ACTIONS_LOCK_WORKERS.
	Workers int
	// StallHintMS is the stall-detection threshold in milliseconds.
	// 0 disables the watcher. Overridden by GH_ACTIONS_LOCK_STALL_HINT_MS.
	StallHintMS int
	// DebugProgress enables per-phase progress tracing.
	DebugProgress bool
}

// Load reads the config file and environment, returning a Config
// with sensible defaults for any unset values.
func Load() Config {
	p := configPath()
	c := Config{
		Path:          p,
		Workers:       8,
		StallHintMS:   -1, // sentinel: use pinpool default
		DebugProgress: envBool("GH_ACTIONS_LOCK_DEBUG_PROGRESS"),
	}

	if v, err := strconv.Atoi(os.Getenv("GH_ACTIONS_LOCK_WORKERS")); err == nil && v > 0 {
		c.Workers = v
	}
	if v := os.Getenv("GH_ACTIONS_LOCK_STALL_HINT_MS"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil {
			c.StallHintMS = ms
		}
	}

	c.Cooldown = loadCooldownFromFile(p)
	return c
}

func envBool(key string) bool {
	v := os.Getenv(key)
	return v == "1" || v == "true" || v == "yes"
}

// loadCooldownFromFile reads cooldown settings from the config file.
func loadCooldownFromFile(path string) tag.CooldownConfig {
	cfg := tag.CooldownConfig{
		DefaultDays:   3,
		RepoOverrides: make(map[string]int),
	}
	if path == "" {
		return cfg
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	var file struct {
		CooldownDays int `yaml:"cooldown_days"`
		Repos        map[string]struct {
			CooldownDays int `yaml:"cooldown_days"`
		} `yaml:"repos"`
	}
	if err := yaml.Unmarshal(data, &file); err != nil {
		return cfg
	}
	if file.CooldownDays > 0 {
		cfg.DefaultDays = file.CooldownDays
	}
	for nwo, repoCfg := range file.Repos {
		if repoCfg.CooldownDays >= 0 {
			cfg.RepoOverrides[nwo] = repoCfg.CooldownDays
		}
	}
	return cfg
}

// configPath returns the path to the config file, respecting
// GH_ACTIONS_LOCK_CONFIG for testing/demos.
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
