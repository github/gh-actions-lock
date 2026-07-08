package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSSOFixHints(t *testing.T) {
	t.Run("codespace points to devcontainer docs", func(t *testing.T) {
		t.Setenv("CODESPACES", "true")
		hints := ssoFixHints()
		assert.Len(t, hints, 2)
		assert.Contains(t, hints[0], "cannot be SSO-authorized")
		assert.Contains(t, hints[1], "devcontainer permissions")
	})

	t.Run("non-codespace points to gh auth refresh", func(t *testing.T) {
		t.Setenv("CODESPACES", "")
		hints := ssoFixHints()
		assert.Len(t, hints, 1)
		assert.Contains(t, hints[0], "gh auth refresh")
	})
}
