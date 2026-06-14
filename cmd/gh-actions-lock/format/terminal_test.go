package format

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/github/gh-actions-lock/internal/pipeline/checks"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/ui"
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
		findings   []checks.Finding
		wantOutput []string
		notWanted  []string
	}{
		{
			name: "ref-moved warning is swallowed until update path exists",
			findings: []checks.Finding{{
				WorkflowPath: ".github/workflows/a.yml",
				Category:     checks.RefMoved,
				Severity:     checks.SeverityWarning,
				Confidence:   checks.ConfidenceHigh,
				Dependency: &dep.Dependency{
					NWO: "octo/action",
					Ref: "v1",
					SHA: "1111111111111111111111111111111111111111",
				},
				ObservedSHA: "2222222222222222222222222222222222222222",
				Detail:      "ref v1 now resolves to 222222222222, lockfile pins 111111111111",
			}},
			notWanted: []string{
				"moved upstream",
				"compare/111111111111...222222222222",
				"run `gh actions-lock` to update",
			},
		},
		{
			name: "sha-as-ref direct warning surfaces",
			findings: []checks.Finding{{
				WorkflowPath: ".github/workflows/a.yml",
				Category:     checks.ShaAsRef,
				Severity:     checks.SeverityWarning,
				Confidence:   checks.ConfidenceHigh,
				ActionRef:    &parserlock.ActionRef{Owner: "octo", Repo: "action", Ref: "abc123"},
				Dependency: &dep.Dependency{
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
			findings: []checks.Finding{{
				WorkflowPath: ".github/workflows/a.yml",
				Category:     checks.NotPinned,
				Severity:     checks.SeverityWarning,
				Confidence:   checks.ConfidenceHigh,
				Detail:       "no lockfile entry",
			}},
			wantOutput: []string{
				"not yet pinned",
			},
		},
		{
			name: "reachability-unknown surfaces under the dep key",
			findings: []checks.Finding{{
				WorkflowPath: ".github/workflows/a.yml",
				Category:     checks.ReachabilityUnknown,
				Severity:     checks.SeverityWarning,
				Confidence:   checks.ConfidenceLow,
				Dependency: &dep.Dependency{
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
			report := &checks.Report{
				Workflows: []checks.WorkflowReport{{
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

// TestPresentResults_NoSuccessLine verifies PresentResults no longer emits
// the "All N workflows valid" success line — renderPinSummary owns that.
func TestPresentResults_NoSuccessLine(t *testing.T) {
	u, buf := newTestUI()
	report := &checks.Report{
		Workflows: []checks.WorkflowReport{
			{Path: ".github/workflows/a.yml"},
			{Path: ".github/workflows/b.yml"},
		},
	}
	PresentResults(u, report, true, false)

	got := buf.String()
	if strings.Contains(got, "All") && strings.Contains(got, "valid") {
		t.Errorf("PresentResults should not print success line, got:\n%s", got)
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
		findings      []checks.Finding
		wantOutput    []string
		notWanted     []string
	}{
		{
			name:          "not-pinned shows resolving-below when remediating",
			willRemediate: true,
			findings: []checks.Finding{{
				WorkflowPath: ".github/workflows/a.yml",
				Category:     checks.NotPinned,
				Severity:     checks.SeverityWarning,
				Confidence:   checks.ConfidenceHigh,
			}},
			wantOutput: []string{"not yet pinned", "↳ resolving below"},
			notWanted:  []string{"run `gh actions-lock`"},
		},
		{
			name:          "not-pinned shows manual hint when not remediating",
			willRemediate: false,
			findings: []checks.Finding{{
				WorkflowPath: ".github/workflows/a.yml",
				Category:     checks.NotPinned,
				Severity:     checks.SeverityWarning,
				Confidence:   checks.ConfidenceHigh,
			}},
			wantOutput: []string{"not yet pinned", "↳ run `gh actions-lock` to pin them"},
			notWanted:  []string{"resolving below"},
		},
		{
			name:          "sha-as-ref suppressed when remediating",
			willRemediate: true,
			findings: []checks.Finding{{
				WorkflowPath: ".github/workflows/a.yml",
				Category:     checks.ShaAsRef,
				Severity:     checks.SeverityWarning,
				Confidence:   checks.ConfidenceHigh,
				ActionRef:    &parserlock.ActionRef{Owner: "octo", Repo: "action", Ref: "abc123"},
				Dependency: &dep.Dependency{
					NWO: "octo/action",
					Ref: "abc123",
					SHA: "abc1230000000000000000000000000000000000",
				},
			}},
			notWanted: []string{"bare SHA", "resolving below"},
		},
		{
			name:          "ref-moved is swallowed (deferred to update path)",
			willRemediate: true,
			findings: []checks.Finding{{
				WorkflowPath: ".github/workflows/a.yml",
				Category:     checks.RefMoved,
				Severity:     checks.SeverityWarning,
				Confidence:   checks.ConfidenceHigh,
				Dependency: &dep.Dependency{
					NWO: "octo/action",
					Ref: "v1",
					SHA: "1111111111111111111111111111111111111111",
				},
				ObservedSHA: "2222222222222222222222222222222222222222",
				Detail:      "ref v1 now resolves to 222222222222",
			}},
			notWanted: []string{
				"moved upstream",
				"run `gh actions-lock` to update",
			},
		},
		{
			name:          "inconclusive warning surfaces ↳ remediation when set",
			willRemediate: false,
			findings: []checks.Finding{{
				WorkflowPath: ".github/workflows/a.yml",
				Category:     checks.ReachabilityUnknown,
				Severity:     checks.SeverityWarning,
				Confidence:   checks.ConfidenceLow,
				Dependency: &dep.Dependency{
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
			report := &checks.Report{
				Workflows: []checks.WorkflowReport{{
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

func TestPresentResults_TransitiveSHAAsRefStillSuppressed(t *testing.T) {
	u, buf := newTestUI()
	report := &checks.Report{
		Workflows: []checks.WorkflowReport{{
			Path: ".github/workflows/a.yml",
			Findings: []checks.Finding{{
				WorkflowPath: ".github/workflows/a.yml",
				Category:     checks.ShaAsRef,
				Severity:     checks.SeverityWarning,
				Confidence:   checks.ConfidenceHigh,
				// Transitive: Dependency set, ActionRef nil.
				Dependency: &dep.Dependency{
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
	report := &checks.Report{
		Workflows: []checks.WorkflowReport{{
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
	report := &checks.Report{
		RepoFindings: []checks.Finding{{
			Category:   checks.NotPinned,
			Severity:   checks.SeverityWarning,
			Confidence: checks.ConfidenceHigh,
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
