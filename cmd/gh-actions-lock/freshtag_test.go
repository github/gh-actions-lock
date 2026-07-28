package main

import (
	"testing"
	"time"

	"github.com/github/gh-actions-lock/internal/pipeline/checks"
)

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
