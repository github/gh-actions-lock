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
