package format

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/github/gh-actions-pin/internal/doctor"
	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/ui"
)

// newTestUI returns a UI whose narration log sinks to io.Discard, mirroring
// the production setup `check.go` installs for human-terminal runs. Term*
// methods bypass the log and write to the returned buffer; narration
// helpers (Success/Warning/Detail) go to the discard sink.
func newTestUI() (*ui.UI, *bytes.Buffer) {
	var buf bytes.Buffer
	u := ui.NewPlain(&buf)
	u.SetLog(io.Discard)
	return u, &buf
}

// TestPresentResults_WarningsReachTerminal locks parity between the
// human-terminal renderer and the JSON renderer for warning-severity
// findings.
func TestPresentResults_WarningsReachTerminal(t *testing.T) {
	tests := []struct {
		name       string
		findings   []doctor.Finding
		wantOutput []string
		notWanted  []string
	}{
		{
			name: "ref-moved warning surfaces with compare URL",
			findings: []doctor.Finding{{
				WorkflowPath: ".github/workflows/a.yml",
				Category:     doctor.CategoryRefMoved,
				Severity:     doctor.SeverityWarning,
				Confidence:   doctor.ConfidenceHigh,
				Dependency: &lockfile.Dependency{
					NWO: "octo/action",
					Ref: "v1",
					SHA: "1111111111111111111111111111111111111111",
				},
				ObservedSHA: "2222222222222222222222222222222222222222",
				Detail:      "ref v1 now resolves to 222222222222, lockfile pins 111111111111",
			}},
			wantOutput: []string{
				"moved upstream",
				"octo/action@v1",
				"compare/111111111111...222222222222",
			},
		},
		{
			name: "sha-as-ref direct warning surfaces",
			findings: []doctor.Finding{{
				WorkflowPath: ".github/workflows/a.yml",
				Category:     doctor.CategorySHAAsRef,
				Severity:     doctor.SeverityWarning,
				Confidence:   doctor.ConfidenceHigh,
				ActionRef:    &lockfile.ActionRef{Owner: "octo", Repo: "action", Ref: "abc123"},
				Dependency: &lockfile.Dependency{
					NWO: "octo/action",
					Ref: "abc123",
					SHA: "abc1230000000000000000000000000000000000",
				},
				Detail: "pinned to a bare SHA without a symbolic ref",
			}},
			wantOutput: []string{
				"pinned to a bare SHA",
			},
		},
		{
			name: "workflow-level not-pinned warning surfaces",
			findings: []doctor.Finding{{
				WorkflowPath: ".github/workflows/a.yml",
				Category:     doctor.CategoryNotPinned,
				Severity:     doctor.SeverityWarning,
				Confidence:   doctor.ConfidenceHigh,
				Detail:       "no lockfile entry",
			}},
			wantOutput: []string{
				"not yet pinned",
			},
		},
		{
			name: "reachability-unknown surfaces under the dep key",
			findings: []doctor.Finding{{
				WorkflowPath: ".github/workflows/a.yml",
				Category:     doctor.CategoryReachabilityUnknown,
				Severity:     doctor.SeverityWarning,
				Confidence:   doctor.ConfidenceLow,
				Dependency: &lockfile.Dependency{
					NWO: "octo/action",
					Ref: "v1",
					SHA: "1111111111111111111111111111111111111111",
				},
				Detail:      "branch list rate limited",
				Remediation: "retry later",
			}},
			wantOutput: []string{
				"octo/action@v1",
				"branch list rate limited",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, buf := newTestUI()
			report := &doctor.Report{
				Workflows: []doctor.WorkflowReport{{
					Path:     ".github/workflows/a.yml",
					Findings: tt.findings,
				}},
			}
			PresentResults(u, report, true, false)

			got := buf.String()
			for _, want := range tt.wantOutput {
				if !strings.Contains(got, want) {
					t.Errorf("PresentResults output missing %q\nfull output:\n%s", want, got)
				}
			}
			for _, unwanted := range tt.notWanted {
				if strings.Contains(got, unwanted) {
					t.Errorf("PresentResults output unexpectedly contains %q\nfull output:\n%s", unwanted, got)
				}
			}
		})
	}
}

// TestPresentResults_RemediateHints locks the willRemediate-aware "↳"
// follow-up lines that PresentResults emits under each warning headline.
// Categories the remediator auto-fixes (NotPinned, SHAAsRef) flip between
// "resolving below" and a manual "run X" hint; categories it skips
// (RefMoved, inconclusive) always show the manual hint.
func TestPresentResults_RemediateHints(t *testing.T) {
	tests := []struct {
		name          string
		willRemediate bool
		findings      []doctor.Finding
		wantOutput    []string
		notWanted     []string
	}{
		{
			name:          "not-pinned shows resolving-below when remediating",
			willRemediate: true,
			findings: []doctor.Finding{{
				WorkflowPath: ".github/workflows/a.yml",
				Category:     doctor.CategoryNotPinned,
				Severity:     doctor.SeverityWarning,
				Confidence:   doctor.ConfidenceHigh,
			}},
			wantOutput: []string{"not yet pinned", "↳ resolving below"},
			notWanted:  []string{"run `gh actions-pin`"},
		},
		{
			name:          "not-pinned shows manual hint when not remediating",
			willRemediate: false,
			findings: []doctor.Finding{{
				WorkflowPath: ".github/workflows/a.yml",
				Category:     doctor.CategoryNotPinned,
				Severity:     doctor.SeverityWarning,
				Confidence:   doctor.ConfidenceHigh,
			}},
			wantOutput: []string{"not yet pinned", "↳ run `gh actions-pin` to pin them"},
			notWanted:  []string{"resolving below"},
		},
		{
			name:          "sha-as-ref shows resolving-below when remediating",
			willRemediate: true,
			findings: []doctor.Finding{{
				WorkflowPath: ".github/workflows/a.yml",
				Category:     doctor.CategorySHAAsRef,
				Severity:     doctor.SeverityWarning,
				Confidence:   doctor.ConfidenceHigh,
				ActionRef:    &lockfile.ActionRef{Owner: "octo", Repo: "action", Ref: "abc123"},
				Dependency: &lockfile.Dependency{
					NWO: "octo/action",
					Ref: "abc123",
					SHA: "abc1230000000000000000000000000000000000",
				},
			}},
			wantOutput: []string{"bare SHA", "↳ resolving below"},
			notWanted:  []string{"run `gh actions-pin`"},
		},
		{
			name:          "ref-moved always shows upgrade hint as ↳ follow-up",
			willRemediate: true,
			findings: []doctor.Finding{{
				WorkflowPath: ".github/workflows/a.yml",
				Category:     doctor.CategoryRefMoved,
				Severity:     doctor.SeverityWarning,
				Confidence:   doctor.ConfidenceHigh,
				Dependency: &lockfile.Dependency{
					NWO: "octo/action",
					Ref: "v1",
					SHA: "1111111111111111111111111111111111111111",
				},
				ObservedSHA: "2222222222222222222222222222222222222222",
				Detail:      "ref v1 now resolves to 222222222222",
			}},
			wantOutput: []string{
				"moved upstream",
				"↳ run `gh actions-pin` to update",
			},
		},
		{
			name:          "inconclusive warning surfaces ↳ remediation when set",
			willRemediate: false,
			findings: []doctor.Finding{{
				WorkflowPath: ".github/workflows/a.yml",
				Category:     doctor.CategoryReachabilityUnknown,
				Severity:     doctor.SeverityWarning,
				Confidence:   doctor.ConfidenceLow,
				Dependency: &lockfile.Dependency{
					NWO: "octo/action",
					Ref: "v1",
					SHA: "1111111111111111111111111111111111111111",
				},
				Detail:      "branch list rate limited",
				Remediation: "retry later",
			}},
			wantOutput: []string{
				"branch list rate limited",
				"↳ retry later",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, buf := newTestUI()
			report := &doctor.Report{
				Workflows: []doctor.WorkflowReport{{
					Path:     ".github/workflows/a.yml",
					Findings: tt.findings,
				}},
			}
			PresentResults(u, report, true, tt.willRemediate)

			got := buf.String()
			for _, want := range tt.wantOutput {
				if !strings.Contains(got, want) {
					t.Errorf("PresentResults output missing %q\nfull output:\n%s", want, got)
				}
			}
			for _, unwanted := range tt.notWanted {
				if strings.Contains(got, unwanted) {
					t.Errorf("PresentResults output unexpectedly contains %q\nfull output:\n%s", unwanted, got)
				}
			}
		})
	}
}

// long-standing decision (see TODO in terminal.go) that transitive deps
// pinned to bare SHAs are not surfaced — the consumer can't act on
// upstream composite actions' pinning choices and per-transitive
// warnings drown out the actionable ones.
// TestPresentResults_TransitiveSHAAsRefStillSuppressed locks in the
// long-standing decision (see TODO in terminal.go) that transitive deps
// pinned to bare SHAs are not surfaced — the consumer can't act on
// upstream composite actions' pinning choices and per-transitive
// warnings drown out the actionable ones.
func TestPresentResults_TransitiveSHAAsRefStillSuppressed(t *testing.T) {
	u, buf := newTestUI()
	report := &doctor.Report{
		Workflows: []doctor.WorkflowReport{{
			Path: ".github/workflows/a.yml",
			Findings: []doctor.Finding{{
				WorkflowPath: ".github/workflows/a.yml",
				Category:     doctor.CategorySHAAsRef,
				Severity:     doctor.SeverityWarning,
				Confidence:   doctor.ConfidenceHigh,
				// Transitive: Dependency set, ActionRef nil.
				Dependency: &lockfile.Dependency{
					NWO: "octo/composite",
					Ref: "abc123",
					SHA: "abc1230000000000000000000000000000000000",
				},
				Detail: "transitive bare SHA",
			}},
		}},
	}
	PresentResults(u, report, true, false)

	got := buf.String()
	if strings.Contains(got, "bare SHA") {
		t.Errorf("transitive sha-as-ref should be suppressed, got:\n%s", got)
	}
}

// TestPresentResults_ParseWarningsSurface guards the per-workflow
// ParseWarnings rendering: malformed `uses:` lines must reach the
// terminal, not just JSON.
func TestPresentResults_ParseWarningsSurface(t *testing.T) {
	u, buf := newTestUI()
	report := &doctor.Report{
		Workflows: []doctor.WorkflowReport{{
			Path:          ".github/workflows/a.yml",
			ParseWarnings: []string{"malformed uses: at line 12"},
		}},
	}
	PresentResults(u, report, true, false)

	got := buf.String()
	if !strings.Contains(got, "malformed uses:") {
		t.Errorf("ParseWarnings missing from terminal output:\n%s", got)
	}
	if !strings.Contains(got, ".github/workflows/a.yml") {
		t.Errorf("ParseWarnings should be prefixed with workflow path:\n%s", got)
	}
}

// TestPresentResults_RepoFindingsSurface guards repo-level warnings
// (e.g. non-immutable releases) reach the terminal.
func TestPresentResults_RepoFindingsSurface(t *testing.T) {
	u, buf := newTestUI()
	report := &doctor.Report{
		RepoFindings: []doctor.Finding{{
			Category:   doctor.CategoryNotPinned,
			Severity:   doctor.SeverityWarning,
			Confidence: doctor.ConfidenceHigh,
			Detail:     "repo publishes non-immutable releases",
			DocURL:     "https://example.com/docs",
		}},
	}
	PresentResults(u, report, true, false)

	got := buf.String()
	if !strings.Contains(got, "repo publishes non-immutable releases") {
		t.Errorf("RepoFindings missing from terminal output:\n%s", got)
	}
	if !strings.Contains(got, "example.com/docs") {
		t.Errorf("RepoFindings DocURL missing from terminal output:\n%s", got)
	}
}
