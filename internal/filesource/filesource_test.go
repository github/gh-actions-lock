package filesource

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClean(t *testing.T) {
	tests := []struct {
		in    string
		want  string
		isErr bool
	}{
		{in: ".github/workflows/ci.yml", want: ".github/workflows/ci.yml"},
		{in: "./a/../b.yml", want: "b.yml"},
		{in: `a\b.yml`, want: "a/b.yml"},
		{in: "../x", isErr: true},
		{in: "a/../../x", isErr: true},
		{in: "/etc/passwd", isErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := clean(tt.in)
			if tt.isErr {
				require.ErrorIs(t, err, ErrEscapesRoot)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestLocal(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".github", "workflows", "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".github", "workflows", "b.yml"), []byte("b"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".github", "workflows", "a.yml"), []byte("a"), 0o644))
	src := Local{Root: root}
	ctx := context.Background()

	got, err := src.ReadFile(ctx, ".github/workflows/a.yml")
	require.NoError(t, err)
	assert.Equal(t, "a", string(got))

	names, err := src.ListFiles(ctx, ".github/workflows")
	require.NoError(t, err)
	assert.Equal(t, []string{"a.yml", "b.yml"}, names, "sorted, files only")

	_, err = src.ReadFile(ctx, "missing.yml")
	assert.True(t, IsNotExist(err))

	_, err = src.ReadFile(ctx, "../outside")
	assert.ErrorIs(t, err, ErrEscapesRoot)
}

type fakeReader struct {
	files   map[string]string
	dirs    map[string][]Entry
	gotRefs []string
}

func (f *fakeReader) ReadFile(_ context.Context, owner, repo, ref, path string) ([]byte, error) {
	f.gotRefs = append(f.gotRefs, owner+"/"+repo+"@"+ref)
	if s, ok := f.files[path]; ok {
		return []byte(s), nil
	}
	return nil, fs.ErrNotExist
}

func (f *fakeReader) ListDir(_ context.Context, owner, repo, ref, dir string) ([]Entry, error) {
	f.gotRefs = append(f.gotRefs, owner+"/"+repo+"@"+ref)
	if d, ok := f.dirs[dir]; ok {
		return d, nil
	}
	return nil, fs.ErrNotExist
}

func TestRemote(t *testing.T) {
	fr := &fakeReader{
		files: map[string]string{".github/workflows/ci.yml": "ci"},
		dirs: map[string][]Entry{".github/workflows": {
			{Name: "z.yml"}, {Name: "sub", IsDir: true}, {Name: "a.yml"},
		}},
	}
	src := Remote{Reader: fr, Owner: "octo", Repo: "api", SHA: "abc123"}
	ctx := context.Background()

	got, err := src.ReadFile(ctx, "./.github/workflows/ci.yml")
	require.NoError(t, err)
	assert.Equal(t, "ci", string(got))

	names, err := src.ListFiles(ctx, ".github/workflows")
	require.NoError(t, err)
	assert.Equal(t, []string{"a.yml", "z.yml"}, names)

	assert.Equal(t, []string{"octo/api@abc123", "octo/api@abc123"}, fr.gotRefs, "every read is pinned to the SHA")

	_, err = src.ReadFile(ctx, "nope.yml")
	assert.True(t, IsNotExist(err))

	_, err = src.ReadFile(ctx, "../secret")
	assert.ErrorIs(t, err, ErrEscapesRoot)
	_, err = src.ListFiles(ctx, "/abs")
	assert.ErrorIs(t, err, ErrEscapesRoot)
}
