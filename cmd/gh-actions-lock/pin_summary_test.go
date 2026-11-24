package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/github/gh-actions-lock/internal/pin"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/github/gh-actions-lock/internal/resolve"
	"github.com/github/gh-actions-lock/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestRenderNarrowedEntries_BranchRepair(t *testing.T) {
	var buf bytes.Buffer
	console := ui.NewPlain(&buf)

	renderNarrowedEntries(console, []pin.Entry{{
		NWO:          "owner/action",
		Ref:          "main",
		AutoFixedRef: strings.Repeat("a", 40),
	}})

	out := buf.String()
	assert.Contains(t, out, "Updated 1 ref")
	assert.NotContains(t, out, "semver")
	assert.Contains(t, out, "owner/action@aaaaaaaa")
	assert.Contains(t, out, "→ main")
}

func TestReportHasUnfixableErrors_ClassifiesWorkflowNotPinned(t *testing.T) {
	for _, tt := range []struct {
		name        string
		category    checks.Category
		remediable  bool
		wantBlocked bool
	}{
		{name: "malformed nested ref", category: checks.NotPinned, wantBlocked: true},
		{name: "dependency inventory error", category: checks.NotPinned, remediable: true},
		{name: "workflow load error", category: checks.InvalidWorkflow, wantBlocked: true},
		{name: "missing ref", category: checks.MissingRef, wantBlocked: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			report := &checks.Report{
				Workflows: []checks.WorkflowReport{{
					Findings: []checks.Finding{{
						Category:   tt.category,
						Severity:   checks.SeverityError,
						Remediable: tt.remediable,
					}},
				}},
			}

			if got := reportHasUnfixableErrors(report, false); got != tt.wantBlocked {
				t.Errorf("reportHasUnfixableErrors() = %v, want %v", got, tt.wantBlocked)
			}
			if got := reportHasNonInvestigatedUnfixableErrors(report); got != tt.wantBlocked {
				t.Errorf("reportHasNonInvestigatedUnfixableErrors() = %v, want %v", got, tt.wantBlocked)
			}
		})
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
	if !strings.Contains(out, "1 action requires investigation") {
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

	if !strings.Contains(out, "2 actions require investigation") {
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
			wantHint: "Run:  gh auth refresh -h github.com",
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

	// Fix hint should now be rendered inline (no longer relies on X-GitHub-SSO header).
	if !strings.Contains(out, "gh auth refresh") {
		t.Errorf("expected fix hint containing 'gh auth refresh', got:\n%s", out)
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

	// Fix hint should appear exactly once (deduped with the reason bucket).
	if got := strings.Count(out, "gh auth refresh"); got != 1 {
		t.Errorf("expected fix hint to appear once (deduped), got %d\noutput:\n%s", got, out)
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

func TestRenderVersionRefNudge_SkipsTransitiveDeps(t *testing.T) {
	record := &pin.Record{
		Entries: []pin.Entry{
			// Direct dep with imprecise ref → should appear in the nudge.
			{NWO: "actions/checkout", Ref: "v6", SHA: "aaa", Resolution: pin.Pinned, Direct: true, Workflows: []string{"ci.yml"}},
			// Transitive dep with imprecise ref → should NOT appear.
			{NWO: "actions/setup-go", Ref: "v6", SHA: "bbb", Resolution: pin.Pinned, Direct: false, Workflows: []string{"ci.yml"}},
			// Transitive dep pinned to a bare SHA → should NOT appear.
			{NWO: "golangci/golangci-lint-action", Ref: "1e7e51e771db61008b38414a730f564565cf7c20", SHA: "1e7e51e771db61008b38414a730f564565cf7c20", Resolution: pin.Pinned, Direct: false, Workflows: []string{"ci.yml"}},
			// Direct dep with full semver → should NOT appear (already precise).
			{NWO: "actions/cache", Ref: "v4.2.1", SHA: "ccc", Resolution: pin.Pinned, Direct: true, Workflows: []string{"ci.yml"}},
		},
	}

	reg := &httpmock.Registry{}
	reg.Register(httpmock.REST("GET", "repos/actions/checkout/tags"), httpmock.JSONResponse([]map[string]any{
		{"name": "v6.1.2", "commit": map[string]string{"sha": "ddd"}},
		{"name": "v6", "commit": map[string]string{"sha": "ddd"}},
	}))
	r, err := resolve.New("github.com", nil, resolve.WithTransport(reg))
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	console := ui.NewPlain(&buf)
	renderVersionRefNudge(context.Background(), console, record, r)
	out := buf.String()

	if !strings.Contains(out, "actions/checkout@v6") {
		t.Errorf("expected direct imprecise dep in nudge, got:\n%s", out)
	}
	if !strings.Contains(out, "v6.1.2") {
		t.Errorf("expected latest tag suggestion, got:\n%s", out)
	}
	if strings.Contains(out, "actions/setup-go") {
		t.Errorf("transitive dep should be excluded from nudge, got:\n%s", out)
	}
	if strings.Contains(out, "golangci/golangci-lint-action") {
		t.Errorf("transitive dep should be excluded from nudge, got:\n%s", out)
	}
	if strings.Contains(out, "actions/cache") {
		t.Errorf("full semver dep should be excluded from nudge, got:\n%s", out)
	}
	if !strings.Contains(out, "1 action") {
		t.Errorf("expected '1 action' count (only the direct imprecise dep), got:\n%s", out)
	}
}

func TestRenderVersionRefNudge_StaysInTagPrefixFamily(t *testing.T) {
	record := &pin.Record{
		Entries: []pin.Entry{
			{NWO: "octo/tools", Ref: "verify/v0", SHA: "aaa", Resolution: pin.Pinned, Direct: true, Workflows: []string{"ci.yml"}},
			{NWO: "octo/tools", Ref: "v1", SHA: "bbb", Resolution: pin.Pinned, Direct: true, Workflows: []string{"ci.yml"}},
		},
	}

	reg := &httpmock.Registry{}
	reg.RegisterReusable(httpmock.REST("GET", "repos/octo/tools/tags"), httpmock.JSONResponse([]map[string]any{
		{"name": "lint/v3.0.0", "commit": map[string]string{"sha": "ccc"}},
		{"name": "verify/v0.6.0", "commit": map[string]string{"sha": "ddd"}},
		{"name": "v1.2.0", "commit": map[string]string{"sha": "eee"}},
	}))
	r, err := resolve.New("github.com", nil, resolve.WithTransport(reg))
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	renderVersionRefNudge(context.Background(), ui.NewPlain(&buf), record, r)
	out := buf.String()

	if !strings.Contains(out, "octo/tools@verify/v0 → latest: verify/v0.6.0") {
		t.Errorf("expected prefixed ref nudged to its own family, got:\n%s", out)
	}
	if !strings.Contains(out, "octo/tools@v1 → latest: v1.2.0") {
		t.Errorf("expected unprefixed ref nudged to an unprefixed tag, got:\n%s", out)
	}
	if strings.Contains(out, "lint/") {
		t.Errorf("nudge crossed into another tag family, got:\n%s", out)
	}
}

// A multi-action repository can have more than one page of tags, and a
// family can sort entirely past the first page.
func TestRenderVersionRefNudge_PrefixedFamilyOnLaterPage(t *testing.T) {
	record := &pin.Record{
		Entries: []pin.Entry{
			{NWO: "octo/tools", Ref: "verify/v0", SHA: "aaa", Resolution: pin.Pinned, Direct: true, Workflows: []string{"ci.yml"}},
		},
	}

	var first []string
	for i := range 100 {
		first = append(first, fmt.Sprintf("zeta/v1.0.%d", i), "ccc")
	}
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(httpmock.RESTWithQuery("GET", `^/repos/octo/tools/tags$`, "&page=1"), httpmock.JSONResponse(httpmock.TagListResponse(first...)))
	reg.Register(httpmock.RESTWithQuery("GET", `^/repos/octo/tools/tags$`, "&page=2"), httpmock.JSONResponse(httpmock.TagListResponse("verify/v0.6.0", "ddd")))
	r, err := resolve.New("github.com", nil, resolve.WithTransport(reg))
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	renderVersionRefNudge(context.Background(), ui.NewPlain(&buf), record, r)

	if out := buf.String(); !strings.Contains(out, "octo/tools@verify/v0 → latest: verify/v0.6.0") {
		t.Errorf("expected nudge to the family's tag on page 2, got:\n%s", out)
	}
}

func TestRenderVersionRefNudge_AllTransitive_NoOutput(t *testing.T) {
	record := &pin.Record{
		Entries: []pin.Entry{
			{NWO: "actions/checkout", Ref: "v6", SHA: "aaa", Resolution: pin.Pinned, Direct: false, Workflows: []string{"ci.yml"}},
			{NWO: "actions/setup-go", Ref: "v6", SHA: "bbb", Resolution: pin.Verified, Direct: false, Workflows: []string{"ci.yml"}},
		},
	}

	var buf bytes.Buffer
	console := ui.NewPlain(&buf)
	renderVersionRefNudge(context.Background(), console, record, nil)

	if buf.Len() > 0 {
		t.Errorf("expected no output when all imprecise deps are transitive, got:\n%s", buf.String())
	}
}

func TestRenderVersionRefNudge_SkipsReposWithoutSemverTags(t *testing.T) {
	record := &pin.Record{
		Entries: []pin.Entry{
			{NWO: "octo/no-releases", Ref: "main", SHA: "aaa", Resolution: pin.Pinned, Direct: true, Workflows: []string{"ci.yml"}},
		},
	}

	reg := &httpmock.Registry{}
	reg.Register(httpmock.REST("GET", "repos/octo/no-releases/tags"), httpmock.JSONResponse([]map[string]any{
		{"name": "nightly", "commit": map[string]string{"sha": "aaa"}},
	}))
	r, err := resolve.New("github.com", nil, resolve.WithTransport(reg))
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	console := ui.NewPlain(&buf)
	renderVersionRefNudge(context.Background(), console, record, r)

	if buf.Len() > 0 {
		t.Errorf("expected no output for repo without semver releases, got:\n%s", buf.String())
	}
}

func TestInjectVersionRefFindings_SkipsTransitiveDeps(t *testing.T) {
	report := &checks.Report{
		Workflows: []checks.WorkflowReport{
			{Path: "ci.yml"},
		},
	}
	record := &pin.Record{
		Entries: []pin.Entry{
			{NWO: "actions/checkout", Ref: "v6", SHA: "aaa", Resolution: pin.Pinned, Direct: true, Workflows: []string{"ci.yml"}},
			{NWO: "actions/setup-go", Ref: "v6", SHA: "bbb", Resolution: pin.Pinned, Direct: false, Workflows: []string{"ci.yml"}},
		},
	}

	injectVersionRefFindings(context.Background(), report, record, nil)

	findings := report.Workflows[0].Findings
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding (direct only), got %d", len(findings))
	}
	if !strings.Contains(findings[0].Detail, "actions/checkout@v6") {
		t.Errorf("expected finding for direct dep, got: %s", findings[0].Detail)
	}
	if strings.Contains(findings[0].Detail, "actions/setup-go") {
		t.Errorf("transitive dep should not produce a finding, got: %s", findings[0].Detail)
	}
}

func TestInjectVersionRefFindings_PrefixedPartialRef(t *testing.T) {
	report := &checks.Report{Workflows: []checks.WorkflowReport{{Path: "ci.yml"}}}
	record := &pin.Record{
		Entries: []pin.Entry{
			{NWO: "octo/tools", Ref: "verify/v0", SHA: "aaa", Resolution: pin.Pinned, Direct: true, Workflows: []string{"ci.yml"}},
			{NWO: "octo/tools", Ref: "build/v0.5.1", SHA: "bbb", Resolution: pin.Pinned, Direct: true, Workflows: []string{"ci.yml"}},
			{NWO: "octo/tools", Ref: "feature/2fa", SHA: "ccc", Resolution: pin.Pinned, Direct: true, Workflows: []string{"ci.yml"}},
			{NWO: "octo/tools", Ref: "releases/v4", SHA: "ddd", Resolution: pin.Pinned, Direct: true, Workflows: []string{"ci.yml"}},
		},
	}
	reg := &httpmock.Registry{}
	reg.RegisterReusable(httpmock.REST("GET", "repos/octo/tools/tags"), httpmock.JSONResponse([]map[string]any{
		{"name": "verify/v0.5.1", "commit": map[string]string{"sha": "aaa"}},
		{"name": "build/v0.5.1", "commit": map[string]string{"sha": "bbb"}},
		{"name": "v4.0.0", "commit": map[string]string{"sha": "ddd"}},
	}))
	r, err := resolve.New("github.com", nil, resolve.WithTransport(reg))
	if err != nil {
		t.Fatal(err)
	}

	injectVersionRefFindings(context.Background(), report, record, r)

	findings := report.Workflows[0].Findings
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding for the partial prefixed ref (releases/v4 has no family tags), got %d: %+v", len(findings), findings)
	}
	if !strings.Contains(findings[0].Detail, "octo/tools@verify/v0:") {
		t.Errorf("expected finding for octo/tools@verify/v0, got: %s", findings[0].Detail)
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

func TestRenderPinnedEntries_TransitiveWorkflowMerge(t *testing.T) {
	// A dep that is direct in reusable-build.yml and transitive in
	// happy-path.yml (via composite expansion) should show both workflows.
	pinned := []pin.Entry{
		{NWO: "nodeselector/fixtures", Ref: "main", SHA: "aaa", Direct: true, Workflows: []string{"reusable-build.yml"}},
		{NWO: "nodeselector/fixtures", Ref: "main", SHA: "aaa", Direct: false, Workflows: []string{"happy-path.yml"}},
	}

	var buf bytes.Buffer
	console := ui.NewPlain(&buf)
	renderPinnedEntries(console, pinned)
	out := buf.String()

	if !strings.Contains(out, "reusable-build.yml") {
		t.Errorf("expected direct workflow in output, got:\n%s", out)
	}
	if !strings.Contains(out, "happy-path.yml") {
		t.Errorf("expected transitive workflow merged into output, got:\n%s", out)
	}
	// Should count as 1 action, not 2.
	if !strings.Contains(out, "1 action") {
		t.Errorf("expected '1 action' count, got:\n%s", out)
	}
}

func TestRenderPinnedEntries_PurelyTransitiveShown(t *testing.T) {
	// A dep that is only transitive (no direct counterpart) must still
	// appear in the output so the user can see the full closure.
	pinned := []pin.Entry{
		{NWO: "org/composite", Ref: "v1", SHA: "aaa", Direct: true, Workflows: []string{"ci.yml"}},
		{NWO: "org/transitive", Ref: "main", SHA: "bbb", Direct: false,
			Workflows: []string{"ci.yml"}, RequiredBy: []string{"org/composite@v1"}},
	}

	var buf bytes.Buffer
	console := ui.NewPlain(&buf)
	renderPinnedEntries(console, pinned)
	out := buf.String()

	if !strings.Contains(out, "org/transitive@main") {
		t.Errorf("expected purely transitive dep in output, got:\n%s", out)
	}
	if !strings.Contains(out, "via org/composite@v1") {
		t.Errorf("expected 'via' parent attribution, got:\n%s", out)
	}
	if !strings.Contains(out, "(+ 1 transitive)") {
		t.Errorf("expected transitive count in header, got:\n%s", out)
	}
	if !strings.Contains(out, "1 action") {
		t.Errorf("expected '1 action' direct count, got:\n%s", out)
	}
}

func TestRenderAcceptedOverrides_NamesEachOverride(t *testing.T) {
	report := &checks.Report{Workflows: []checks.WorkflowReport{{
		Path: ".github/workflows/ci.yml",
		Findings: []checks.Finding{
			{Category: checks.UnreachablePin, Dependency: &dep.Dependency{NWO: "actions/checkout", Ref: "v4.2.2", SHA: "1111111111111111111111111111111111111111"}},
			{Category: checks.RefMoved, Dependency: &dep.Dependency{NWO: "actions/setup-node", Ref: "v4.4.0", SHA: "8f152de45cc393bb48ce5d89d36b731f54556e65"}},
			{Category: checks.UnreachablePin, Dependency: &dep.Dependency{NWO: "octo/kept", Ref: "v1", SHA: "cccccccccccccccccccccccccccccccccccccccc"}},
		},
	}}}
	pinned := []pin.Entry{
		{NWO: "actions/checkout", Ref: "v4.2.2", SHA: "11bd71901bbe5b1630ceea73d27597364c9af683"},
		{NWO: "actions/setup-node", Ref: "v4.4.0", SHA: "49933ea5288caeca8642d1e84afbd3f7d6820020"},
		{NWO: "octo/kept", Ref: "v1", SHA: "cccccccccccccccccccccccccccccccccccccccc"},
	}

	var buf bytes.Buffer
	renderAcceptedOverrides(ui.NewPlain(&buf), report, pinned)
	out := buf.String()

	assert.Contains(t, out, "Re-pinned actions/checkout@v4.2.2: 111111111111 → 11bd71901bbe (accepted)")
	assert.Contains(t, out, "Re-pinned actions/setup-node@v4.4.0: 8f152de45cc3 → 49933ea5288c (accepted)")
	assert.NotContains(t, out, "octo/kept", "an unchanged pin is not an override")
}

// TestRenderAcceptedOverrides_NamesReRecordedRef: a full-SHA pin keeps its
// commit, but --accept-moved records a new tag or branch for it.
func TestRenderAcceptedOverrides_NamesReRecordedRef(t *testing.T) {
	const sha = "11bd71901bbe5b1630ceea73d27597364c9af683"
	report := &checks.Report{Workflows: []checks.WorkflowReport{{
		Path: ".github/workflows/ci.yml",
		Findings: []checks.Finding{
			{Category: checks.UnreachablePin, Severity: checks.SeverityWarning, RecordedRef: "v4-gone",
				Dependency: &dep.Dependency{NWO: "actions/checkout", Ref: sha, SHA: sha}},
		},
	}}}
	for name, pinned := range map[string][]pin.Entry{
		"uses: rewritten to the tag": {{NWO: "actions/checkout", Ref: "v4.2.2", AutoFixedRef: sha, SHA: sha}},
		"uses: kept as the SHA":      {{NWO: "actions/checkout", Ref: sha, SHA: sha, Tag: "v4.2.2"}},
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			renderAcceptedOverrides(ui.NewPlain(&buf), report, pinned)
			assert.Contains(t, buf.String(), "actions/checkout: recorded ref v4-gone no longer contains 11bd71901bbe; re-recorded as v4.2.2 (--accept-moved)")
		})
	}
}

// TestRenderAcceptedOverrides_AttributesTheFindingsPin: another workflow
// pinning the same commit under a different ref is not the re-recorded pin.
func TestRenderAcceptedOverrides_AttributesTheFindingsPin(t *testing.T) {
	const sha = "a7bc67e1a1d9ae52bb4f3b3a2ef3c1fe62b9d3a4"
	report := &checks.Report{Workflows: []checks.WorkflowReport{
		{Path: ".github/workflows/a.yml"},
		{Path: ".github/workflows/b.yml", Findings: []checks.Finding{
			{Category: checks.UnreachablePin, Severity: checks.SeverityWarning, RecordedRef: "gone",
				Dependency: &dep.Dependency{NWO: "o/child", Ref: sha, SHA: sha}},
		}},
	}}
	for name, b := range map[string]pin.Entry{
		"uses: rewritten to the tag": {NWO: "o/child", Ref: "z-032", AutoFixedRef: sha, SHA: sha,
			Workflows: []string{".github/workflows/b.yml"}},
		"uses: kept as the SHA": {NWO: "o/child", Ref: sha, SHA: sha, OnBranch: "z-032",
			Workflows: []string{".github/workflows/b.yml"}},
	} {
		t.Run(name, func(t *testing.T) {
			pinned := []pin.Entry{
				{NWO: "o/child", Ref: "z-129", SHA: sha, Workflows: []string{".github/workflows/a.yml"}},
				b,
			}
			var buf bytes.Buffer
			renderAcceptedOverrides(ui.NewPlain(&buf), report, pinned)
			assert.Equal(t, "o/child: recorded ref gone no longer contains a7bc67e1a1d9; re-recorded as z-032 (--accept-moved)\n", buf.String())
		})
	}
}

// TestRenderPinSummary_HousekeepingBeforeSummary: the schema upgrade and
// pruning notes print before the pinned summary, not after it.
func TestRenderPinSummary_HousekeepingBeforeSummary(t *testing.T) {
	record := &pin.Record{Entries: []pin.Entry{{
		NWO: "actions/checkout", Ref: "v4.2.2", SHA: "11bd71901bbe5b1630ceea73d27597364c9af683",
		Resolution: pin.Pinned, Workflows: []string{".github/workflows/ci.yml"}, Direct: true,
	}}}
	report := &checks.Report{Workflows: []checks.WorkflowReport{{Path: ".github/workflows/ci.yml"}}}

	var buf bytes.Buffer
	err := renderPinSummary(context.Background(), ui.NewPlain(&buf), record, report, nil, nil, true, false, "v0.0.2", []string{".github/workflows/old.yml"})
	require.NoError(t, err)
	out := buf.String()

	pinned := strings.Index(out, "Pinned 1 action")
	require.NotEqual(t, -1, pinned, out)
	for _, line := range []string{"Upgraded lockfile schema v0.0.2", "Pruned 1 deleted workflow"} {
		i := strings.Index(out, line)
		require.NotEqual(t, -1, i, out)
		assert.Less(t, i, pinned, "%q should precede the summary:\n%s", line, out)
	}
}

// TestRenderPinSummary_NamesRefOnlyRepair: --accept-moved keeps a full-SHA
// key and its commit (e.g. under --no-narrow), so the entry is Verified,
// but its recorded ref changed and the summary must say so.
func TestRenderPinSummary_NamesRefOnlyRepair(t *testing.T) {
	const sha = "11bd71901bbe5b1630ceea73d27597364c9af683"
	record := &pin.Record{Entries: []pin.Entry{{
		NWO: "actions/checkout", Ref: sha, SHA: sha, OnBranch: "main",
		Resolution: pin.Verified, Workflows: []string{".github/workflows/ci.yml"}, Direct: true,
	}}}
	report := &checks.Report{Workflows: []checks.WorkflowReport{{
		Path: ".github/workflows/ci.yml",
		Findings: []checks.Finding{{
			Category: checks.UnreachablePin, Severity: checks.SeverityWarning, RecordedRef: "v4-gone",
			Dependency: &dep.Dependency{NWO: "actions/checkout", Ref: sha, SHA: sha},
		}},
	}}}

	var buf bytes.Buffer
	err := renderPinSummary(context.Background(), ui.NewPlain(&buf), record, report, nil, nil, true, true, "", nil)
	require.NoError(t, err)
	assert.Contains(t, buf.String(), "actions/checkout: recorded ref v4-gone no longer contains 11bd71901bbe; re-recorded as main (--accept-moved)")
}
