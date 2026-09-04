package ghapi

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// SecureToken returns gh's configured credential for exactly hostname without
// allowing ambient token variables to override it.
func SecureToken(hostname string) (string, error) {
	if hostname == "" {
		return "", errors.New("hostname is required for credential lookup")
	}

	name := "gh"
	if path := os.Getenv("GH_PATH"); path != "" {
		name = path
	}
	cmd := exec.Command(name, "auth", "token", "--hostname", hostname)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN":
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}

	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			missing := fmt.Sprintf("no oauth token found for %s", hostname)
			if strings.TrimSpace(string(exitErr.Stderr)) == missing {
				return "", fmt.Errorf("no credential configured for %s; run `gh auth login --hostname %s`", hostname, hostname)
			}
			return "", fmt.Errorf("gh credential lookup for %s failed (%s)", hostname, exitErr.ProcessState)
		}
		return "", fmt.Errorf("running gh credential lookup for %s: %w", hostname, err)
	}
	token := strings.TrimSpace(string(output))
	if token == "" {
		return "", fmt.Errorf("gh credential lookup for %s returned no token", hostname)
	}
	return token, nil
}
