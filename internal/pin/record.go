// Package pin implements the two-phase pin lifecycle: Plan builds a
// complete Record of what to pin (pure computation + network reads),
// and Commit writes the Record to disk (workflow files + lockfile).
package pin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const (
	schemaVersion  = "run-record/v1"
	toolName       = "gh-actions-pin"
	retentionAge   = 14 * 24 * time.Hour
	retentionCount = 50
)

// RepoInfo identifies the repository the run scanned.
type RepoInfo struct {
	Owner string `json:"owner,omitempty"`
	Name  string `json:"name,omitempty"`
	Host  string `json:"host,omitempty"`
}

// Entry records the plan decision for one action dependency.
type Entry struct {
	NWO          string     `json:"nwo"`
	Ref          string     `json:"ref"`
	SHA          string     `json:"sha,omitempty"`
	ObservedSHA  string     `json:"observed_sha,omitempty"`
	Resolution   Resolution `json:"resolution"`
	Issue        string     `json:"issue,omitempty"`
	Reason       string     `json:"reason,omitempty"`
	Suggestion   string     `json:"suggestion,omitempty"`
	AutoFixedRef string     `json:"auto_fixed_ref,omitempty"` // original ref before sane-release rewrite
	OnBranch     string     `json:"on_branch,omitempty"`
	Tag          string     `json:"tag,omitempty"`
	Workflows    []string   `json:"workflows"`
	RequiredBy   []string   `json:"required_by,omitempty"`
	Direct       bool       `json:"direct"`
	FullScan     bool       `json:"full_scan,omitempty"`
}

// WorkflowPlan records what Commit must write for one workflow file.
// Internal to the pin lifecycle; not serialized.
type WorkflowPlan struct {
	Path     string
	Rewrites map[string]string
}

// Record is the complete output of Plan — everything Commit needs to
// write all changes atomically, and the authoritative run-log artifact.
type Record struct {
	Entries   []Entry
	Workflows []WorkflowPlan // internal, omitted from JSON
	Repo      *RepoInfo
	Version   string
	Created   time.Time
}

// Summary is the run's roll-up: counts by resolution.
type Summary struct {
	Workflows     int  `json:"workflows"`
	Actions       int  `json:"actions"`
	Valid         bool `json:"valid"`
	Pinned        int  `json:"pinned"`
	AlreadyPinned int  `json:"already_pinned"`
	FullScan      int  `json:"full_scan"`
	Investigate   int  `json:"needs_investigation"`
	Skipped       int  `json:"skipped"`
	Unresolved    int  `json:"unresolved"`
}

// Pinned returns entries with Resolution == Pinned.
func (r *Record) Pinned() []Entry {
	return r.byResolution(Pinned)
}

// Investigated returns entries with Resolution == Investigate.
func (r *Record) Investigated() []Entry {
	return r.byResolution(Investigate)
}

// Unresolved returns entries with Resolution == Unresolved.
func (r *Record) Unresolved() []Entry {
	return r.byResolution(Unresolved)
}

// Valid reports whether the record contains no investigate or unresolved entries.
func (r *Record) Valid() bool {
	for _, e := range r.Entries {
		if e.Resolution == Investigate || e.Resolution == Unresolved {
			return false
		}
	}
	return true
}

func (r *Record) byResolution(res Resolution) []Entry {
	var out []Entry
	for _, e := range r.Entries {
		if e.Resolution == res {
			out = append(out, e)
		}
	}
	return out
}

// summary computes roll-up counts from the deduplicated action list.
func (r *Record) summary(actions []Entry) Summary {
	workflows := map[string]bool{}
	for _, e := range r.Entries {
		for _, wf := range e.Workflows {
			workflows[wf] = true
		}
	}
	s := Summary{
		Workflows: len(workflows),
		Actions:   len(actions),
		Valid:     r.Valid(),
	}
	for _, e := range actions {
		switch e.Resolution {
		case Pinned:
			s.Pinned++
		case Verified:
			s.AlreadyPinned++
		case Investigate:
			s.Investigate++
		case Skipped:
			s.Skipped++
		case Unresolved:
			s.Unresolved++
		}
		if e.FullScan {
			s.FullScan++
		}
	}
	return s
}

// dedupActions merges entries that share NWO@Ref, combining workflow lists.
func dedupActions(entries []Entry) []Entry {
	type slot struct {
		idx int
	}
	seen := map[string]*slot{}
	var out []Entry
	for _, e := range entries {
		key := e.NWO + "@" + e.Ref
		if s, ok := seen[key]; ok {
			out[s.idx].Workflows = appendUnique(out[s.idx].Workflows, e.Workflows...)
			continue
		}
		seen[key] = &slot{idx: len(out)}
		out = append(out, e)
	}
	return out
}

// MarshalJSON produces the run-log JSON with schema, tool info, summary,
// and deduplicated action entries.
func (r *Record) MarshalJSON() ([]byte, error) {
	deduped := dedupActions(r.Entries)
	type wire struct {
		Schema    string    `json:"schema"`
		Generated string    `json:"generated_at"`
		Tool      toolInfo  `json:"tool"`
		Repo      *RepoInfo `json:"repo,omitempty"`
		Summary   Summary   `json:"summary"`
		Actions   []Entry   `json:"actions"`
	}
	return json.Marshal(wire{
		Schema:    schemaVersion,
		Generated: r.Created.UTC().Format(time.RFC3339),
		Tool:      toolInfo{Name: toolName, Version: r.Version},
		Repo:      r.Repo,
		Summary:   r.summary(deduped),
		Actions:   deduped,
	})
}

type toolInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// WriteJSON writes the record as indented JSON to the cache directory,
// garbage-collecting old logs. Returns the file path.
func (r *Record) WriteJSON() (string, error) {
	dir, err := logDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	gcLogs(dir)

	name := fmt.Sprintf("run-%s.json", r.Created.Format("20060102-150405.000"))
	path := filepath.Join(dir, name)

	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", err
	}
	b = append(b, '\n')
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func logDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "gh-actions-pin", "logs"), nil
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
	sort.Slice(files, func(i, j int) bool {
		return files[i].modTime.After(files[j].modTime)
	})
	for _, f := range files[retentionCount:] {
		_ = os.Remove(f.path)
	}
}

func appendUnique(base []string, items ...string) []string {
	have := map[string]bool{}
	for _, s := range base {
		have[s] = true
	}
	for _, s := range items {
		if !have[s] {
			base = append(base, s)
			have[s] = true
		}
	}
	return base
}
