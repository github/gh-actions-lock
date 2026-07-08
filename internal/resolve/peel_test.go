package resolve

import (
	"context"
	"testing"

	"github.com/github/gh-actions-lock/internal/ghapi"
)

func TestPeelTagObject_CachesPositive(t *testing.T) {
	r := &Resolver{}
	r.tagObjectCache.Put(
		ghapi.ForNWOSha("actions", "checkout", "tagsha"),
		tagPeel{commit: "commitsha", isTag: true},
	)

	commit, ok := r.PeelTagObject(context.Background(), "actions", "checkout", "tagsha")
	if !ok {
		t.Fatal("expected ok=true for cached tag object")
	}
	if commit != "commitsha" {
		t.Fatalf("expected commitsha, got %q", commit)
	}
}

func TestPeelTagObject_CachesNegative(t *testing.T) {
	r := &Resolver{}
	r.tagObjectCache.Put(
		ghapi.ForNWOSha("actions", "checkout", "commitsha"),
		tagPeel{},
	)

	commit, ok := r.PeelTagObject(context.Background(), "actions", "checkout", "commitsha")
	if ok {
		t.Fatal("expected ok=false for cached non-tag")
	}
	if commit != "" {
		t.Fatalf("expected empty commit, got %q", commit)
	}
}

func TestIsKnownTagObject(t *testing.T) {
	r := &Resolver{}

	// Not cached — should return false.
	if r.IsKnownTagObject("actions", "checkout", "unknown") {
		t.Fatal("expected false for uncached SHA")
	}

	// Cached as tag — should return true.
	r.tagObjectCache.Put(
		ghapi.ForNWOSha("actions", "checkout", "tagsha"),
		tagPeel{commit: "c", isTag: true},
	)
	if !r.IsKnownTagObject("actions", "checkout", "tagsha") {
		t.Fatal("expected true for cached tag object")
	}

	// Cached as non-tag — should return false.
	r.tagObjectCache.Put(
		ghapi.ForNWOSha("actions", "checkout", "commitsha"),
		tagPeel{},
	)
	if r.IsKnownTagObject("actions", "checkout", "commitsha") {
		t.Fatal("expected false for cached non-tag")
	}
}
