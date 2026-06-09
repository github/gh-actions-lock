package discover

import (
	"context"
	"errors"
	"testing"

	"github.com/github/gh-actions-pin/internal/ghapi"
)

func tag(name, sha string) ghapi.RepoTag { return ghapi.RepoTag{Name: name, SHA: sha} }

// checkoutTags is a realistic mixed set: floating major tags alongside full
// patch tags, as actions/checkout actually publishes.
func checkoutTags() []ghapi.RepoTag {
	return []ghapi.RepoTag{
		tag("v4", "sha-v4"),
		tag("v4.2.2", "sha-v422"),
		tag("v5", "sha-v5"),
		tag("v5.1.0", "sha-v510"),
		tag("v6", "sha-v6"),
		tag("v6.1.2", "sha-v612"),
	}
}

// fakeLister serves canned tags keyed by owner/repo.
type fakeLister struct {
	byRepo map[string][]ghapi.RepoTag
	err    error
}

func (f *fakeLister) RepoTags(_ context.Context, owner, repo string) ([]ghapi.RepoTag, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.byRepo[owner+"/"+repo], nil
}

func refs(cands []Candidate) []string {
	out := make([]string, len(cands))
	for i, c := range cands {
		out[i] = c.Ref
	}
	return out
}

func TestCandidates(t *testing.T) {
	tests := []struct {
		name     string
		current  string
		tags     []ghapi.RepoTag
		wantRefs []string
	}{
		{
			name:    "semver current: only strictly-greater, semver-desc",
			current: "v5",
			tags:    checkoutTags(),
			// v4/v4.2.2/v5 dropped (<= 5.0.0); v5.1.0 is newer than the v5
			// float (5.1.0 > 5.0.0); v6.x ranks highest.
			wantRefs: []string{"v6.1.2", "v6", "v5.1.0"},
		},
		{
			name:     "full-precision current still offers higher fulls and floats",
			current:  "v5.1.0",
			tags:     checkoutTags(),
			wantRefs: []string{"v6.1.2", "v6"},
		},
		{
			name:     "newest current: nothing newer",
			current:  "v6.1.2",
			tags:     checkoutTags(),
			wantRefs: []string{},
		},
		{
			name:    "mixed major and full both offered, desc",
			current: "v4",
			tags:    checkoutTags(),
			// v6.1.2 > v6 > v5.1.0 > v5 > v4.2.2 (v4 itself excluded).
			wantRefs: []string{"v6.1.2", "v6", "v5.1.0", "v5", "v4.2.2"},
		},
		{
			name:    "prereleases dropped when current is stable",
			current: "v1",
			tags: []ghapi.RepoTag{
				tag("v1", "sha-v1"),
				tag("v2.0.0-beta.1", "sha-beta"),
				tag("v2.0.0", "sha-v200"),
			},
			wantRefs: []string{"v2.0.0"},
		},
		{
			name:    "prereleases kept when current is a prerelease",
			current: "v2.0.0-beta.1",
			tags: []ghapi.RepoTag{
				tag("v2.0.0-beta.1", "sha-b1"),
				tag("v2.0.0-beta.2", "sha-b2"),
				tag("v2.0.0", "sha-v200"),
			},
			wantRefs: []string{"v2.0.0", "v2.0.0-beta.2"},
		},
		{
			name:    "non-version current: every tag offered, semver-desc then release order",
			current: "main",
			tags: []ghapi.RepoTag{
				tag("v1.0.0", "sha-v100"),
				tag("nightly", "sha-nightly"),
				tag("v2.0.0", "sha-v200"),
				tag("latest", "sha-latest"),
			},
			// semver desc first (v2 > v1), then non-semver in listing order.
			wantRefs: []string{"v2.0.0", "v1.0.0", "nightly", "latest"},
		},
		{
			name:    "non-semver tags appended after semver upgrades",
			current: "v1.0.0",
			tags: []ghapi.RepoTag{
				tag("v1.0.0", "sha-v100"),
				tag("v2.0.0", "sha-v200"),
				tag("stable", "sha-stable"),
			},
			wantRefs: []string{"v2.0.0", "stable"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lister := &fakeLister{byRepo: map[string][]ghapi.RepoTag{
				"actions/checkout": tt.tags,
			}}
			got, err := Candidates(context.Background(), "actions/checkout", tt.current, lister)
			if err != nil {
				t.Fatalf("Candidates: %v", err)
			}
			gotRefs := refs(got)
			if len(gotRefs) != len(tt.wantRefs) {
				t.Fatalf("refs = %v, want %v", gotRefs, tt.wantRefs)
			}
			for i := range gotRefs {
				if gotRefs[i] != tt.wantRefs[i] {
					t.Fatalf("refs = %v, want %v", gotRefs, tt.wantRefs)
				}
			}
		})
	}
}

func TestCandidates_CarriesSHA(t *testing.T) {
	lister := &fakeLister{byRepo: map[string][]ghapi.RepoTag{
		"actions/checkout": checkoutTags(),
	}}
	got, err := Candidates(context.Background(), "actions/checkout", "v5", lister)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("expected candidates")
	}
	if got[0].Ref != "v6.1.2" || got[0].SHA != "sha-v612" {
		t.Errorf("top candidate = %+v, want {v6.1.2 sha-v612}", got[0])
	}
}

func TestCandidates_InvalidNWO(t *testing.T) {
	lister := &fakeLister{}
	if _, err := Candidates(context.Background(), "checkout", "v5", lister); err == nil {
		t.Fatal("expected error for nwo without owner/repo")
	}
}

func TestCandidates_ListerError(t *testing.T) {
	lister := &fakeLister{err: errors.New("boom")}
	if _, err := Candidates(context.Background(), "actions/checkout", "v5", lister); err == nil {
		t.Fatal("expected listing error to propagate")
	}
}
