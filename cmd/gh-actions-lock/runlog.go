package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/github/gh-actions-lock/cmd/gh-actions-lock/format"
	"github.com/github/gh-actions-lock/internal/pin"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
)

const (
	runLogRetentionAge   = 14 * 24 * time.Hour
	runLogRetentionCount = 50
)

// writeRunLog saves the full --json output for this run under the user
// cache dir so a run that can't be reproduced later still leaves evidence
// for a bug report. Best effort: returns "" on any failure.
func writeRunLog(dir string, report *checks.Report, record *pin.Record, valid bool, lockfileVersion, homeHost string) string {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	gcLogs(dir)

	path := filepath.Join(dir, fmt.Sprintf("run-%s.json", time.Now().Format("20060102-150405.000")))
	f, err := os.Create(path)
	if err != nil {
		return ""
	}
	werr := format.WriteJSON(f, report, record, valid, format.AllJSONFields, cliVersion(), lockfileVersion, homeHost)
	if cerr := f.Close(); werr != nil || cerr != nil {
		_ = os.Remove(path)
		return ""
	}
	return path
}

func runLogDir() string {
	base, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(base, "gh-actions-lock", "logs")
}

func gcLogs(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type logFile struct {
		path    string
		modTime time.Time
	}
	var files []logFile
	cutoff := time.Now().Add(-runLogRetentionAge)
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
	if len(files) <= runLogRetentionCount {
		return
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].modTime.After(files[j].modTime)
	})
	for _, f := range files[runLogRetentionCount:] {
		_ = os.Remove(f.path)
	}
}
