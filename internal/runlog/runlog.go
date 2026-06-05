// Package runlog manages the on-disk transcript of a gh-actions-pin run.
//
// Detailed narration goes to a plain-text log under os.UserCacheDir, keeping
// interactive output limited to spinners, prompts, and the final summary. Old
// logs are garbage-collected on open so the directory stays bounded.
package runlog

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	// retention bounds the log directory by age and count so it never grows
	// without limit. Both limits are applied on every Open.
	retentionAge   = 14 * 24 * time.Hour
	retentionCount = 50
)

// Logger is an io.Writer-backed transcript file for a single run. Safe for
// concurrent use by multiple goroutines; Write serializes via mu.
type Logger struct {
	mu   sync.Mutex
	f    *os.File
	path string
}

// Open creates a new timestamped log file, garbage-collects stale logs, and
// returns a Logger ready to receive narration. The caller must Close it.
func Open() (*Logger, error) {
	dir, err := logDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	gc(dir)

	name := fmt.Sprintf("run-%s.log", time.Now().Format("20060102-150405.000"))
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}

	l := &Logger{f: f, path: path}
	fmt.Fprintf(f, "# gh-actions-pin run %s\n\n", time.Now().Format(time.RFC3339))
	return l, nil
}

// Write implements io.Writer so the UI can stream narration directly.
func (l *Logger) Write(p []byte) (int, error) {
	if l == nil || l.f == nil {
		return len(p), nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Write(p)
}

// Path returns the absolute path of the log file.
func (l *Logger) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// Close flushes and closes the underlying file.
func (l *Logger) Close() error {
	if l == nil || l.f == nil {
		return nil
	}
	return l.f.Close()
}

// logDir returns the directory where run logs are stored.
func logDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "gh-actions-pin", "logs"), nil
}

// gc removes logs older than retentionAge and, beyond that, keeps only the
// most recent retentionCount files. Failures are ignored — GC is best-effort.
func gc(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	type logFile struct {
		path    string
		modTime time.Time
	}
	var files []logFile
	cutoff := time.Now().Add(-retentionAge)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(path)
			continue
		}
		files = append(files, logFile{path: path, modTime: info.ModTime()})
	}

	if len(files) <= retentionCount {
		return
	}
	// Newest first; remove everything past the retention count.
	sort.Slice(files, func(i, j int) bool {
		return files[i].modTime.After(files[j].modTime)
	})
	for _, f := range files[retentionCount:] {
		_ = os.Remove(f.path)
	}
}
