package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/github/gh-actions-pin/internal/pin"
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
