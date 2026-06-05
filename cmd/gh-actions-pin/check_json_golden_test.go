package main

// Golden-file snapshot test for the `gh actions-pin check --json` contract.
//
// We promised Dependabot the JSON shape is additive-only: no field renames,
// no removals, no type shifts. New optional fields are allowed. This test
// enforces that promise structurally.
//
// To regenerate the golden after an intentional additive change:
//
//	UPDATE_GOLDEN=1 go test ./cmd/gh-actions-pin/ -run TestCheckCommand_JSONGolden
//
// CI runs without the env var, so any change to the JSON shape must be
// intentional and committed alongside the code change.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/github/gh-actions-pin/internal/httpmock"
	"github.com/stretchr/testify/require"
)

// goldenFixtureDir is the source-of-truth fixture for the JSON golden test.
// The .github/workflows/ subtree is copied verbatim into a tempdir before
// each run so the test doesn't mutate state under testdata/.
const goldenFixtureDir = "testdata/golden-json"

func TestCheckCommand_JSONGolden(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	const (
		checkoutSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		setupGoSHA  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		cacheSHA    = "cccccccccccccccccccccccccccccccccccccccc"
		helperSHA   = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	)

	// setup-go is a composite action that fans out to actions/cache AND
	// a helper-only dep. The helper appears only in the transitive
	// closure (no direct workflow ref), so the formatter populates its
	// required_by[] from the resolver's parent map — exactly the
	// transitive-coverage signal we want the golden to lock down.
	compositeYAML := "name: Setup Go\n" +
		"runs:\n" +
		"  using: composite\n" +
		"  steps:\n" +
		"    - uses: actions/cache@v4\n" +
		"    - uses: helper/only-transitive@v1\n"

	// Direct refs from the workflow: checkout@v6, setup-go@v6, cache@v3.
	// The resolver batches them into a single GraphQL request keyed by
	// owner/name pairs (a0/a1/a2).
	reg.Register(
		httpmock.GraphQLForRepo("actions", "checkout"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/checkout", checkoutSHA, nodeActionYAML),
				"a1": testRepoResponse("actions/setup-go", setupGoSHA, compositeYAML),
				"a2": testRepoResponse("actions/cache", cacheSHA, nodeActionYAML),
			},
		}),
	)

	// Transitive batch discovered from the setup-go composite: cache@v4
	// (same NWO as a direct workflow ref) and helper/only-transitive@v1
	// (transitive-only, gives us a populated required_by[] in the JSON).
	reg.Register(
		httpmock.GraphQLForRepo("actions", "cache"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/cache", cacheSHA, nodeActionYAML),
				"a1": testRepoResponse("helper/only-transitive", helperSHA, nodeActionYAML),
			},
		}),
	)

	// Resolve the fixture path against the package directory BEFORE
	// chdir'ing into the tempdir — UPDATE_GOLDEN rewrites the source
	// expected.json, not a copy under the tempdir.
	srcDir, err := filepath.Abs(goldenFixtureDir)
	require.NoError(t, err)
	dir := t.TempDir()
	copyFixtureTree(t, srcDir, dir)
	t.Chdir(dir)

	stdout, _, err := runCommandWithHTTPAndReach(t, reg, reachableFunc(),
		// The fixture's lockfile addresses the workflow as
		// .github/workflows/ci.yml, so we run check on that exact path.
		"check", "--rescan", "--json=valid,findings,workflows,dependencies",
		".github/workflows/ci.yml",
	)
	// We expect findings (ref-changed + stale), so the command exits
	// non-zero. JSON output is still on stdout per the contract.
	require.ErrorIs(t, err, errSilent, "JSON mode should exit non-zero when findings are present")

	got := normalizeGoldenJSON(t, []byte(stdout))

	goldenPath := filepath.Join(srcDir, "expected.json")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		writeGolden(t, goldenPath, got)
		return
	}

	wantRaw, err := os.ReadFile(goldenPath)
	require.NoError(t, err, "read golden %s — run with UPDATE_GOLDEN=1 to create it", goldenPath)
	want := normalizeGoldenJSON(t, wantRaw)

	diffs := diffAdditive("", want, got)
	if len(diffs) == 0 {
		return
	}
	t.Fatalf("JSON contract drift detected (re-run with UPDATE_GOLDEN=1 only if the change is intentional and additive):\n  %s",
		strings.Join(diffs, "\n  "))
}

// normalizeGoldenJSON parses the raw JSON, masks volatile fields, and
// canonicalizes array ordering so the golden compares deterministically
// across runs and machines.
func normalizeGoldenJSON(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var obj map[string]any
	require.NoError(t, json.Unmarshal(raw, &obj), "parse JSON")

	// Mask the cli_version: it varies by build mode ("(devel)" for `go
	// test`, a real semver tag for installed extension). Lockfile schema
	// version is deterministic (v0.0.1 today) but mask it too so a future
	// schema bump can ship additive fields without rewriting the golden
	// for the version string.
	if _, ok := obj["cli_version"]; ok {
		obj["cli_version"] = "<MASKED>"
	}
	if _, ok := obj["lockfile_version"]; ok {
		obj["lockfile_version"] = "<MASKED>"
	}

	// Sort arrays whose element order isn't part of the contract. The
	// JSON formatter is mostly deterministic today, but lockfile-derived
	// maps can shuffle if the engine changes iteration order.
	if findings, ok := obj["findings"].([]any); ok {
		obj["findings"] = sortByKey(findings, findingSortKey)
	}
	if deps, ok := obj["dependencies"].([]any); ok {
		obj["dependencies"] = sortByKey(deps, depSortKey)
	}
	if wfs, ok := obj["workflows"].([]any); ok {
		obj["workflows"] = sortByKey(wfs, workflowSortKey)
		// Sort nested arrays inside each workflow.
		for _, w := range obj["workflows"].([]any) {
			wm, ok := w.(map[string]any)
			if !ok {
				continue
			}
			if fs, ok := wm["findings"].([]any); ok {
				wm["findings"] = sortByKey(fs, findingSortKey)
			}
			if ds, ok := wm["dependencies"].([]any); ok {
				wm["dependencies"] = sortByKey(ds, depSortKey)
			}
		}
	}
	return obj
}

// findingSortKey returns a stable sort key for a Finding object.
// Category + workflow + detail picks a unique row in our fixture.
func findingSortKey(v any) string {
	m, _ := v.(map[string]any)
	return fmt.Sprintf("%v|%v|%v|%v", m["category"], m["workflow"], m["dependency"], m["detail"])
}

// depSortKey returns a stable sort key for a Dependency object.
func depSortKey(v any) string {
	m, _ := v.(map[string]any)
	return fmt.Sprintf("%v|%v|%v", m["nwo"], m["ref"], m["sha"])
}

// workflowSortKey returns a stable sort key for a Workflow object.
func workflowSortKey(v any) string {
	m, _ := v.(map[string]any)
	s, _ := m["path"].(string)
	return s
}

func sortByKey(arr []any, keyFn func(any) string) []any {
	out := make([]any, len(arr))
	copy(out, arr)
	sort.SliceStable(out, func(i, j int) bool {
		return keyFn(out[i]) < keyFn(out[j])
	})
	return out
}

// diffAdditive recursively compares want to got and returns a list of
// human-readable diffs. The comparison is structural-additive: every key
// in want must exist in got with the same (recursively-compared) value,
// but extra keys in got are allowed (new optional fields). Array element
// counts must match — adding or removing elements is a shape change.
func diffAdditive(path string, want, got any) []string {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return []string{fmt.Sprintf("type mismatch at %s: want object, got %T", path, got)}
		}
		var diffs []string
		// Deterministic key order so failure messages are stable.
		keys := make([]string, 0, len(w))
		for k := range w {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			child := joinPath(path, k)
			wv := w[k]
			gv, present := g[k]
			if !present {
				diffs = append(diffs, fmt.Sprintf("missing field at %s (breaking removal/rename — additive changes must keep prior fields)", child))
				continue
			}
			diffs = append(diffs, diffAdditive(child, wv, gv)...)
		}
		return diffs
	case []any:
		g, ok := got.([]any)
		if !ok {
			return []string{fmt.Sprintf("type mismatch at %s: want array, got %T", path, got)}
		}
		if len(w) != len(g) {
			return []string{fmt.Sprintf("array length changed at %s: want %d elements, got %d", path, len(w), len(g))}
		}
		var diffs []string
		for i := range w {
			diffs = append(diffs, diffAdditive(fmt.Sprintf("%s[%d]", path, i), w[i], g[i])...)
		}
		return diffs
	default:
		if !equalScalar(want, got) {
			return []string{fmt.Sprintf("value changed at %s: want %v (%T), got %v (%T)", path, want, want, got, got)}
		}
		return nil
	}
}

func equalScalar(a, b any) bool {
	// JSON numbers all decode to float64, so equality is direct.
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

func joinPath(parent, child string) string {
	if parent == "" {
		return child
	}
	return parent + "." + child
}

// writeGolden serializes the masked, sorted payload back to disk in pretty
// form. UPDATE_GOLDEN=1 is the only caller.
func writeGolden(t *testing.T, path string, obj map[string]any) {
	t.Helper()
	buf, err := json.MarshalIndent(obj, "", "  ")
	require.NoError(t, err)
	buf = append(buf, '\n')
	require.NoError(t, os.WriteFile(path, buf, 0o600))
	t.Logf("wrote golden %s (%d bytes)", path, len(buf))
}

// copyFixtureTree mirrors the .github/workflows/ subtree from srcDir to
// dstDir. The fixture is text-only YAML/JSON, so we don't preserve modes
// beyond 0o600.
func copyFixtureTree(t *testing.T, srcDir, dstDir string) {
	t.Helper()
	walkRoot := filepath.Join(srcDir, ".github")
	require.NoError(t, filepath.Walk(walkRoot, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dstDir, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	}), "copy fixture tree")
}

// silence unused-import linter if our pipeline gets stricter; http is
// referenced only transitively today.
var _ http.RoundTripper = (*httpmock.Registry)(nil)
