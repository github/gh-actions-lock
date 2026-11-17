package main

import (
	"bytes"
	"os"
	"path/filepath"
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

func TestEnterRepoRootRewritesArgs(t *testing.T) {
	root := t.TempDir()
	wfDir := filepath.Join(root, ".github", "workflows")
	if err := os.MkdirAll(wfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wfDir, "ci.yml"), []byte("on: push\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
	if err := os.Chdir(wfDir); err != nil {
		t.Fatal(err)
	}

	args := []string{"--no-fix", "ci.yml", "missing.yml"}
	if err := enterRepoRoot(args); err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(".github", "workflows", "ci.yml"); args[1] != want {
		t.Errorf("args[1] = %q, want %q", args[1], want)
	}
	if args[0] != "--no-fix" || args[2] != "missing.yml" {
		t.Errorf("unrelated args modified: %v", args)
	}
	if _, err := os.Stat(args[1]); err != nil {
		t.Errorf("rewritten path not valid from new cwd: %v", err)
	}
}

func TestRepoRootFromGitHubDir(t *testing.T) {
	for in, want := range map[string]string{
		filepath.Join("r", ".github", "workflows"): "r",
		filepath.Join("r", ".github"): "r",
		filepath.Join("r", "src"): "",
	} {
		if got := repoRootFromGitHubDir(in); got != want {
			t.Errorf("repoRootFromGitHubDir(%q) = %q, want %q", in, got, want)
		}
	}
}
