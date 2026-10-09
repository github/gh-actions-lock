package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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
// for a bug report. repo ("host/owner/name", empty if unknown) is added
// only here, not to --json. Best effort: returns "" on any failure.
func writeRunLog(dir string, report *checks.Report, record *pin.Record, valid bool, lockfileVersion, homeHost, repo string) string {
	if dir == "" {
		return ""
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	// CreateTemp opens with 0600: logs can name private repos.
	f, err := os.CreateTemp(dir, fmt.Sprintf("run-%s-*.json", time.Now().Format("20060102-150405")))
	if err != nil {
		return ""
	}
	path := f.Name()
	werr := writeRunLogJSON(f, report, record, valid, lockfileVersion, homeHost, repo)
	if cerr := f.Close(); werr != nil || cerr != nil {
		_ = os.Remove(path)
		return ""
	}
	gcLogs(dir)
	return path
}

func writeRunLogJSON(w io.Writer, report *checks.Report, record *pin.Record, valid bool, lockfileVersion, homeHost, repo string) error {
	var buf bytes.Buffer
	if err := format.WriteJSON(&buf, report, record, valid, format.AllJSONFields, cliVersion(), lockfileVersion, homeHost); err != nil {
		return err
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(buf.Bytes(), &payload); err != nil {
		return err
	}
	if repo != "" {
		payload["repo"], _ = json.Marshal(repo)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(payload)
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
