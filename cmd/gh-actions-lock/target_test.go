package main

import (
	"testing"

	"github.com/github/gh-actions-lock/internal/repospec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func stubPathExists(t *testing.T, existing ...string) {
	t.Helper()
	orig := pathExists
	t.Cleanup(func() { pathExists = orig })
	pathExists = func(name string) bool {
		for _, e := range existing {
			if e == name {
				return true
			}
		}
		return false
	}
}

func TestResolveTarget(t *testing.T) {
	tests := []struct {
		name       string
		opts       checkOptions
		args       []string
		existing   []string
		wantRemote *repospec.Spec
		wantPaths  []string
		wantErr    string
	}{
		{
			name:      "no verify keeps repo-shaped args as workflow paths",
			opts:      checkOptions{},
			args:      []string{"octo/api"},
			wantPaths: []string{"octo/api"},
		},
		{
			name:      "verify without args",
			opts:      checkOptions{verify: true},
			wantPaths: nil,
		},
		{
			name:       "positional repo",
			opts:       checkOptions{verify: true},
			args:       []string{"octo/api"},
			wantRemote: &repospec.Spec{Owner: "octo", Repo: "api"},
		},
		{
			name:       "positional repo with ref",
			opts:       checkOptions{verify: true},
			args:       []string{"octo/api@v1"},
			wantRemote: &repospec.Spec{Owner: "octo", Repo: "api", Ref: "v1"},
		},
		{
			name:       "positional url",
			opts:       checkOptions{verify: true},
			args:       []string{"https://acme.ghe.com/octo/api"},
			wantRemote: &repospec.Spec{Host: "acme.ghe.com", Owner: "octo", Repo: "api"},
		},
		{
			name:       "positional repo then repo-relative workflows",
			opts:       checkOptions{verify: true},
			args:       []string{"octo/api", ".github/workflows/ci.yml", "octo/api"},
			wantRemote: &repospec.Spec{Owner: "octo", Repo: "api"},
			wantPaths:  []string{".github/workflows/ci.yml", "octo/api"},
		},
		{
			name:      "existing local path wins over repo shape",
			opts:      checkOptions{verify: true},
			args:      []string{"octo/api"},
			existing:  []string{"octo/api"},
			wantPaths: []string{"octo/api"},
		},
		{
			name:      "yml suffix is a local path",
			opts:      checkOptions{verify: true},
			args:      []string{"workflows/ci.yml"},
			wantPaths: []string{"workflows/ci.yml"},
		},
		{
			name:      "only the first arg can be a repo",
			opts:      checkOptions{verify: true},
			args:      []string{".github/workflows/ci.yml", "octo/api"},
			wantPaths: []string{".github/workflows/ci.yml", "octo/api"},
		},
		{
			name:       "-R makes all args repo-relative paths",
			opts:       checkOptions{verify: true, repo: "octo/api"},
			args:       []string{".github/workflows/ci.yml"},
			wantRemote: &repospec.Spec{Owner: "octo", Repo: "api"},
			wantPaths:  []string{".github/workflows/ci.yml"},
		},
		{
			name:    "-R with a positional repo is rejected, even the same one",
			opts:    checkOptions{verify: true, repo: "octo/api"},
			args:    []string{"OCTO/api@main", "a.yml"},
			wantErr: "repository given twice",
		},
		{
			name:    "-R with a positional repo URL on another host is rejected, not merged",
			opts:    checkOptions{verify: true, repo: "octo/api"},
			args:    []string{"https://tenant.ghe.com/octo/api"},
			wantErr: "repository given twice",
		},
		{
			name:    "-R with a different positional repo",
			opts:    checkOptions{verify: true, repo: "octo/api"},
			args:    []string{"octo/other"},
			wantErr: "repository given twice",
		},
		{
			name:    "unsupported repository URL surfaces the URL error",
			opts:    checkOptions{verify: true},
			args:    []string{"https://github.com/octo/api/blob/main/README.md"},
			wantErr: "invalid repository URL",
		},
		{
			name:       "--ref applies to the repo",
			opts:       checkOptions{verify: true, repo: "octo/api", ref: "v2"},
			wantRemote: &repospec.Spec{Owner: "octo", Repo: "api", Ref: "v2"},
		},
		{
			name:       "@ref and identical --ref",
			opts:       checkOptions{verify: true, ref: "v2"},
			args:       []string{"octo/api@v2"},
			wantRemote: &repospec.Spec{Owner: "octo", Repo: "api", Ref: "v2"},
		},
		{
			name:    "@ref and different --ref",
			opts:    checkOptions{verify: true, ref: "v3"},
			args:    []string{"octo/api@v2"},
			wantErr: "conflicting refs",
		},
		{
			name:       "tree url ref when --ref absent",
			opts:       checkOptions{verify: true},
			args:       []string{"https://github.com/octo/api/tree/v9"},
			wantRemote: &repospec.Spec{Host: "github.com", Owner: "octo", Repo: "api", Ref: "v9"},
		},
		{
			name:    "--ref without a remote repository",
			opts:    checkOptions{verify: true, ref: "v2"},
			args:    []string{".github/workflows/ci.yml"},
			wantErr: "--ref requires a remote repository",
		},
		{
			name:    "invalid --repo",
			opts:    checkOptions{verify: true, repo: "nope"},
			wantErr: "invalid --repo",
		},
		{
			name:    "host conflicts with --hostname",
			opts:    checkOptions{verify: true, hostname: "other.ghe.com"},
			args:    []string{"acme.ghe.com/octo/api"},
			wantErr: "conflicts with --hostname",
		},
		{
			name:       "host matches --hostname case-insensitively",
			opts:       checkOptions{verify: true, hostname: "ACME.ghe.com"},
			args:       []string{"acme.ghe.com/octo/api"},
			wantRemote: &repospec.Spec{Host: "acme.ghe.com", Owner: "octo", Repo: "api"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubPathExists(t, tt.existing...)
			opts := tt.opts

			err := opts.resolveTarget(tt.args)

			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantRemote, opts.remote)
			if len(tt.wantPaths) == 0 {
				assert.Empty(t, opts.workflowPaths)
			} else {
				assert.Equal(t, tt.wantPaths, opts.workflowPaths)
			}
		})
	}
}

func TestValidateOutputFlags_Remote(t *testing.T) {
	remote := &repospec.Spec{Owner: "octo", Repo: "api"}
	tests := []struct {
		name    string
		opts    checkOptions
		wantErr string
	}{
		{name: "verify with remote", opts: checkOptions{verify: true, remote: remote}},
		{name: "verify with -R and --ref", opts: checkOptions{verify: true, repo: "octo/api", ref: "main", remote: remote}},
		{name: "-R without verify", opts: checkOptions{repo: "octo/api"}, wantErr: "only valid with --verify"},
		{name: "--ref without verify", opts: checkOptions{ref: "main"}, wantErr: "only valid with --verify"},
		{name: "remote with verify-local", opts: checkOptions{verifyLocal: true, remote: remote}, wantErr: "only valid with --verify"},
		{name: "remote with accept-moved", opts: checkOptions{verify: true, remote: remote, acceptMoved: true}, wantErr: "read-only"},
		{name: "remote with relock", opts: checkOptions{verify: true, remote: remote, relock: true}, wantErr: "read-only"},
		{name: "remote with no-migrate-local-actions", opts: checkOptions{verify: true, remote: remote, noMigrateLocalActions: true}, wantErr: "never migrates"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.opts.validateOutputFlags()
			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr)
				return
			}
			assert.NoError(t, err)
		})
	}
}
