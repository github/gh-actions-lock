package resolve

import (
	"testing"
)

func TestDefaultMaxRecursionDepth(t *testing.T) {
	if DefaultMaxRecursionDepth != 10 {
		t.Fatalf("expected 10, got %d", DefaultMaxRecursionDepth)
	}
}

func TestResolvedEntry(t *testing.T) {
	e := resolvedEntry{actionYML: "name: test"}
	if e.actionYML != "name: test" {
		t.Fatalf("unexpected actionYML: %s", e.actionYML)
	}
}

func TestTagPeel(t *testing.T) {
	t.Run("tag object", func(t *testing.T) {
		tp := tagPeel{commit: "abc", isTag: true}
		if !tp.isTag || tp.commit != "abc" {
			t.Fatal("expected tag peel with commit abc")
		}
	})

	t.Run("non-tag", func(t *testing.T) {
		tp := tagPeel{}
		if tp.isTag || tp.commit != "" {
			t.Fatal("expected zero-value non-tag peel")
		}
	})
}
