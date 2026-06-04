package doctor

import (
	"os"
	"path/filepath"
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
