package pipeline

import (
	"testing"

	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/stretchr/testify/assert"
)

func TestIsHex(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"lowercase hex", "abcdef0123456789", true},
		{"uppercase hex", "ABCDEF", true},
		{"empty is vacuously hex", "", true},
		{"non-hex letter", "g123", false},
		{"contains dash", "abc-def", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isHex(tt.in))
		})
	}
}

func TestIsLikelyTag(t *testing.T) {
	fullSHA := "1234567890abcdef1234567890abcdef12345678" // 40 hex chars
	tests := []struct {
		name string
		ref  string
		want bool
	}{
		{"empty", "", false},
		{"main branch", "main", false},
		{"master branch", "master", false},
		{"trunk branch", "trunk", false},
		{"40-char sha", fullSHA, false},
		{"semver tag", "v4.2.1", true},
		{"major tag", "v4", true},
		{"short hex looks like a tag", "abc123", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isLikelyTag(tt.ref))
		})
	}
}

func TestReleasesURL(t *testing.T) {
	// A tag-like ref deep-links to the specific release tag.
	assert.Equal(t,
		"https://github.com/actions/checkout/releases/tag/v4.2.1",
		ReleasesURL("actions", "checkout", "v4.2.1"))

	// A branch ref falls back to the releases index.
	assert.Equal(t,
		"https://github.com/actions/checkout/releases",
		ReleasesURL("actions", "checkout", "main"))

	// A full SHA is not a tag, so it also falls back to the index.
	sha := "1234567890abcdef1234567890abcdef12345678"
	assert.Equal(t,
		"https://github.com/actions/checkout/releases",
		ReleasesURL("actions", "checkout", sha))
}

func TestDocURLFor(t *testing.T) {
	// A known category resolves to a security-hardening anchor.
	assert.Contains(t, DocURLFor(checks.NotPinned), securityHardeningBase)
	// Categories without a finding URL (e.g. Valid) return empty.
	assert.Equal(t, "", DocURLFor(checks.Valid))
}
