package main

import (
	"slices"
	"testing"

	"github.com/github/gh-actions-pin/internal/doctor"
	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/runlog"
)

// TestBuildProvenanceReport_RecordsObservedSHA verifies that a finding carrying a
// resolver ObservedSHA distinct from the pinned SHA surfaces on the run record.
// This makes misleading-sha / ref-moved claims falsifiable: a reader can
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
		Category:     doctor.Category("misleading-sha"),
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
	out := newProvenanceOutcomes(nil, nil, nil, nil, nil, nil, nil)

	rep := buildProvenanceReport(report, store, false, nil, out, nil)

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
	out := newProvenanceOutcomes(nil, nil, nil, nil, nil, nil, nil)

	rep := buildProvenanceReport(report, store, true, nil, out, nil)

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
// resolver's live SHA is the falsifiability evidence — misleading-sha,
// ref-moved, lockfile-forgery — buildProvenanceReport must surface it on the
// run record. Without this, claims like "the lockfile is forged" are
// unverifiable from the run record alone; a reader would have to re-run the
// resolver to compare. omitempty stays correct because most actions in a run
// don't diverge (already-pinned, valid); the contract is "populated whenever
// the divergence categories fire", enforced here.
func TestBuildProvenanceReport_RecordsObservedSHA_AllDivergenceCategories(t *testing.T) {
	const pinnedSHA = "11bd71901bbe5b1630ceea73d27597364c9af683"
	const observedSHA = "8e8c483db84b4bee98b60c0593521ed34d9990e8"
	// misleading-sha's pinned SHA is the SHA-shaped ref itself.
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
			name:        "ref-moved",
			category:    doctor.CategoryRefMoved,
			pinnedSHA:   pinnedSHA,
			observedSHA: observedSHA,
			ref:         "v4",
		},
		{
			name:        "lockfile-forgery",
			category:    doctor.CategoryLockfileForgery,
			pinnedSHA:   pinnedSHA,
			observedSHA: observedSHA,
			ref:         "v4",
		},
		{
			name:        "misleading-sha",
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
			out := newProvenanceOutcomes(nil, nil, nil, nil, nil, nil, nil)

			rep := buildProvenanceReport(report, store, false, nil, out, nil)

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

// TestBuildProvenanceReport_RecordsAutoFixed verifies that AutoFixedImpostor
// records flow through to the runlog.AutoFixed array. Downstream consumers
// (Dependabot, audit tooling) read this to confirm what the run rewrote.
func TestBuildProvenanceReport_RecordsAutoFixed(t *testing.T) {
	report := &doctor.Report{
		Workflows: []doctor.WorkflowReport{{Path: ".github/workflows/ci.yml"}},
	}
	store, err := lockfile.OpenStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	out := newProvenanceOutcomes(nil, nil, nil, nil, nil, nil, nil)

	fixes := []doctor.AutoFixedImpostor{{
		Workflow: ".github/workflows/ci.yml",
		NWO:      "evil/action",
		OldRef:   "v1",
		OldSHA:   "ffffffffffffffffffffffffffffffffffffffff",
		NewTag:   "v3.0.3",
		NewSHA:   "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}}

	rep := buildProvenanceReport(report, store, true, nil, out, fixes)

	if len(rep.AutoFixed) != 1 {
		t.Fatalf("expected 1 auto-fix entry, got %d", len(rep.AutoFixed))
	}
	af := rep.AutoFixed[0]
	want := runlog.AutoFix{
		Workflow: ".github/workflows/ci.yml",
		NWO:      "evil/action",
		FromRef:  "v1",
		FromSHA:  "ffffffffffffffffffffffffffffffffffffffff",
		ToRef:    "v3.0.3",
		ToSHA:    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Reason:   "impostor pin rewritten to reachable release",
	}
	if af != want {
		t.Errorf("AutoFixed[0] = %+v, want %+v", af, want)
	}
}

// TestBuildProvenanceReport_SynthesizesAlertedActions locks the
// security-critical synthesis invariant: an alerted dependency key that never
// appears in any workflow inventory or finding (it reaches buildProvenanceReport
// only through the remediator's alerted slice) must still yield a renderable
// Action. The terminal investigation block is a pure walk of prov.Actions, so a
// missing synthesized row would silently drop a security alert. Each case
// asserts the synthesized action carries the supplied reason and workflows,
// splits NWO@Ref correctly (sub-action paths included), and sets Escalate only
// for the publisher-side off-branch reason.
func TestBuildProvenanceReport_SynthesizesAlertedActions(t *testing.T) {
	cases := []struct {
		name         string
		key          string
		reason       string
		workflows    []string
		wantNWO      string
		wantRef      string
		wantEscalate bool
	}{
		{
			name:         "off-branch impostor escalates",
			key:          "mmastrac/mmm-matrix@v1.0.5",
			reason:       doctor.ReasonImpostorOffBranch,
			workflows:    []string{".github/workflows/build_and_deploy.yml"},
			wantNWO:      "mmastrac/mmm-matrix",
			wantRef:      "v1.0.5",
			wantEscalate: true,
		},
		{
			name:         "lockfile forgery does not escalate",
			key:          "evil/action@v2",
			reason:       doctor.ReasonLockfileForgery,
			workflows:    []string{".github/workflows/ci.yml", ".github/workflows/release.yml"},
			wantNWO:      "evil/action",
			wantRef:      "v2",
			wantEscalate: false,
		},
		{
			name:         "misleading sha does not escalate",
			key:          "deceptive/dep@v1",
			reason:       doctor.ReasonMisleadingSHA,
			workflows:    []string{".github/workflows/ci.yml"},
			wantNWO:      "deceptive/dep",
			wantRef:      "v1",
			wantEscalate: false,
		},
		{
			name:         "sub-action path splits on last @",
			key:          "actions/cache/save@v4",
			reason:       doctor.ReasonImpostorOffBranch,
			workflows:    []string{".github/workflows/cache.yml"},
			wantNWO:      "actions/cache/save",
			wantRef:      "v4",
			wantEscalate: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// An empty report guarantees the alerted key is absent from every
			// inventory and finding, so the only way an Action can exist is the
			// synthesis pass.
			report := &doctor.Report{}
			store, err := lockfile.OpenStore(t.TempDir(), nil)
			if err != nil {
				t.Fatalf("OpenStore: %v", err)
			}
			out := newProvenanceOutcomes(
				[]string{tc.key}, nil, nil, nil,
				map[string]string{tc.key: tc.reason},
				nil,
				map[string][]string{tc.key: tc.workflows},
			)

			rep := buildProvenanceReport(report, store, false, nil, out, nil)

			if len(rep.Actions) != 1 {
				t.Fatalf("expected 1 synthesized action, got %d", len(rep.Actions))
			}
			a := rep.Actions[0]
			if a.Resolution != runlog.ResolutionInvestigate {
				t.Errorf("Resolution: got %q, want %q", a.Resolution, runlog.ResolutionInvestigate)
			}
			if a.NWO != tc.wantNWO {
				t.Errorf("NWO: got %q, want %q", a.NWO, tc.wantNWO)
			}
			if a.Ref != tc.wantRef {
				t.Errorf("Ref: got %q, want %q", a.Ref, tc.wantRef)
			}
			if a.Reason != tc.reason {
				t.Errorf("Reason: got %q, want %q", a.Reason, tc.reason)
			}
			if !slices.Equal(a.Workflows, tc.workflows) {
				t.Errorf("Workflows: got %v, want %v", a.Workflows, tc.workflows)
			}
			if a.Escalate != tc.wantEscalate {
				t.Errorf("Escalate: got %v, want %v (reason %q)", a.Escalate, tc.wantEscalate, tc.reason)
			}
			if rep.Summary.Investigate != 1 {
				t.Errorf("Summary.Investigate: got %d, want 1", rep.Summary.Investigate)
			}
		})
	}
}

// TestBuildProvenanceReport_OmitsAutoFixedWhenEmpty verifies the AutoFixed
// array is omitted (nil) when nothing was auto-fixed — keeps the JSON
// surface clean for the steady-state case.
func TestBuildProvenanceReport_OmitsAutoFixedWhenEmpty(t *testing.T) {
	report := &doctor.Report{Workflows: nil}
	store, err := lockfile.OpenStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	out := newProvenanceOutcomes(nil, nil, nil, nil, nil, nil, nil)

	rep := buildProvenanceReport(report, store, true, nil, out, nil)

	if rep.AutoFixed != nil {
		t.Errorf("expected AutoFixed to be nil (omitempty), got %+v", rep.AutoFixed)
	}
}
