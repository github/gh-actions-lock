package ghapi

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

type commandOutput func(string, ...string) ([]byte, error)

// SecureToken returns the credential stored for exactly hostname, ignoring
// ambient token environment variables.
func SecureToken(hostname string) (string, error) {
	return secureToken(hostname, func(name string, args ...string) ([]byte, error) {
		if path := os.Getenv("GH_PATH"); path != "" {
			name = path
		}
		return exec.Command(name, args...).Output()
	})
}

func secureToken(hostname string, output commandOutput) (string, error) {
	if hostname == "" {
		return "", fmt.Errorf("hostname is required for secure credential lookup")
	}
	token, err := output("gh", "auth", "token", "--secure-storage", "--hostname", hostname)
	if err != nil || strings.TrimSpace(string(token)) == "" {
		return "", fmt.Errorf("secure credential for %s unavailable; run `gh auth login --hostname %s`", hostname, hostname)
	}
	return strings.TrimSpace(string(token)), nil
}
