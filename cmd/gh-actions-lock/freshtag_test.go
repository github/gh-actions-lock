package main

import (
	"context"
	"testing"
	"time"

	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/github/gh-actions-lock/internal/pin"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/github/gh-actions-lock/internal/tag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInjectFreshTagFindings_Cooldown proves a configured cooldown widens the
// freshness window and changes the wording instead of silencing the nudge.
func TestInjectFreshTagFindings_Cooldown(t *testing.T) {
	tests := []struct {
		name       string
		releasedAt time.Duration
		cfg        tag.CooldownConfig
		wantDetail string // empty means no finding
	}{
		{
			name:       "fresh tag within configured cooldown warns",
			releasedAt: 5 * 24 * time.Hour,
			cfg:        tag.CooldownConfig{RepoOverrides: map[string]int{"actions/checkout": 7}},
			wantDetail: "newer than your configured cooldown (7 days)",
		},
		{
			name:       "tag older than configured cooldown is quiet",
			releasedAt: 10 * 24 * time.Hour,
			cfg:        tag.CooldownConfig{DefaultDays: 7},
		},
		{
			name:       "no cooldown keeps default window and wording",
			releasedAt: 24 * time.Hour,
			cfg:        tag.CooldownConfig{},
			wantDetail: "no Dependabot cooldown configured",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := &httpmock.Registry{}
			reg.Register(
				httpmock.REST("GET", `(?i)repos/actions/checkout/tags`),
				httpmock.JSONResponse([]map[string]any{
					{"name": "v4", "commit": map[string]any{"sha": "s1"}},
				}),
			)
			reg.Register(
				httpmock.REST("GET", `(?i)repos/actions/checkout/releases`),
				httpmock.JSONResponse([]map[string]any{
					{"tag_name": "v4", "published_at": time.Now().Add(-tt.releasedAt).Format(time.RFC3339), "immutable": false},
				}),
			)
			tagger := tag.NewListerForTest(t, reg)
			record := &pin.Record{Entries: []pin.Entry{
				{NWO: "actions/checkout", Ref: "v4", SHA: "s1", Resolution: pin.Pinned},
			}}
			report := &checks.Report{}
			injectFreshTagFindings(context.Background(), report, record, tagger, tt.cfg)

			if tt.wantDetail == "" {
				assert.Empty(t, report.RepoFindings)
				return
			}
			require.Len(t, report.RepoFindings, 1)
			f := report.RepoFindings[0]
			assert.Equal(t, checks.FreshTag, f.Category)
			assert.Equal(t, checks.SeverityInfo, f.Severity)
			assert.Contains(t, f.Detail, tt.wantDetail)
		})
	}
}

func TestFreshTagFinding(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	iso := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }

	tests := []struct {
		name     string
		iso      string
		cooldown int
		wantOK   bool
		wantAge  string
	}{
		{"fresh", iso(24 * time.Hour), 0, true, "1d ago"},
		{"just under window", iso(freshTagWindow - time.Hour), 0, true, "2d ago"},
		{"exactly at window", iso(freshTagWindow), 0, false, ""},
		{"old", iso(30 * 24 * time.Hour), 0, false, ""},
		{"empty", "", 0, false, ""},
		{"unparseable", "not-a-date", 0, false, ""},
		{"just under cooldown", iso(10*24*time.Hour - time.Minute), 10, true, "9d ago"},
		{"exactly at cooldown", iso(10 * 24 * time.Hour), 10, false, ""},
		{"cooldown shorter than default window", iso(2 * 24 * time.Hour), 1, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, ok := freshTagFinding("actions/checkout", "v4.2.1", tt.iso, tt.cooldown, now)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok {
				if f.Category != checks.FreshTag || f.Severity != checks.SeverityInfo {
					t.Errorf("finding category/severity = %s/%s", f.Category, f.Severity)
				}
				assert.Contains(t, f.Detail, "released "+tt.wantAge)
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

func TestInjectFreshTagFindings_PrefixedTag(t *testing.T) {
	fresh := time.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	reg := &httpmock.Registry{}
	reg.Register(httpmock.REST("GET", `(?i)repos/octo/tools/tags`),
		httpmock.JSONResponse(httpmock.TagListResponse("verify/v0.5.1", "abc123")))
	reg.Register(httpmock.REST("GET", `(?i)repos/octo/tools/releases`),
		httpmock.JSONResponse([]map[string]any{{"tag_name": "verify/v0.5.1", "published_at": fresh}}))
	tagger := tag.NewListerForTest(t, reg)

	record := &pin.Record{Entries: []pin.Entry{
		{NWO: "octo/tools", Ref: "verify/v0.5.1", SHA: "abc123", Resolution: pin.Pinned},
	}}
	report := &checks.Report{}
	injectFreshTagFindings(context.Background(), report, record, tagger, tag.CooldownConfig{})

	if len(report.RepoFindings) != 1 || report.RepoFindings[0].Category != checks.FreshTag {
		t.Fatalf("RepoFindings = %+v, want one fresh-tag finding", report.RepoFindings)
	}
}
