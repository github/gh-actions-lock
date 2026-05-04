package doctor

import (
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// configPath returns the path to the config file, respecting
// GH_ACTIONS_PIN_CONFIG for testing/demos.
func configPath() string {
	if p := os.Getenv("GH_ACTIONS_PIN_CONFIG"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "gh-actions-pin", "config.yml")
}

// ReachabilityEnabled reads the reachability_check flag from the config file.
// Defaults to false (disabled).
//
// The reachability check uses the undocumented /{owner}/{repo}/branch_commits/{sha}
// web endpoint to detect fork-network injection (impostor commits). This endpoint
// is aggressive with 429 rate limits and has no public API equivalent. Until GitHub
// exposes a documented "commit reachability" or "commit contains" API, this feature
// is dead in the water for most users and disabled by default.
//
// To enable:
//
//	# ~/.config/gh-actions-pin/config.yml
//	reachability_check: true
func ReachabilityEnabled() bool {
	p := configPath()
	if p == "" {
		return false
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return false
	}
	var file struct {
		ReachabilityCheck bool `yaml:"reachability_check"`
	}
	if err := yaml.Unmarshal(data, &file); err != nil {
		return false
	}
	return file.ReachabilityCheck
}
