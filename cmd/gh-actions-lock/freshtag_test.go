package main

import (
	"context"
	"testing"
	"time"

	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/github/gh-actions-lock/internal/pin"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/github/gh-actions-lock/internal/tag"
)

// TestInjectFreshTagFindings_PositiveCooldownSkips proves the per-entry gate:
// an action with a positive effective cooldown is not nudged, because the
// resolver's cooldown filter already gated fresh tags for it.
func TestInjectFreshTagFindings_PositiveCooldownSkips(t *testing.T) {
	fresh := time.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", `(?i)repos/actions/checkout/releases`),
		httpmock.JSONResponse([]map[string]any{
			{"tag_name": "v4", "published_at": fresh, "immutable": false},
		}),
	)
	tagger := tag.NewListerForTest(t, reg)

	record := &pin.Record{Entries: []pin.Entry{
		{NWO: "actions/checkout", Ref: "v4", SHA: "s1", Resolution: pin.Pinned},
	}}
	report := &checks.Report{}
	cfg := tag.CooldownConfig{RepoOverrides: map[string]int{"actions/checkout": 7}}
	injectFreshTagFindings(context.Background(), report, record, tagger, cfg)

	if len(report.RepoFindings) != 0 {
		t.Fatalf("RepoFindings = %d, want 0 (positive cooldown skips the nudge)", len(report.RepoFindings))
	}
}

func TestFreshTagFinding(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	iso := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }

	tests := []struct {
		name   string
		iso    string
		wantOK bool
	}{
		{"fresh", iso(24 * time.Hour), true},
		{"just under window", iso(freshTagWindow - time.Hour), true},
		{"exactly at window", iso(freshTagWindow), false},
		{"old", iso(30 * 24 * time.Hour), false},
		{"empty", "", false},
		{"unparseable", "not-a-date", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, ok := freshTagFinding("actions/checkout", "v4.2.1", tt.iso, now)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok {
				if f.Category != checks.FreshTag || f.Severity != checks.SeverityInfo {
					t.Errorf("finding category/severity = %s/%s", f.Category, f.Severity)
				}
			}
		})
	}
}

func TestAppendCooldownConfigFindings(t *testing.T) {
	report := &checks.Report{}
	appendCooldownConfigFindings(report, []string{"a", "b"})
	if len(report.RepoFindings) != 2 {
		t.Fatalf("RepoFindings = %d, want 2", len(report.RepoFindings))
	}
	if report.RepoFindings[0].Category != checks.CooldownConfigIgnored {
		t.Errorf("category = %s, want cooldown-config-ignored", report.RepoFindings[0].Category)
	}
	appendCooldownConfigFindings(report, nil)
	if len(report.RepoFindings) != 2 {
		t.Errorf("nil warnings must add nothing; got %d", len(report.RepoFindings))
	}
}

// TestInjectFreshTagFindings covers the resolution gate: a Pinned entry (an
// initial pin or an update that changed the SHA) fires the fresh-tag nudge,
// while a Verified entry (unchanged re-run) stays quiet.
func TestInjectFreshTagFindings(t *testing.T) {
	fresh := time.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", `(?i)repos/actions/checkout/tags`),
		httpmock.JSONResponse([]map[string]any{
			{"name": "v4", "commit": map[string]any{"sha": "abc123"}},
		}),
	)
	reg.Register(
		httpmock.REST("GET", `(?i)repos/actions/checkout/releases`),
		httpmock.JSONResponse([]map[string]any{
			{"tag_name": "v4", "published_at": fresh, "immutable": false},
		}),
	)
	reg.Register(
		httpmock.REST("GET", `(?i)repos/actions/checkout$`),
		httpmock.JSONResponse(map[string]any{
			"default_branch": "main", "visibility": "public",
			"pushed_at": "2024-01-01T00:00:00Z",
		}),
	)
	tagger := tag.NewListerForTest(t, reg)

	record := &pin.Record{Entries: []pin.Entry{
		{NWO: "actions/checkout", Ref: "v4", SHA: "s1", Resolution: pin.Pinned},
		{NWO: "actions/setup-go", Ref: "v5", SHA: "s2", Resolution: pin.Verified},
	}}
	report := &checks.Report{}
	injectFreshTagFindings(context.Background(), report, record, tagger, tag.CooldownConfig{})

	if len(report.RepoFindings) != 1 {
		t.Fatalf("RepoFindings = %d, want 1 (Pinned fires, Verified quiet)", len(report.RepoFindings))
	}
	if got := report.RepoFindings[0]; got.Category != checks.FreshTag {
		t.Errorf("category = %s, want fresh-tag", got.Category)
	}
}
