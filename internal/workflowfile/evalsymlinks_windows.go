package workflowfile

import (
	"io/fs"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

// evalSymlinks resolves path through symlinks, junctions and volume mount
// points. filepath.EvalSymlinks can't: since Go 1.23 it treats the latter two
// as non-directories, so it fails with ERROR_PATH_NOT_FOUND below one and
// returns a trailing one unresolved (golang/go#61893).
func evalSymlinks(path string) (string, error) {
	long, err := extendedPath(path)
	if err != nil {
		return "", &fs.PathError{Op: "evalsymlinks", Path: path, Err: err}
	}
	p, err := windows.UTF16PtrFromString(long)
	if err != nil {
		return "", &fs.PathError{Op: "evalsymlinks", Path: path, Err: err}
	}
	// Zero access still lets us query the final path, and
	// FILE_FLAG_BACKUP_SEMANTICS is required to open a directory.
	h, err := windows.CreateFile(p, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", &fs.PathError{Op: "evalsymlinks", Path: path, Err: err}
	}
	defer windows.CloseHandle(h)

	final, err := finalPath(h)
	if err != nil {
		return "", &fs.PathError{Op: "evalsymlinks", Path: path, Err: err}
	}
	return trimExtendedPrefix(final), nil
}

// extendedPath returns path as an absolute `\\?\` path once it is too long for
// CreateFile without long-path opt-in, as os.Open does. 248 is Go's cutoff for
// directories, which must leave room for an 8.3 file name.
func extendedPath(path string) (string, error) {
	if strings.HasPrefix(path, `\\?\`) || strings.HasPrefix(path, `\\.\`) {
		return path, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	switch {
	case len(abs) < 248:
		return abs, nil
	case strings.HasPrefix(abs, `\\`):
		return `\\?\UNC\` + abs[2:], nil
	}
	return `\\?\` + abs, nil
}

func finalPath(h windows.Handle) (string, error) {
	buf := make([]uint16, windows.MAX_PATH)
	for {
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 0)
		if err != nil {
			return "", err
		}
		if int(n) < len(buf) {
			return syscall.UTF16ToString(buf[:n]), nil
		}
		// Too small: n is the required size including the terminating NUL.
		buf = make([]uint16, n)
	}
}

// trimExtendedPrefix turns `\\?\C:\x` into `C:\x` and `\\?\UNC\h\s` into
// `\\h\s`.
func trimExtendedPrefix(p string) string {
	switch {
	case strings.HasPrefix(p, `\\?\UNC\`):
		return `\` + p[len(`\\?\UNC`):]
	case len(p) >= 6 && strings.HasPrefix(p, `\\?\`) && p[5] == ':':
		return p[len(`\\?\`):]
	}
	return p
}
