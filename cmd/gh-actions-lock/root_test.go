package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/github/gh-actions-lock/internal/pinpool"
	"github.com/github/gh-actions-lock/internal/resolve"
)

func TestNewRootCmdSuppressesCobraUsageForHandledErrors(t *testing.T) {
	cmd := newRootCmd(nil)
	if !cmd.SilenceUsage || !cmd.SilenceErrors {
		t.Fatalf("expected root command to suppress Cobra usage/errors for handled failures")
	}
}

func TestNewRootCmd_Version(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newRootCmd(func(string, *pinpool.Pool) (*resolve.Resolver, error) {
		t.Fatalf("unexpected resolver call: --version must skip resolution")
		return nil, nil
	})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--version"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}

	// We check the output shape ("actions-lock version <value>\n") instead of
	// comparing with cliVersion(). cliVersion() reads build info that can change
	// across build setups, and calling it here would only repeat the code.
	const prefix = "actions-lock version "
	out := stdout.String()
	if !strings.HasPrefix(out, prefix) || !strings.HasSuffix(out, "\n") {
		t.Fatalf("unexpected output format: %q", out)
	}
	version := strings.TrimSuffix(strings.TrimPrefix(out, prefix), "\n")
	if version == "" {
		t.Fatalf("expected non-empty version in stdout: %q", out)
	}
}
