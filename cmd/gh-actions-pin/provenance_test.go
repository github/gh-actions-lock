package main

import (
	"testing"

	"github.com/github/gh-actions-pin/internal/doctor"
	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/runlog"
)

// TestBuildProvenanceReport_RecordsLiveSHA verifies that a finding carrying a
// resolver LiveSHA distinct from the pinned SHA surfaces on the run record.
// This makes MISLEADING_SHA / REF_MOVED claims falsifiable: a reader can
// compare what's pinned vs. what upstream resolves to right now without
// re-running the resolver.
func TestBuildProvenanceReport_RecordsLiveSHA(t *testing.T) {
	const pinnedSHA = "d746ffe35508b1917358783b479e04febd2b8f71"
	const liveSHA = "3a2844b7e9c422d3c10d287c895573f7108da1b3"

	dep := &lockfile.Dependency{
		NWO: "actions/github-script",
		Ref: pinnedSHA,
		SHA: pinnedSHA,
	}
	finding := doctor.Finding{
		WorkflowPath: ".github/workflows/ci.yml",
		Category:     doctor.Category("misleading_sha"),
		Severity:     doctor.SeverityWarning,
		Dependency:   dep,
		LiveSHA:      liveSHA,
	}
	report := &doctor.Report{
		Workflows: []doctor.WorkflowReport{{
			Path:     ".github/workflows/ci.yml",
			Findings: []doctor.Finding{finding},
		}},
	}
	store, err := lockfile.OpenStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	out := newProvenanceOutcomes(nil, nil, nil, nil, nil)

	rep := buildProvenanceReport(report, store, false, nil, out)

	if len(rep.Actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(rep.Actions))
	}
	a := rep.Actions[0]
	if a.SHA != pinnedSHA {
		t.Errorf("SHA: got %q, want %q (pinned ref)", a.SHA, pinnedSHA)
	}
	if a.LiveSHA != liveSHA {
		t.Errorf("LiveSHA: got %q, want %q (resolver output)", a.LiveSHA, liveSHA)
	}
}

// TestBuildProvenanceReport_OmitsLiveSHAWhenEqual verifies the run record does
// not duplicate the SHA into LiveSHA when the resolver and the pin agree.
func TestBuildProvenanceReport_OmitsLiveSHAWhenEqual(t *testing.T) {
	const sha = "3a2844b7e9c422d3c10d287c895573f7108da1b3"
	dep := &lockfile.Dependency{NWO: "actions/github-script", Ref: "v9.0.0", SHA: sha}
	finding := doctor.Finding{
		WorkflowPath: ".github/workflows/ci.yml",
		Category:     doctor.Category("stale"),
		Severity:     doctor.SeverityInfo,
		Dependency:   dep,
		LiveSHA:      sha, // matches pinned SHA — nothing to flag
	}
	report := &doctor.Report{
		Workflows: []doctor.WorkflowReport{{
			Path:     ".github/workflows/ci.yml",
			Findings: []doctor.Finding{finding},
		}},
	}
	store, err := lockfile.OpenStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	out := newProvenanceOutcomes(nil, nil, nil, nil, nil)

	rep := buildProvenanceReport(report, store, true, nil, out)

	if len(rep.Actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(rep.Actions))
	}
	if got := rep.Actions[0].LiveSHA; got != "" {
		t.Errorf("LiveSHA: got %q, want empty (matches pinned SHA)", got)
	}
}

var _ = runlog.Action{} // keep runlog imported for future field references
