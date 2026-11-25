package workflowfile

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mkJunction needs no privileges, unlike os.Symlink on Windows.
func mkJunction(t *testing.T, link, target string) {
	t.Helper()
	out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
	require.NoError(t, err, string(out))
}

func TestEvalSymlinks_Junction(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	require.NoError(t, os.MkdirAll(filepath.Join(target, "sub"), 0o755))
	link := filepath.Join(dir, "link")
	mkJunction(t, link, target)
	wantTarget, err := evalSymlinks(target)
	require.NoError(t, err)

	tests := []struct {
		name string
		path string
		want string
	}{
		{"plain directory", target, wantTarget},
		{"junction", link, wantTarget},
		{"below a junction", filepath.Join(link, "sub"), filepath.Join(wantTarget, "sub")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := evalSymlinks(tt.path)

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}

	t.Run("missing below a junction", func(t *testing.T) {
		_, err := evalSymlinks(filepath.Join(link, "missing"))

		assert.ErrorIs(t, err, fs.ErrNotExist)
	})
}

func TestValidatePathWithinRoot_Junction(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	outside := filepath.Join(dir, "outside")
	for _, d := range []string{filepath.Join(repo, ".github", "workflows"), filepath.Join(repo, "actions"), outside} {
		require.NoError(t, os.MkdirAll(d, 0o755))
	}
	for _, f := range []string{
		filepath.Join(repo, ".github", "workflows", "ci.yml"),
		filepath.Join(repo, "actions", "action.yml"),
		filepath.Join(outside, "action.yml"),
	} {
		require.NoError(t, os.WriteFile(f, []byte("name: x\n"), 0o644))
	}
	repoLink := filepath.Join(dir, "repo-link")
	mkJunction(t, repoLink, repo)
	mkJunction(t, filepath.Join(repo, "alias"), filepath.Join(repo, "actions"))
	mkJunction(t, filepath.Join(repo, "escape"), outside)

	tests := []struct {
		name    string
		root    string
		path    string
		wantErr error
	}{
		{"root reached through a junction", repoLink, filepath.Join(repoLink, ".github", "workflows", "ci.yml"), nil},
		{"junction inside the root", repo, filepath.Join(repo, "alias"), nil},
		{"below a junction inside the root", repo, filepath.Join(repo, "alias", "action.yml"), nil},
		{"junction out of the root", repo, filepath.Join(repo, "escape"), errPathOutsideRoot},
		{"below a junction out of the root", repo, filepath.Join(repo, "escape", "action.yml"), errPathOutsideRoot},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePathWithinRoot(tt.root, tt.path)

			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
				return
			}
			assert.NoError(t, err)
		})
	}
}

func TestValidateInRepo_RepoBehindJunction(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".github", "workflows"), 0o755))
	workflow := filepath.Join(repo, ".github", "workflows", "ci.yml")
	require.NoError(t, os.WriteFile(workflow, []byte("name: ci\n"), 0o644))
	repoLink := filepath.Join(dir, "repo-link")
	mkJunction(t, repoLink, repo)

	assert.NoError(t, ValidateInRepo(filepath.Join(repoLink, ".github", "workflows", "ci.yml")))
}

func TestEvalSymlinks_LongPath(t *testing.T) {
	dir := t.TempDir()
	rel := filepath.Join(strings.Repeat("a", 100), strings.Repeat("b", 100), strings.Repeat("c", 100))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, rel), 0o755))
	wantDir, err := evalSymlinks(dir)
	require.NoError(t, err)

	got, err := evalSymlinks(filepath.Join(dir, rel))

	require.NoError(t, err)
	assert.Equal(t, filepath.Join(wantDir, rel), got, "no extended prefix in the result")
	assert.Greater(t, len(got), 260)
	assert.NoError(t, ValidatePathWithinRoot(dir, filepath.Join(dir, rel)))
}

func TestExtendedPath(t *testing.T) {
	long := strings.Repeat("a", 250)
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"short drive path", `C:\a\b`, `C:\a\b`},
		{"long drive path", `C:\` + long, `\\?\C:\` + long},
		{"long UNC path", `\\srv\share\` + long, `\\?\UNC\srv\share\` + long},
		{"long path needing cleaning", `C:\a\..\` + long, `\\?\C:\` + long},
		{"already extended", `\\?\C:\` + long, `\\?\C:\` + long},
		{"device path", `\\.\pipe\` + long, `\\.\pipe\` + long},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := extendedPath(tt.in)

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestTrimExtendedPrefix(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"drive letter", `\\?\C:\a\b`, `C:\a\b`},
		{"UNC", `\\?\UNC\srv\share\a`, `\\srv\share\a`},
		{"already plain", `C:\a\b`, `C:\a\b`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, trimExtendedPrefix(tt.in))
		})
	}
}
