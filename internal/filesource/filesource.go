// Package filesource abstracts where repository content is read from, so the
// scanning pipeline can run against a local checkout or a remote repository
// pinned to a commit.
package filesource

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

)

// Source reads files relative to a repository root. Paths use forward
// slashes. Missing paths wrap fs.ErrNotExist.
type Source interface {
	ReadFile(ctx context.Context, name string) ([]byte, error)
	// ListFiles returns the base names of the regular files directly in dir,
	// sorted. Subdirectories are omitted.
	ListFiles(ctx context.Context, dir string) ([]string, error)
}

// ErrEscapesRoot is returned for paths that would leave the repository.
var ErrEscapesRoot = errors.New("path escapes repository root")

// clean normalizes name to a rooted-relative slash path and rejects
// anything that would leave the repository.
func clean(name string) (string, error) {
	name = strings.ReplaceAll(name, `\`, "/")
	if path.IsAbs(name) {
		return "", fmt.Errorf("%s: %w", name, ErrEscapesRoot)
	}
	c := path.Clean(name)
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("%s: %w", name, ErrEscapesRoot)
	}
	return c, nil
}

// Local reads from a directory on disk.
type Local struct {
	Root string
}

// resolve joins name under Root and refuses symlinks that point outside it.
func (l Local) resolve(name string) (string, error) {
	c, err := clean(name)
	if err != nil {
		return "", err
	}
	full := filepath.Join(l.Root, filepath.FromSlash(c))
	root, err := filepath.EvalSymlinks(l.Root)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(full)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s: %w", name, ErrEscapesRoot)
	}
	return full, nil
}

// ReadFile implements Source.
func (l Local) ReadFile(_ context.Context, name string) ([]byte, error) {
	full, err := l.resolve(name)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(full)
}

// ListFiles implements Source.
func (l Local) ListFiles(_ context.Context, dir string) ([]string, error) {
	full, err := l.resolve(dir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.Type().IsRegular() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// Entry is one item of a directory listing.
type Entry struct {
	Name  string
	Path  string
	IsDir bool
}

// Reader is the slice of the GitHub API client a Remote needs.
type Reader interface {
	ReadFile(ctx context.Context, owner, repo, ref, path string) ([]byte, error)
	ListDir(ctx context.Context, owner, repo, ref, dir string) ([]Entry, error)
}

// Remote reads from a repository at a fixed commit. Pinning every read to
// SHA keeps one verification a consistent snapshot.
type Remote struct {
	Reader Reader
	Owner  string
	Repo   string
	SHA    string
}

// ReadFile implements Source.
func (r Remote) ReadFile(ctx context.Context, name string) ([]byte, error) {
	c, err := clean(name)
	if err != nil {
		return nil, err
	}
	return r.Reader.ReadFile(ctx, r.Owner, r.Repo, r.SHA, c)
}

// ListFiles implements Source.
func (r Remote) ListFiles(ctx context.Context, dir string) ([]string, error) {
	c, err := clean(dir)
	if err != nil {
		return nil, err
	}
	entries, err := r.Reader.ListDir(ctx, r.Owner, r.Repo, r.SHA, c)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir {
			names = append(names, e.Name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// IsNotExist reports whether err means a path is absent.
func IsNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }
