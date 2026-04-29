package lockfile

import (
	"testing"
)

func dep(nwo, ref, sha string) Dependency {
	return Dependency{NWO: nwo, Ref: ref, SHA: sha, HashAlgo: "sha1"}
}

func TestDiffDeps_Unchanged(t *testing.T) {
	old := []Dependency{dep("a/b", "v1", "aaa")}
	new := []Dependency{dep("a/b", "v1", "aaa")}
	d := DiffDeps(old, new)
	if len(d.Unchanged) != 1 || len(d.Changed)+len(d.Rekeyed)+len(d.Added)+len(d.Removed) != 0 {
		t.Fatalf("expected 1 unchanged, got diff=%+v", d)
	}
}

func TestDiffDeps_Changed(t *testing.T) {
	old := []Dependency{dep("a/b", "v1", "aaa")}
	new := []Dependency{dep("a/b", "v1", "bbb")}
	d := DiffDeps(old, new)
	if len(d.Changed) != 1 {
		t.Fatalf("expected 1 changed, got %+v", d)
	}
	if d.Changed[0].Old.SHA != "aaa" || d.Changed[0].New.SHA != "bbb" {
		t.Fatalf("wrong pair: %+v", d.Changed[0])
	}
}

func TestDiffDeps_Rekeyed(t *testing.T) {
	old := []Dependency{dep("a/b", "v1", "aaa")}
	new := []Dependency{dep("a/b", "v2", "bbb")}
	d := DiffDeps(old, new)
	if len(d.Rekeyed) != 1 {
		t.Fatalf("expected 1 rekeyed, got %+v", d)
	}
	if d.Rekeyed[0].Old.Ref != "v1" || d.Rekeyed[0].New.Ref != "v2" {
		t.Fatalf("wrong refs: %+v", d.Rekeyed[0])
	}
}

func TestDiffDeps_RekeyedSameSHA(t *testing.T) {
	// Tag moved but SHA unchanged (ref-only change)
	old := []Dependency{dep("a/b", "v1", "aaa")}
	new := []Dependency{dep("a/b", "v2", "aaa")}
	d := DiffDeps(old, new)
	if len(d.Rekeyed) != 1 {
		t.Fatalf("expected 1 rekeyed, got %+v", d)
	}
}

func TestDiffDeps_AddedRemoved(t *testing.T) {
	old := []Dependency{dep("a/b", "v1", "aaa")}
	new := []Dependency{dep("c/d", "v1", "bbb")}
	d := DiffDeps(old, new)
	if len(d.Added) != 1 || d.Added[0].NWO != "c/d" {
		t.Fatalf("expected c/d added, got %+v", d)
	}
	if len(d.Removed) != 1 || d.Removed[0].NWO != "a/b" {
		t.Fatalf("expected a/b removed, got %+v", d)
	}
}

func TestDiffDeps_MultipleNWO_FuzzyPrefersSameSHA(t *testing.T) {
	// Two old deps with same NWO, two new deps with same NWO.
	// Fuzzy matching should prefer same-SHA pairing.
	old := []Dependency{
		dep("a/b", "v1", "aaa"),
		dep("a/b", "v2", "bbb"),
	}
	new := []Dependency{
		dep("a/b", "v3", "bbb"), // should match old v2 (same SHA)
		dep("a/b", "v4", "ccc"), // should match old v1 (first unhandled)
	}
	d := DiffDeps(old, new)

	if len(d.Rekeyed) != 2 {
		t.Fatalf("expected 2 rekeyed, got %+v", d)
	}
	// v3 should pair with v2 (same SHA bbb)
	if d.Rekeyed[0].Old.Ref != "v2" || d.Rekeyed[0].New.Ref != "v3" {
		t.Errorf("first rekeyed: expected v2→v3, got %s→%s", d.Rekeyed[0].Old.Ref, d.Rekeyed[0].New.Ref)
	}
	// v4 should pair with v1 (remaining)
	if d.Rekeyed[1].Old.Ref != "v1" || d.Rekeyed[1].New.Ref != "v4" {
		t.Errorf("second rekeyed: expected v1→v4, got %s→%s", d.Rekeyed[1].Old.Ref, d.Rekeyed[1].New.Ref)
	}
}

func TestDiffDeps_MixedScenario(t *testing.T) {
	old := []Dependency{
		dep("a/b", "v1", "aaa"), // unchanged
		dep("c/d", "v1", "ccc"), // SHA changes
		dep("e/f", "v1", "eee"), // rekeyed to v2
		dep("g/h", "v1", "ggg"), // removed
	}
	new := []Dependency{
		dep("a/b", "v1", "aaa"), // unchanged
		dep("c/d", "v1", "ddd"), // changed
		dep("e/f", "v2", "fff"), // rekeyed
		dep("x/y", "v1", "xxx"), // added
	}
	d := DiffDeps(old, new)

	if len(d.Unchanged) != 1 || d.Unchanged[0].NWO != "a/b" {
		t.Errorf("unchanged: %+v", d.Unchanged)
	}
	if len(d.Changed) != 1 || d.Changed[0].Old.NWO != "c/d" {
		t.Errorf("changed: %+v", d.Changed)
	}
	if len(d.Rekeyed) != 1 || d.Rekeyed[0].Old.NWO != "e/f" {
		t.Errorf("rekeyed: %+v", d.Rekeyed)
	}
	if len(d.Added) != 1 || d.Added[0].NWO != "x/y" {
		t.Errorf("added: %+v", d.Added)
	}
	if len(d.Removed) != 1 || d.Removed[0].NWO != "g/h" {
		t.Errorf("removed: %+v", d.Removed)
	}
}

func TestDiffDeps_CaseInsensitiveSHA(t *testing.T) {
	old := []Dependency{dep("a/b", "v1", "AABBCC")}
	new := []Dependency{dep("a/b", "v1", "aabbcc")}
	d := DiffDeps(old, new)
	if len(d.Unchanged) != 1 {
		t.Fatalf("expected case-insensitive SHA match, got %+v", d)
	}
}

func TestDiffDeps_Empty(t *testing.T) {
	d := DiffDeps(nil, nil)
	if len(d.Changed)+len(d.Rekeyed)+len(d.Added)+len(d.Removed)+len(d.Unchanged) != 0 {
		t.Fatalf("expected empty diff, got %+v", d)
	}
}

func TestPreserveRefs(t *testing.T) {
	old := []Dependency{
		dep("a/b", "v1.0.0", "aaa"),
		dep("c/d", "v2.0.0", "ccc"),
	}
	new := []Dependency{
		dep("a/b", "aaa", "aaa"),    // bare SHA ref, same SHA → should restore v1.0.0
		dep("c/d", "v2.0.0", "ddd"), // different SHA → keep new ref
	}

	result := PreserveRefs(old, new)
	if result[0].Ref != "v1.0.0" {
		t.Errorf("expected preserved ref v1.0.0, got %s", result[0].Ref)
	}
	if result[1].Ref != "v2.0.0" {
		t.Errorf("expected kept ref v2.0.0, got %s", result[1].Ref)
	}
}

func TestPreserveRefs_NoMutation(t *testing.T) {
	old := []Dependency{dep("a/b", "v1", "aaa")}
	new := []Dependency{dep("a/b", "aaa", "aaa")}

	// Ensure original slice is not mutated.
	result := PreserveRefs(old, new)
	if result[0].Ref != "v1" {
		t.Fatal("result not preserved")
	}
	if new[0].Ref != "aaa" {
		t.Fatal("original new slice was mutated")
	}
}
