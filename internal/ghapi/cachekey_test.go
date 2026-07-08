package ghapi

import "testing"

func TestRepoNormalizesCase(t *testing.T) {
	a := ForRepo("Actions", "Checkout")
	b := ForRepo("actions", "checkout")
	if a != b {
		t.Fatalf("expected Repo to be case-insensitive: %v vs %v", a, b)
	}
	if got := a.String(); got != "actions/checkout" {
		t.Fatalf("Repo.String() = %q, want %q", got, "actions/checkout")
	}
}

func TestNWORefPreservesRefCase(t *testing.T) {
	a := ForNWORef("Actions", "Checkout", "Main")
	b := ForNWORef("actions", "checkout", "Main")
	c := ForNWORef("actions", "checkout", "main")
	if a != b {
		t.Fatalf("expected owner/repo case to fold: %v vs %v", a, b)
	}
	if a == c {
		t.Fatalf("expected ref case to preserve: %v vs %v", a, c)
	}
	if got := a.String(); got != "actions/checkout@Main" {
		t.Fatalf("NWORef.String() = %q", got)
	}
}

func TestNWOShaLowercasesSha(t *testing.T) {
	a := ForNWOSha("actions", "checkout", "ABCDEF")
	b := ForNWOSha("actions", "checkout", "abcdef")
	if a != b {
		t.Fatalf("expected SHA case to fold: %v vs %v", a, b)
	}
	if got := a.String(); got != "actions/checkout|abcdef" {
		t.Fatalf("NWOSha.String() = %q", got)
	}
}

func TestCompareLowercasesBothShas(t *testing.T) {
	a := ForCompare("o", "r", "AAA", "BBB")
	b := ForCompare("o", "r", "aaa", "bbb")
	if a != b {
		t.Fatalf("expected SHAs to fold: %v vs %v", a, b)
	}
	// Asymmetry: base and head are not interchangeable.
	c := ForCompare("o", "r", "bbb", "aaa")
	if a == c {
		t.Fatalf("expected base/head to be ordered: %v vs %v", a, c)
	}
}

func TestActionRefDistinguishesPath(t *testing.T) {
	plain := ForActionRef("actions", "cache", "", "v4")
	subpath := ForActionRef("actions", "cache", "save", "v4")
	if plain == subpath {
		t.Fatalf("expected sub-action path to distinguish: %v vs %v", plain, subpath)
	}
	if got := plain.String(); got != "actions/cache@v4" {
		t.Fatalf("ActionRef.String() pathless = %q", got)
	}
	if got := subpath.String(); got != "actions/cache/save@v4" {
		t.Fatalf("ActionRef.String() with path = %q", got)
	}
}

func TestKeysUsableAsMapKeys(t *testing.T) {
	m := map[NWORef]int{}
	m[ForNWORef("actions", "checkout", "v4")] = 1
	if got := m[ForNWORef("Actions", "Checkout", "v4")]; got != 1 {
		t.Fatalf("expected case-insensitive map lookup, got %d", got)
	}
}
