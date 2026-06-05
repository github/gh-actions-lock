package main

import (
	"testing"

	"github.com/github/gh-actions-pin/internal/doctor"
	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/runlog"
)

// TestBuildProvenanceReport_RecordsObservedSHA verifies that a finding carrying a
// resolver ObservedSHA distinct from the pinned SHA surfaces on the run record.
// This makes MISLEADING_SHA / REF_MOVED claims falsifiable: a reader can
// compare what's pinned vs. what upstream resolves to right now without
// re-running the resolver.
func TestBuildProvenanceReport_RecordsObservedSHA(t *testing.T) {
	const pinnedSHA = "d746ffe35508b1917358783b479e04febd2b8f71"
	const observedSHA = "3a2844b7e9c422d3c10d287c895573f7108da1b3"

	dep := &lockfile.Dependency{
		NWO: "actions/github-script",
		Ref: pinnedSHA,
		SHA: pinnedSHA,
	}
	finding := doctor.Finding{
		WorkflowPath: ".github/workflows/ci.yml",
		Category:     doctor.Category("misleading_sha"),
		Severity:     doctor.SeverityWarning,
		Confidence:   doctor.ConfidenceHigh,
		Dependency:   dep,
		ObservedSHA:  observedSHA,
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
	if a.ObservedSHA != observedSHA {
		t.Errorf("ObservedSHA: got %q, want %q (resolver output)", a.ObservedSHA, observedSHA)
	}
}

// TestBuildProvenanceReport_OmitsObservedSHAWhenEqual verifies the run record does
// not duplicate the SHA into ObservedSHA when the resolver and the pin agree.
func TestBuildProvenanceReport_OmitsObservedSHAWhenEqual(t *testing.T) {
	const sha = "3a2844b7e9c422d3c10d287c895573f7108da1b3"
	dep := &lockfile.Dependency{NWO: "actions/github-script", Ref: "v9.0.0", SHA: sha}
	finding := doctor.Finding{
		WorkflowPath: ".github/workflows/ci.yml",
		Category:     doctor.Category("stale"),
		Severity:     doctor.SeverityInfo,
		Confidence:   doctor.ConfidenceHigh,
		Dependency:   dep,
		ObservedSHA:  sha, // matches pinned SHA — nothing to flag
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
	if got := rep.Actions[0].ObservedSHA; got != "" {
		t.Errorf("ObservedSHA: got %q, want empty (matches pinned SHA)", got)
	}
}

var _ = runlog.Action{} // keep runlog imported for future field references

// TestBuildProvenanceReport_RecordsObservedSHA_AllDivergenceCategories pins down the
// invariant that motivates ObservedSHA: for every finding category where the
// resolver's live SHA is the falsifiability evidence — MISLEADING_SHA,
// REF_MOVED, LOCKFILE_FORGERY — buildProvenanceReport must surface it on the
// run record. Without this, claims like "the lockfile is forged" are
// unverifiable from the run record alone; a reader would have to re-run the
// resolver to compare. omitempty stays correct because most actions in a run
// don't diverge (already-pinned, valid); the contract is "populated whenever
// the divergence categories fire", enforced here.
func TestBuildProvenanceReport_RecordsObservedSHA_AllDivergenceCategories(t *testing.T) {
	const pinnedSHA = "11bd71901bbe5b1630ceea73d27597364c9af683"
	const observedSHA = "8e8c483db84b4bee98b60c0593521ed34d9990e8"
	// MISLEADING_SHA's pinned SHA is the SHA-shaped ref itself.
	const misleadingPinned = "d746ffe35508b1917358783b479e04febd2b8f71"
	const misleadingLive = "3a2844b7e9c422d3c10d287c895573f7108da1b3"

	cases := []struct {
		name        string
		category    doctor.Category
		pinnedSHA   string
		observedSHA string
		ref         string
	}{
		{
			name:        "ref_moved",
			category:    doctor.CategoryRefMoved,
			pinnedSHA:   pinnedSHA,
			observedSHA: observedSHA,
			ref:         "v4",
		},
		{
			name:        "lockfile_forgery",
			category:    doctor.CategoryLockfileForgery,
			pinnedSHA:   pinnedSHA,
			observedSHA: observedSHA,
			ref:         "v4",
		},
		{
			name:        "misleading_sha",
			category:    doctor.CategoryMisleadingSHA,
			pinnedSHA:   misleadingPinned,
			observedSHA: misleadingLive,
			ref:         misleadingPinned, // ref looks like a SHA
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dep := &lockfile.Dependency{
				NWO: "actions/checkout",
				Ref: tc.ref,
				SHA: tc.pinnedSHA,
			}
			finding := doctor.Finding{
				WorkflowPath: ".github/workflows/ci.yml",
				Category:     tc.category,
				Severity:     doctor.SeverityError,
				Confidence:   doctor.ConfidenceHigh,
				Dependency:   dep,
				ObservedSHA:  tc.observedSHA,
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
			if a.SHA != tc.pinnedSHA {
				t.Errorf("SHA: got %q, want %q (pinned)", a.SHA, tc.pinnedSHA)
			}
			if a.ObservedSHA == "" {
				t.Fatalf("ObservedSHA empty for %s — claim is unfalsifiable from the run record", tc.category)
			}
			if a.ObservedSHA != tc.observedSHA {
				t.Errorf("ObservedSHA: got %q, want %q (resolver output)", a.ObservedSHA, tc.observedSHA)
			}
			if a.Issue != string(tc.category) {
				t.Errorf("Issue: got %q, want %q", a.Issue, tc.category)
			}
		})
	}
}
