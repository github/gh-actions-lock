package pin

import (
	"testing"

	"github.com/github/gh-actions-pin/internal/dep"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func keySet(deps []dep.Dependency) map[string]dep.Dependency {
	m := make(map[string]dep.Dependency, len(deps))
	for _, d := range deps {
		m[d.Key()] = d
	}
	return m
}

func TestSpliceClosure_PreservesUntargetedAndSwapsTarget(t *testing.T) {
	existing := []dep.Dependency{
		{NWO: "actions/checkout", Ref: "v4", SHA: "old1", Branch: "main"},
		{NWO: "actions/setup-go", Ref: "v6", SHA: "sg", Branch: "main"},
		{NWO: "helper/only", Ref: "v1", SHA: "hp", Branch: "main"},
	}
	existingPM := map[string][]string{
		"helper/only@v1": {"actions/setup-go@v6"},
	}
	existingDirect := map[string]bool{
		"actions/checkout@v4": true,
		"actions/setup-go@v6": true,
	}
	// Target resolves to just itself (no new transitives).
	targetDeps := []dep.Dependency{
		{NWO: "actions/checkout", Ref: "v6", SHA: "new1", Branch: "main"},
	}

	closure, pm, direct := spliceClosure(existing, existingPM, existingDirect, targetDeps, nil, "actions/checkout", "v6")

	got := keySet(closure)
	require.Contains(t, got, "actions/checkout@v6", "new target must be present")
	assert.Equal(t, "new1", got["actions/checkout@v6"].SHA)
	require.NotContains(t, got, "actions/checkout@v4", "old target ref must be dropped from closure")

	// Untargeted deps preserved verbatim (SHA/branch unchanged).
	require.Contains(t, got, "actions/setup-go@v6")
	assert.Equal(t, "sg", got["actions/setup-go@v6"].SHA)
	require.Contains(t, got, "helper/only@v1")
	assert.Equal(t, "hp", got["helper/only@v1"].SHA)

	// Direct set swaps old target ref for new.
	assert.True(t, direct["actions/checkout@v6"])
	assert.False(t, direct["actions/checkout@v4"])
	assert.True(t, direct["actions/setup-go@v6"])

	// Untargeted transitive parent links survive.
	assert.Equal(t, []string{"actions/setup-go@v6"}, pm["helper/only@v1"])
}

func TestSpliceClosure_AddsNewTransitive(t *testing.T) {
	existing := []dep.Dependency{
		{NWO: "actions/checkout", Ref: "v4", SHA: "old1", Branch: "main"},
	}
	existingDirect := map[string]bool{"actions/checkout@v4": true}
	targetDeps := []dep.Dependency{
		{NWO: "actions/checkout", Ref: "v6", SHA: "new1", Branch: "main"},
		{NWO: "vendor/dep", Ref: "v2", SHA: "vd", Branch: "main"},
	}
	targetPM := dep.ParentMap{"vendor/dep@v2": {"actions/checkout@v6"}}

	closure, pm, direct := spliceClosure(existing, nil, existingDirect, targetDeps, targetPM, "actions/checkout", "v6")

	got := keySet(closure)
	require.Contains(t, got, "actions/checkout@v6")
	require.Contains(t, got, "vendor/dep@v2", "genuinely-new transitive must be added")
	assert.True(t, direct["actions/checkout@v6"])
	assert.False(t, direct["vendor/dep@v2"], "a transitive is not a direct pin")
	assert.Equal(t, []string{"actions/checkout@v6"}, pm["vendor/dep@v2"])
}

func TestSpliceClosure_SHAMoveSameRef(t *testing.T) {
	// Re-pinning the same ref to a new SHA: the target dep overwrites the
	// existing entry, and no direct pin is lost.
	existing := []dep.Dependency{
		{NWO: "actions/checkout", Ref: "v6", SHA: "old", Branch: "main"},
	}
	existingDirect := map[string]bool{"actions/checkout@v6": true}
	targetDeps := []dep.Dependency{
		{NWO: "actions/checkout", Ref: "v6", SHA: "moved", Branch: "main"},
	}

	closure, _, direct := spliceClosure(existing, nil, existingDirect, targetDeps, nil, "actions/checkout", "v6")
	got := keySet(closure)
	require.Contains(t, got, "actions/checkout@v6")
	assert.Equal(t, "moved", got["actions/checkout@v6"].SHA, "target dep wins, carrying the new SHA")
	assert.True(t, direct["actions/checkout@v6"])
}

func TestSpliceClosure_DropsAllOldDirectRefsOfTarget(t *testing.T) {
	// Lockfile somehow records the target at two refs as direct pins; both
	// non-newRef entries are dropped.
	existing := []dep.Dependency{
		{NWO: "actions/checkout", Ref: "v4", SHA: "a", Branch: "main"},
		{NWO: "actions/checkout", Ref: "v5", SHA: "b", Branch: "main"},
	}
	existingDirect := map[string]bool{
		"actions/checkout@v4": true,
		"actions/checkout@v5": true,
	}
	targetDeps := []dep.Dependency{
		{NWO: "actions/checkout", Ref: "v6", SHA: "c", Branch: "main"},
	}

	closure, _, direct := spliceClosure(existing, nil, existingDirect, targetDeps, nil, "actions/checkout", "v6")
	got := keySet(closure)
	require.Contains(t, got, "actions/checkout@v6")
	assert.NotContains(t, got, "actions/checkout@v4")
	assert.NotContains(t, got, "actions/checkout@v5")
	assert.True(t, direct["actions/checkout@v6"])
	assert.False(t, direct["actions/checkout@v4"])
	assert.False(t, direct["actions/checkout@v5"])
}

func TestSpliceClosure_SharedTransitiveExistingPinWins(t *testing.T) {
	// A shared transitive already pinned at sha-old (reached via an untargeted
	// action). The bumped target re-resolves the SAME ref to sha-new (a moved
	// tag). The existing pin must win: a lockfile pins a moving ref to a stable
	// SHA, and re-pinning here would churn an untargeted dependency and split
	// one ref across two SHAs.
	existing := []dep.Dependency{
		{NWO: "actions/checkout", Ref: "v4", SHA: "old1", Branch: "main"},
		{NWO: "actions/setup-go", Ref: "v6", SHA: "sg", Branch: "main"},
		{NWO: "shared/dep", Ref: "v1", SHA: "shaold", Branch: "main"},
	}
	existingPM := map[string][]string{
		"shared/dep@v1": {"actions/setup-go@v6"},
	}
	existingDirect := map[string]bool{
		"actions/checkout@v4": true,
		"actions/setup-go@v6": true,
	}
	targetDeps := []dep.Dependency{
		{NWO: "actions/checkout", Ref: "v6", SHA: "new1", Branch: "main"},
		{NWO: "shared/dep", Ref: "v1", SHA: "shanew", Branch: "main"},
	}
	targetPM := dep.ParentMap{"shared/dep@v1": {"actions/checkout@v6"}}

	closure, pm, _ := spliceClosure(existing, existingPM, existingDirect, targetDeps, targetPM, "actions/checkout", "v6")

	got := keySet(closure)
	require.Contains(t, got, "shared/dep@v1")
	assert.Equal(t, "shaold", got["shared/dep@v1"].SHA, "existing shared-transitive pin wins (stability + minimal diff)")
	// One stable entry for the ref, with both consumers recorded as parents.
	assert.ElementsMatch(t, []string{"actions/setup-go@v6", "actions/checkout@v6"}, pm["shared/dep@v1"])
}

func TestSortChanges_Deterministic(t *testing.T) {
	changes := []ActionChange{
		{NWO: "b/y", OldRef: "v1", NewRef: "v2", NewSHA: "z"},
		{NWO: "a/x", OldRef: "v2", NewRef: "v3", NewSHA: "p"},
		{NWO: "a/x", OldRef: "v1", NewRef: "v3", NewSHA: "q"},
	}
	sortChanges(changes)
	assert.Equal(t, "a/x", changes[0].NWO)
	assert.Equal(t, "v1", changes[0].OldRef)
	assert.Equal(t, "a/x", changes[1].NWO)
	assert.Equal(t, "v2", changes[1].OldRef)
	assert.Equal(t, "b/y", changes[2].NWO)
}

func TestKeyNWO(t *testing.T) {
	assert.Equal(t, "actions/checkout", keyNWO("actions/checkout@v6"))
	assert.Equal(t, "no-at", keyNWO("no-at"))
}

func TestUnionStrings(t *testing.T) {
	got := unionStrings([]string{"a", "b"}, []string{"b", "c"})
	assert.Equal(t, []string{"a", "b", "c"}, got)
}

func TestLooksLikeSHA(t *testing.T) {
	cases := []struct {
		ref  string
		want bool
	}{
		{"de0fac2e4500dabe0009e67214ff5f5447ce83dd", true},
		{"DE0FAC2E4500DABE0009E67214FF5F5447CE83DD", true},
		{"de0fac2e4500dabe0009e67214ff5f5447ce83d", false},   // 39 chars
		{"de0fac2e4500dabe0009e67214ff5f5447ce83ddd", false}, // 41 chars
		{"v6", false},
		{"de0fac2e4500dabe0009e67214ff5f5447ce83dg", false}, // non-hex 'g'
		{"", false},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, looksLikeSHA(c.ref), "looksLikeSHA(%q)", c.ref)
	}
}

func TestVerifyTargetPin(t *testing.T) {
	const newSHA = "11111111111111111111111111111111111111aa"
	edit := func(closure []dep.Dependency, direct map[string]bool) workflowEdit {
		return workflowEdit{path: ".github/workflows/ci.yml", closure: closure, directKeys: direct}
	}

	t.Run("satisfied", func(t *testing.T) {
		e := edit(
			[]dep.Dependency{{NWO: "actions/checkout", Ref: "v6", SHA: newSHA}},
			map[string]bool{"actions/checkout@v6": true},
		)
		_, bad := verifyTargetPin(e, "actions/checkout", "v6", newSHA)
		assert.False(t, bad, "a correct pin must not produce a finding")
	})

	t.Run("not a direct pin", func(t *testing.T) {
		e := edit(
			[]dep.Dependency{{NWO: "actions/checkout", Ref: "v6", SHA: newSHA}},
			map[string]bool{}, // target missing from direct set
		)
		f, bad := verifyTargetPin(e, "actions/checkout", "v6", newSHA)
		require.True(t, bad)
		assert.Equal(t, checksNotPinned, string(f.Category))
	})

	t.Run("stale sha", func(t *testing.T) {
		e := edit(
			[]dep.Dependency{{NWO: "actions/checkout", Ref: "v6", SHA: "wrongsha"}},
			map[string]bool{"actions/checkout@v6": true},
		)
		f, bad := verifyTargetPin(e, "actions/checkout", "v6", newSHA)
		require.True(t, bad)
		assert.Equal(t, checksStale, string(f.Category))
	})

	t.Run("missing from closure", func(t *testing.T) {
		e := edit(
			[]dep.Dependency{{NWO: "other/dep", Ref: "v1", SHA: "x"}},
			map[string]bool{"actions/checkout@v6": true},
		)
		f, bad := verifyTargetPin(e, "actions/checkout", "v6", newSHA)
		require.True(t, bad)
		assert.Equal(t, checksNotPinned, string(f.Category))
	})
}

const (
	checksNotPinned = "not-pinned"
	checksStale     = "stale"
)
