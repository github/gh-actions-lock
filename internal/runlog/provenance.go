package runlog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// SchemaVersion identifies the provenance document shape so downstream tools
// can detect breaking changes.
const SchemaVersion = "gh-actions-pin/provenance/v1"

// Resolution classifies what was done to an action during a run.
const (
	ResolutionPinned        = "pinned"              // newly locked (or re-locked) to a commit SHA
	ResolutionAlreadyPinned = "already-pinned"      // already locked and still valid; no change
	ResolutionInvestigate   = "needs-investigation" // security gate tripped; left for a human
	ResolutionSkipped       = "skipped"             // user (or non-interactive mode) declined to fix
	ResolutionUnresolved    = "unresolved"          // ref could not be resolved to a SHA
)

// Report is the structured, action-centric provenance document written at the
// end of a run. It records what was resolved and how, deduplicating actions
// (one entry per unique action@ref) and listing the workflows that reference
// each one — so nothing is repeated per workflow.
type Report struct {
	Schema      string    `json:"schema"`
	GeneratedAt string    `json:"generated_at"`
	Tool        ToolInfo  `json:"tool"`
	Repo        *RepoInfo `json:"repo,omitempty"`
	Summary     Summary   `json:"summary"`
	Actions     []Action  `json:"actions"`
}

// ToolInfo identifies the tool and version that produced the report.
type ToolInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// RepoInfo identifies the repository the run scanned.
type RepoInfo struct {
	Owner string `json:"owner,omitempty"`
	Name  string `json:"name,omitempty"`
	Host  string `json:"host,omitempty"`
}

// Summary is the run's roll-up: counts by resolution plus overall validity.
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

// Action is a single deduplicated action dependency and the provenance of how
// it was resolved during the run.
type Action struct {
	NWO      string `json:"nwo"`
	Ref      string `json:"ref"`
	SHA      string `json:"sha,omitempty"`
	HashAlgo string `json:"hash_algo,omitempty"`
	// ObservedSHA is the SHA the resolver got when it looked up Ref during
	// this run. Recorded when it differs from SHA (the pinned value) — e.g.
	// MISLEADING_SHA, REF_MOVED, LOCKFILE_FORGERY. Makes a finding
	// falsifiable: a reader can compare what was pinned against what
	// upstream actually resolved to at scan time, without re-running the
	// resolver. Empty when the run did not surface a divergence.
	ObservedSHA string `json:"observed_sha,omitempty"`
	Direct      bool   `json:"direct"`
	Resolution  string `json:"resolution"`
	// How is concise, human-readable provenance ("locked ref v4 to <sha>",
	// "verified via full branch scan", a security reason, etc.).
	How string `json:"how,omitempty"`
	// Issue is the originating finding category (e.g. MISSING, REF_MOVED) when
	// the action needed work; empty when it was already valid.
	Issue string `json:"issue,omitempty"`
	// CanonicalBranch reports whether the commit was found on a canonical
	// branch. nil when reachability wasn't decided (e.g. unresolved actions).
	CanonicalBranch *bool `json:"canonical_branch,omitempty"`
	// Workflows lists every workflow file that references this action, so the
	// action is recorded once rather than repeated per workflow.
	Workflows []string `json:"workflows"`
	// RequiredBy lists parent composite actions for transitive dependencies.
	RequiredBy []string `json:"required_by,omitempty"`
}

// WriteReport garbage-collects old logs, writes r as a pretty-printed JSON
// document to a timestamped file in the log directory, and returns its path.
func WriteReport(r *Report) (string, error) {
	if r.Schema == "" {
		r.Schema = SchemaVersion
	}
	if r.GeneratedAt == "" {
		r.GeneratedAt = time.Now().Format(time.RFC3339Nano)
	}

	dir, err := logDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	gc(dir)

	name := fmt.Sprintf("run-%s.json", time.Now().Format("20060102-150405.000"))
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
