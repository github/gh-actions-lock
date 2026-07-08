package main

import (
	"os"
)

// isCodespace reports whether the process is running inside a GitHub Codespace.
func isCodespace() bool {
	return os.Getenv("CODESPACES") == "true"
}

// ssoFixHints returns actionable guidance for SSO failures. In a Codespace
// it points to devcontainer permissions; otherwise it defers to gh auth.
func ssoFixHints() []string {
	if isCodespace() {
		return []string{
			"Your codespace's GITHUB_TOKEN cannot be SSO-authorized.",
			"Update your devcontainer permissions: https://docs.github.com/en/codespaces/managing-your-codespaces/managing-repository-access-for-your-codespaces#authorizing-requested-permissions",
		}
	}
	return []string{
		"Run:  gh auth refresh -h github.com",
	}
}
