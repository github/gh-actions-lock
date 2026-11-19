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

func chdirTemp(t *testing.T, dir string) {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(orig)
		startDir, repoRoot = "", ""
	})
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
}

func TestEnterRepoRoot(t *testing.T) {
	root := t.TempDir()
	wfDir := filepath.Join(root, ".github", "workflows")
	if err := os.MkdirAll(wfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	chdirTemp(t, wfDir)
	t.Setenv("GH_ACTIONS_LOCK_WORKFLOWS_DIR", ".")

	if err := enterRepoRoot(); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("GH_ACTIONS_LOCK_WORKFLOWS_DIR"); !filepath.IsAbs(got) || filepath.Base(got) != "workflows" {
		t.Errorf("WORKFLOWS_DIR = %q, want absolute path to original dir", got)
	}
	if got, want := rebasePath("ci.yml"), filepath.Join(".github", "workflows", "ci.yml"); got != want {
		t.Errorf("rebasePath(ci.yml) = %q, want %q", got, want)
	}
}

func TestEnterRepoRootStaysInCheckoutNamedDotGithub(t *testing.T) {
	checkout := filepath.Join(t.TempDir(), ".github")
	if err := os.MkdirAll(filepath.Join(checkout, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(checkout, ".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	chdirTemp(t, checkout)

	if err := enterRepoRoot(); err != nil {
		t.Fatal(err)
	}
	if startDir != "" {
		t.Errorf("moved out of a checkout root")
	}
}

func TestRepoRootFromGitHubDir(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	noGit := t.TempDir()
	for name, tc := range map[string]struct{ in, want string }{
		"workflows":       {filepath.Join(repo, ".github", "workflows"), repo},
		"github":          {filepath.Join(repo, ".github"), repo},
		"mixed case":      {filepath.Join(repo, ".GitHub", "Workflows"), repo},
		"other dir":       {filepath.Join(repo, "src"), ""},
		"no .git at root": {filepath.Join(noGit, ".github"), ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := repoRootFromGitHubDir(tc.in); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
