package discover

import (
	"context"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-pin/internal/dep"
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

func TestPickUpgrade(t *testing.T) {
	tests := []struct {
		name      string
		current   string
		tags      []ghapi.RepoTag
		wantRef   string
		wantSHA   string
		wantPrec  precision
		wantOffer bool
	}{
		{
			name:      "major float offers highest major-form tag",
			current:   "v5",
			tags:      checkoutTags(),
			wantRef:   "v6",
			wantSHA:   "sha-v6",
			wantPrec:  precMajor,
			wantOffer: true,
		},
		{
			name:      "full pin offers highest full tag, not the major float",
			current:   "v5.1.0",
			tags:      checkoutTags(),
			wantRef:   "v6.1.2",
			wantSHA:   "sha-v612",
			wantPrec:  precFull,
			wantOffer: true,
		},
		{
			name:      "minor pin offers highest minor-form tag",
			current:   "v5.1",
			tags:      []ghapi.RepoTag{tag("v5.1", "a"), tag("v6.1", "b"), tag("v6.1.2", "c")},
			wantRef:   "v6.1",
			wantSHA:   "b",
			wantPrec:  precMinor,
			wantOffer: true,
		},
		{
			name:      "cross-major is allowed",
			current:   "v5",
			tags:      []ghapi.RepoTag{tag("v5", "a"), tag("v6", "b")},
			wantRef:   "v6",
			wantSHA:   "b",
			wantPrec:  precMajor,
			wantOffer: true,
		},
		{
			name:      "already newest is a no-op",
			current:   "v6",
			tags:      checkoutTags(),
			wantOffer: false,
		},
		{
			name:      "full already newest is a no-op",
			current:   "v6.1.2",
			tags:      checkoutTags(),
			wantOffer: false,
		},
		{
			name:      "prereleases dropped when current is stable",
			current:   "v5",
			tags:      []ghapi.RepoTag{tag("v5", "a"), tag("v6.0.0-beta.1", "b")},
			wantOffer: false,
		},
		{
			name:      "prereleases kept when current is a prerelease",
			current:   "v5.0.0-beta.1",
			tags:      []ghapi.RepoTag{tag("v5.0.0-beta.1", "a"), tag("v5.0.0-beta.2", "b")},
			wantRef:   "v5.0.0-beta.2",
			wantSHA:   "b",
			wantPrec:  precFull,
			wantOffer: true,
		},
		{
			name:      "non-semver candidates are ignored",
			current:   "v5",
			tags:      []ghapi.RepoTag{tag("v5", "a"), tag("latest", "b"), tag("nightly", "c"), tag("v6", "d")},
			wantRef:   "v6",
			wantSHA:   "d",
			wantPrec:  precMajor,
			wantOffer: true,
		},
		{
			name:      "lower-precision fallback: full current, only a higher major tag",
			current:   "v5.1.0",
			tags:      []ghapi.RepoTag{tag("v5.1.0", "a"), tag("v6", "b")},
			wantRef:   "v6",
			wantSHA:   "b",
			wantPrec:  precMajor,
			wantOffer: true,
		},
		{
			name:      "no fallback past major: major current with only full higher tags",
			current:   "v5",
			tags:      []ghapi.RepoTag{tag("v5", "a"), tag("v5.3.1", "b")},
			wantOffer: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cur, ok := parserlock.ParseSemVer(tt.current)
			if !ok {
				t.Fatalf("test ref %q did not parse as semver", tt.current)
			}
			gotTag, gotPrec, gotOffer := pickUpgrade(cur, tt.tags)
			if gotOffer != tt.wantOffer {
				t.Fatalf("offer = %v, want %v (tag=%+v)", gotOffer, tt.wantOffer, gotTag)
			}
			if !tt.wantOffer {
				return
			}
			if gotTag.Name != tt.wantRef {
				t.Errorf("ref = %q, want %q", gotTag.Name, tt.wantRef)
			}
			if gotTag.SHA != tt.wantSHA {
				t.Errorf("sha = %q, want %q", gotTag.SHA, tt.wantSHA)
			}
			if gotPrec != tt.wantPrec {
				t.Errorf("precision = %q, want %q", gotPrec, tt.wantPrec)
			}
		})
	}
}

// fakeLister serves canned tags keyed by owner/repo.
type fakeLister struct {
	byRepo map[string][]ghapi.RepoTag
	calls  int
}

func (f *fakeLister) RepoTags(_ context.Context, owner, repo string) ([]ghapi.RepoTag, error) {
	f.calls++
	return f.byRepo[owner+"/"+repo], nil
}

func TestDiscover(t *testing.T) {
	deps := []dep.Dependency{
		{NWO: "actions/checkout", Ref: "v5", SHA: "sha-v5"},
		{NWO: "actions/setup-go", Ref: "v5.1.0", SHA: "sha-sg510"},
		{NWO: "some/branch-pinned", Ref: "main", SHA: "sha-main"}, // skipped: not semver
		{NWO: "actions/checkout", Ref: "v5", SHA: "sha-v5"},       // duplicate: deduped
	}
	lister := &fakeLister{byRepo: map[string][]ghapi.RepoTag{
		"actions/checkout": checkoutTags(),
		"actions/setup-go": {tag("v5.1.0", "sha-sg510"), tag("v5.2.0", "sha-sg520")},
	}}

	offers, err := Discover(context.Background(), deps, lister)
	if err != nil {
		t.Fatal(err)
	}
	if len(offers) != 2 {
		t.Fatalf("expected 2 offers, got %d: %+v", len(offers), offers)
	}
	// Sorted by NWO: checkout before setup-go.
	if offers[0].NWO != "actions/checkout" || offers[0].AvailableRef != "v6" || offers[0].Precision != "major" {
		t.Errorf("offer[0] = %+v", offers[0])
	}
	if offers[0].CurrentSHA != "sha-v5" {
		t.Errorf("offer[0] current sha = %q", offers[0].CurrentSHA)
	}
	if offers[1].NWO != "actions/setup-go" || offers[1].AvailableRef != "v5.2.0" || offers[1].Precision != "full" {
		t.Errorf("offer[1] = %+v", offers[1])
	}
	if offers[1].AvailableSHA != "sha-sg520" {
		t.Errorf("offer[1] available sha = %q", offers[1].AvailableSHA)
	}
}

func TestDiscover_NoOffersWhenCurrent(t *testing.T) {
	deps := []dep.Dependency{{NWO: "actions/checkout", Ref: "v6", SHA: "sha-v6"}}
	lister := &fakeLister{byRepo: map[string][]ghapi.RepoTag{"actions/checkout": checkoutTags()}}
	offers, err := Discover(context.Background(), deps, lister)
	if err != nil {
		t.Fatal(err)
	}
	if len(offers) != 0 {
		t.Fatalf("expected no offers, got %+v", offers)
	}
}
