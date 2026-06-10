package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/github/gh-actions-pin/internal/pin"
	"github.com/github/gh-actions-pin/internal/resolve"
	"github.com/github/gh-actions-pin/internal/ui"
)

// A workflow shared between two different actions must still be listed under
// every action that uses it. The earlier global dedup dropped a workflow from a
// later group once any earlier group had already claimed it.
func TestRenderPinnedEntries_WorkflowSharedAcrossActions(t *testing.T) {
	pinned := []pin.Entry{
		{NWO: "actions/checkout", Ref: "v4", SHA: "1111111aaaaaaa", Direct: true, Workflows: []string{"release.yml"}},
		{NWO: "actions/setup-go", Ref: "v5", SHA: "2222222bbbbbbb", Direct: true, Workflows: []string{"ci.yml"}},
		{NWO: "actions/checkout", Ref: "v4", SHA: "1111111aaaaaaa", Direct: true, Workflows: []string{"ci.yml"}},
	}

	var buf bytes.Buffer
	console := ui.NewPlain(&buf)
	renderPinnedEntries(console, pinned)
	out := buf.String()

	if got := strings.Count(out, "ci.yml"); got != 2 {
		t.Fatalf("expected ci.yml listed under both actions (2 occurrences), got %d\noutput:\n%s", got, out)
	}
	if got := strings.Count(out, "release.yml"); got != 1 {
		t.Fatalf("expected release.yml listed once, got %d\noutput:\n%s", got, out)
	}
	if !strings.Contains(out, "Pinned 2 actions across 2 workflows") {
		t.Fatalf("expected header counting 2 actions across 2 workflows, got:\n%s", out)
	}
}

func TestRenderInvestigationAlerts_DeduplicatesByNWORef(t *testing.T) {
	entries := []pin.Entry{
		{
			NWO:       "JasonEtco/create-an-issue",
			Ref:       "9e6213aec58987fa7d2f4deb8b256b99e63107a2",
			Issue:     "impostor-commit",
			Workflows: []string{".github/workflows/rotation-a.yml"},
		},
		{
			NWO:       "JasonEtco/create-an-issue",
			Ref:       "9e6213aec58987fa7d2f4deb8b256b99e63107a2",
			Issue:     "impostor-commit",
			Workflows: []string{".github/workflows/rotation-b.yml"},
		},
	}

	var buf bytes.Buffer
	console := ui.NewPlain(&buf)
	r := &resolve.Resolver{}
	renderInvestigationAlerts(console, entries, r)
	out := buf.String()

	// Header should count 1 unique action, not 2 raw entries.
	if !strings.Contains(out, "1 action requires maintainer action") {
		t.Errorf("expected '1 action requires maintainer action', got:\n%s", out)
	}

	// The NWO@Ref line should appear exactly once.
	if got := strings.Count(out, "JasonEtco/create-an-issue@9e6213"); got != 1 {
		t.Errorf("expected NWO@Ref listed once, got %d\noutput:\n%s", got, out)
	}

	// Both workflows should be listed.
	if !strings.Contains(out, "rotation-a.yml") || !strings.Contains(out, "rotation-b.yml") {
		t.Errorf("expected both workflows listed, got:\n%s", out)
	}
}

func TestRenderInvestigationAlerts_DistinctActionsStaySeparate(t *testing.T) {
	entries := []pin.Entry{
		{
			NWO:       "octo/action-a",
			Ref:       "aaaa",
			Issue:     "impostor-commit",
			Workflows: []string{"ci.yml"},
		},
		{
			NWO:       "octo/action-b",
			Ref:       "bbbb",
			Issue:     "impostor-commit",
			Workflows: []string{"ci.yml"},
		},
	}

	var buf bytes.Buffer
	console := ui.NewPlain(&buf)
	r := &resolve.Resolver{}
	renderInvestigationAlerts(console, entries, r)
	out := buf.String()

	if !strings.Contains(out, "2 actions require maintainer action") {
		t.Errorf("expected '2 actions require maintainer action', got:\n%s", out)
	}
	if !strings.Contains(out, "octo/action-a@aaaa") || !strings.Contains(out, "octo/action-b@bbbb") {
		t.Errorf("expected both distinct actions listed, got:\n%s", out)
	}
}

func TestRenderInvestigationAlerts_WorkflowDedup(t *testing.T) {
	// Same workflow appears in multiple entries for the same action —
	// should only be listed once after grouping.
	entries := []pin.Entry{
		{
			NWO:       "octo/action",
			Ref:       "v1",
			Issue:     "impostor-commit",
			Workflows: []string{"ci.yml", "deploy.yml"},
		},
		{
			NWO:       "octo/action",
			Ref:       "v1",
			Issue:     "impostor-commit",
			Workflows: []string{"ci.yml"},
		},
	}

	var buf bytes.Buffer
	console := ui.NewPlain(&buf)
	r := &resolve.Resolver{}
	renderInvestigationAlerts(console, entries, r)
	out := buf.String()

	if got := strings.Count(out, "ci.yml"); got != 1 {
		t.Errorf("expected ci.yml listed once (deduped), got %d\noutput:\n%s", got, out)
	}
	if got := strings.Count(out, "deploy.yml"); got != 1 {
		t.Errorf("expected deploy.yml listed once, got %d\noutput:\n%s", got, out)
	}
}

func TestRenderInvestigationAlerts_ImpostorCommitEscalation(t *testing.T) {
	entries := []pin.Entry{
		{
			NWO:       "octo/action",
			Ref:       "abc123abc123abc123abc123abc123abc123abcd",
			Issue:     "impostor-commit",
			Workflows: []string{"ci.yml"},
		},
	}

	var buf bytes.Buffer
	console := ui.NewPlain(&buf)
	r := &resolve.Resolver{}
	renderInvestigationAlerts(console, entries, r)
	out := buf.String()

	// All-impostor header.
	if !strings.Contains(out, "requires maintainer action") {
		t.Errorf("expected maintainer action header for all-impostor entries, got:\n%s", out)
	}

	// Impostor context line.
	if !strings.Contains(out, "indistinguishable from impostor") {
		t.Errorf("expected impostor commit context line, got:\n%s", out)
	}

	// Actionable copy with → arrow.
	if !strings.Contains(out, "→") {
		t.Errorf("expected → action arrow, got:\n%s", out)
	}
	if !strings.Contains(out, "Ask the action maintainer") {
		t.Errorf("expected actionable escalation copy, got:\n%s", out)
	}

	// Doc link present — plain UI renders the display text, not the URL.
	if !strings.Contains(out, "Using tags for release management") {
		t.Errorf("expected doc link for tag release management, got:\n%s", out)
	}
}

func TestCleanUnresolvedReason(t *testing.T) {
	tests := []struct {
		name     string
		reason   string
		nwo      string
		ref      string
		wantText string
		wantHint string
	}{
		{
			name:     "strips resolution failed and NWO@Ref prefix",
			reason:   "resolution failed: actions/checkout@v4.3.1: connection refused",
			nwo:      "actions/checkout",
			ref:      "v4.3.1",
			wantText: "connection refused",
			wantHint: "",
		},
		{
			name:     "strips resolution failed prefix only",
			reason:   "resolution failed: some generic error",
			nwo:      "actions/checkout",
			ref:      "v4",
			wantText: "some generic error",
			wantHint: "",
		},
		{
			name:     "SSO error extracts fix hint and trims authorize guidance",
			reason:   `resolution failed: actions/checkout@v4.3.1: SSO authorization required: your token is not authorized for the "actions" organization (SAML enforcement). Authorize it at https://github.com/orgs/actions/sso and retry`,
			nwo:      "actions/checkout",
			ref:      "v4.3.1",
			wantText: `SSO authorization required: your token is not authorized for the "actions" organization (SAML enforcement)`,
			wantHint: "Authorize in your web browser:  https://github.com/orgs/actions/sso",
		},
		{
			name:     "bare reason without prefixes passes through",
			reason:   "repository not found",
			nwo:      "octo/action",
			ref:      "v1",
			wantText: "repository not found",
			wantHint: "",
		},
		{
			name:     "empty reason returns empty",
			reason:   "",
			nwo:      "octo/action",
			ref:      "v1",
			wantText: "",
			wantHint: "",
		},
		{
			name:     "multiline reason prefers detail after colon header",
			reason:   "resolution failed:\nactual error details here",
			nwo:      "octo/action",
			ref:      "v1",
			wantText: "actual error details here",
			wantHint: "",
		},
		{
			name:     "cross-action NWO@Ref prefix also stripped",
			reason:   "resolution failed: other/action@v2: not found",
			nwo:      "actions/checkout",
			ref:      "v4",
			wantText: "not found",
			wantHint: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotText, gotHint := cleanUnresolvedReason(tt.reason, tt.nwo, tt.ref)
			if gotText != tt.wantText {
				t.Errorf("text: got %q, want %q", gotText, tt.wantText)
			}
			if gotHint != tt.wantHint {
				t.Errorf("hint: got %q, want %q", gotHint, tt.wantHint)
			}
		})
	}
}

func TestRenderUnresolvedWarnings_SSOShowsFixHint(t *testing.T) {
	entries := []pin.Entry{
		{
			NWO:       "actions/checkout",
			Ref:       "v4.3.1",
			Reason:    `resolution failed: actions/checkout@v4.3.1: SSO authorization required: your token is not authorized for the "actions" organization (SAML enforcement). Authorize it at https://github.com/orgs/actions/sso and retry`,
			Workflows: []string{".github/workflows/ci.yml", ".github/workflows/deploy.yml"},
		},
	}

	var buf bytes.Buffer
	console := ui.NewPlain(&buf)
	renderUnresolvedWarnings(console, entries)
	out := buf.String()

	// Redundant "resolution failed: actions/checkout@v4.3.1:" prefix must be gone.
	if strings.Contains(out, "resolution failed:") {
		t.Errorf("expected 'resolution failed:' prefix stripped, got:\n%s", out)
	}

	// Clean reason should appear.
	if !strings.Contains(out, "SSO authorization required") {
		t.Errorf("expected clean reason text, got:\n%s", out)
	}

	// The trailing "Authorize it at ... and retry" noise should be trimmed from the reason.
	if strings.Contains(out, "and retry") {
		t.Errorf("expected trailing 'and retry' guidance trimmed, got:\n%s", out)
	}

	// SSO URL is NOT rendered here — it comes from the X-GitHub-SSO header
	// and is surfaced by check.go at the end of the run.
	if strings.Contains(out, "Authorize in your web browser") {
		t.Errorf("SSO hint should not be rendered by renderUnresolvedWarnings (comes from check.go), got:\n%s", out)
	}
}

func TestRenderUnresolvedWarnings_DedupsBySameReason(t *testing.T) {
	ssoReason := `resolution failed: actions/checkout@v6.0.2: SSO authorization required: your token is not authorized for the "actions" organization (SAML enforcement). Authorize it at https://github.com/orgs/actions/sso and retry`
	entries := []pin.Entry{
		{NWO: "actions/checkout", Ref: "v6.0.2", Reason: ssoReason, Workflows: []string{"ci.yml"}},
		{NWO: "actions/setup-go", Ref: "v6.4.0", Reason: `resolution failed: actions/setup-go@v6.4.0: SSO authorization required: your token is not authorized for the "actions" organization (SAML enforcement). Authorize it at https://github.com/orgs/actions/sso and retry`, Workflows: []string{"ci.yml"}},
		{NWO: "actions/cache", Ref: "v5.0.5", Reason: `resolution failed: actions/cache@v5.0.5: SSO authorization required: your token is not authorized for the "actions" organization (SAML enforcement). Authorize it at https://github.com/orgs/actions/sso and retry`, Workflows: []string{"deploy.yml"}},
	}

	var buf bytes.Buffer
	console := ui.NewPlain(&buf)
	renderUnresolvedWarnings(console, entries)
	out := buf.String()

	// The SSO reason should appear exactly once (deduped).
	if got := strings.Count(out, "SSO authorization required"); got != 1 {
		t.Errorf("expected SSO reason to appear once (deduped), got %d\noutput:\n%s", got, out)
	}

	// All three actions should be listed.
	if !strings.Contains(out, "actions/checkout@v6.0.2") {
		t.Errorf("missing actions/checkout@v6.0.2\noutput:\n%s", out)
	}
	if !strings.Contains(out, "actions/setup-go@v6.4.0") {
		t.Errorf("missing actions/setup-go@v6.4.0\noutput:\n%s", out)
	}
	if !strings.Contains(out, "actions/cache@v5.0.5") {
		t.Errorf("missing actions/cache@v5.0.5\noutput:\n%s", out)
	}

	// Header should show 3 actions.
	if !strings.Contains(out, "3 actions could not be resolved") {
		t.Errorf("expected '3 actions' in header\noutput:\n%s", out)
	}

	// Actions should appear BEFORE the reason (multi-action layout).
	lastAction := strings.LastIndex(out, "actions/cache@v5.0.5")
	reasonIdx := strings.Index(out, "SSO authorization required")
	if lastAction > reasonIdx {
		t.Errorf("actions should appear before reason in multi-action layout\noutput:\n%s", out)
	}
}

func TestRenderUnresolvedWarnings_MixedReasonsNotDeduped(t *testing.T) {
	entries := []pin.Entry{
		{NWO: "actions/checkout", Ref: "v4", Reason: `resolution failed: actions/checkout@v4: SSO authorization required: your token is not authorized for the "actions" organization (SAML enforcement). Authorize it at https://github.com/orgs/actions/sso and retry`, Workflows: []string{"ci.yml"}},
		{NWO: "octo/private", Ref: "v1", Reason: "resolution failed: octo/private@v1: repository not found", Workflows: []string{"ci.yml"}},
	}

	var buf bytes.Buffer
	console := ui.NewPlain(&buf)
	renderUnresolvedWarnings(console, entries)
	out := buf.String()

	// Different reasons should both appear.
	if !strings.Contains(out, "SSO authorization required") {
		t.Errorf("expected SSO reason\noutput:\n%s", out)
	}
	if !strings.Contains(out, "repository not found") {
		t.Errorf("expected 'repository not found' reason\noutput:\n%s", out)
	}

	// Single-action entries should show action above reason (inline style).
	checkoutIdx := strings.Index(out, "actions/checkout@v4")
	ssoIdx := strings.Index(out, "SSO authorization required")
	if checkoutIdx > ssoIdx {
		t.Errorf("single-action entry: action should appear before reason\noutput:\n%s", out)
	}
}


func TestRenderUnresolvedWarnings_PlainErrorNoHint(t *testing.T) {
	entries := []pin.Entry{
		{
			NWO:       "octo/action",
			Ref:       "v1",
			Reason:    "resolution failed: octo/action@v1: repository not found",
			Workflows: []string{"ci.yml"},
		},
	}

	var buf bytes.Buffer
	console := ui.NewPlain(&buf)
	renderUnresolvedWarnings(console, entries)
	out := buf.String()

	// Prefixes stripped, clean reason shown.
	if strings.Contains(out, "resolution failed:") {
		t.Errorf("expected prefix stripped, got:\n%s", out)
	}
	if !strings.Contains(out, "repository not found") {
		t.Errorf("expected clean reason, got:\n%s", out)
	}

	// No fix hint for generic errors.
	if strings.Contains(out, "→") {
		t.Errorf("expected no fix hint for generic error, got:\n%s", out)
	}
}

func TestRenderInvestigationAlerts_MixedIssuesFallbackHeader(t *testing.T) {
	entries := []pin.Entry{
		{
			NWO:       "octo/action",
			Ref:       "abc123abc123abc123abc123abc123abc123abcd",
			Issue:     "impostor-commit",
			Workflows: []string{"ci.yml"},
		},
		{
			NWO:       "other/dep",
			Ref:       "v2",
			Issue:     "ref-moved",
			Workflows: []string{"deploy.yml"},
		},
	}

	var buf bytes.Buffer
	console := ui.NewPlain(&buf)
	r := &resolve.Resolver{}
	renderInvestigationAlerts(console, entries, r)
	out := buf.String()

	// Mixed issues should use the generic header, not the impostor-specific one.
	if strings.Contains(out, "requires maintainer action") {
		t.Errorf("expected generic header for mixed issues, got:\n%s", out)
	}
	if !strings.Contains(out, "investigation") {
		t.Errorf("expected investigation header for mixed issues, got:\n%s", out)
	}
}

func TestRenderPinnedEntries_AutoFixNoteAfterWorkflows(t *testing.T) {
	pinned := []pin.Entry{
		{
			NWO:          "slackapi/slack-github-action",
			Ref:          "v3.0.3",
			SHA:          "45a88b9000000000000000000000000000000000",
			AutoFixedRef: "6c661ce5880400000000000000000000000000000",
			Direct:       true,
			Workflows:    []string{"retry.yml", "deploy.yml"},
		},
	}

	var buf bytes.Buffer
	console := ui.NewPlain(&buf)
	renderPinnedEntries(console, pinned)
	out := buf.String()

	// The auto-fix note should appear after the workflow list.
	wfPos := strings.LastIndex(out, "deploy.yml")
	notePos := strings.Index(out, "re-pinned from unreachable")
	if wfPos == -1 || notePos == -1 {
		t.Fatalf("expected both workflow and auto-fix note in output:\n%s", out)
	}
	if notePos < wfPos {
		t.Errorf("auto-fix note should appear after workflow list, not before\noutput:\n%s", out)
	}

	// Yellow ! icon on the auto-fix line.
	if !strings.Contains(out, "! re-pinned") {
		t.Errorf("expected '!' icon on auto-fix line, got:\n%s", out)
	}
}
