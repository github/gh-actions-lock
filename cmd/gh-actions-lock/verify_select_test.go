package main

import (
	"testing"

	"github.com/github/gh-actions-lock/internal/repospec"
	"github.com/stretchr/testify/assert"
)

func TestSelectVerifier(t *testing.T) {
	remote := &repospec.Spec{Owner: "octo", Repo: "api"}
	tests := []struct {
		name string
		opts checkOptions
		want any
	}{
		{name: "regular check run", opts: checkOptions{}},
		{name: "plain verify stays in runCheck", opts: checkOptions{verify: true}},
		{name: "verify-local", opts: checkOptions{verifyLocal: true}, want: localVerifier{}},
		{name: "remote", opts: checkOptions{verify: true, remote: remote}, want: remoteVerifier{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := selectVerifier(&tt.opts, nil)
			if tt.want == nil {
				assert.Nil(t, got)
				return
			}
			assert.IsType(t, tt.want, got)
		})
	}
}
