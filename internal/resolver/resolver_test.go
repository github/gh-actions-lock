package resolver

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/github/gh-actions-pin/internal/httpmock"
	"github.com/github/gh-actions-pin/internal/lockfile"
)

func TestSelectLatestTagPrefersHighestMajorTag(t *testing.T) {
	got := selectLatestTag([]string{"v3", "v6", "v5", "v4"})
	if got != "v6" {
		t.Fatalf("expected latest major tag v6, got %q", got)
	}
}

func TestSelectLatestTagFallsBackToHighestStableSemver(t *testing.T) {
	got := selectLatestTag([]string{"v1.2.0", "v1.10.0", "v1.9.9"})
	if got != "v1.10.0" {
		t.Fatalf("expected latest semver tag v1.10.0, got %q", got)
	}
}

func TestSelectLatestTagFallbacksWhenNoStableSemver(t *testing.T) {
	got := selectLatestTag([]string{"main", "nightly", "release-candidate"})
	if got != "release-candidate" {
		t.Fatalf("expected lexical fallback tag, got %q", got)
	}
}

func TestBuildResolveWithFileQuery(t *testing.T) {
	refs := []lockfile.ActionRef{
		{Owner: "actions", Repo: "checkout", Ref: "v6"},
		{Owner: "actions", Repo: "cache", Path: "save", Ref: "v4"},
	}

	query, vars, aliases := buildResolveWithFileQuery(refs)
	if len(aliases) != 2 {
		t.Fatalf("expected two aliases, got %+v", aliases)
	}
	for _, want := range []string{
		`$owner0: String!, $name0: String!, $expr0: String!, $yml0: String!, $yaml0: String!`,
		`a0: repository(owner: $owner0, name: $name0)`,
		`object(expression: $expr0)`,
		`file: file(path: $yml0)`,
		`a1: repository(owner: $owner1, name: $name1)`,
		`fileYaml: file(path: $yaml1)`,
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("query missing %q:\n%s", want, query)
		}
	}
	for k, want := range map[string]any{
		"owner0": "actions", "name0": "checkout", "expr0": "v6^{commit}",
		"yml0": "action.yml", "yaml0": "action.yaml",
		"owner1": "actions", "name1": "cache", "expr1": "v4^{commit}",
		"yml1": "save/action.yml", "yaml1": "save/action.yaml",
	} {
		if vars[k] != want {
			t.Fatalf("vars[%q]=%v, want %v", k, vars[k], want)
		}
	}
}

func TestParseResolveWithFileResponse(t *testing.T) {
	refs := []lockfile.ActionRef{
		{Owner: "actions", Repo: "checkout", Ref: "v6"},
		{Owner: "actions", Repo: "cache", Path: "save", Ref: "v4"},
	}
	aliases := map[string]int{"a0": 0, "a1": 1}

	data := map[string]json.RawMessage{
		"a0": json.RawMessage(`{"nameWithOwner":"actions/checkout","object":{"oid":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","file":{"object":{"text":"name: Checkout\nruns:\n  using: node20\n"}}}}`),
		"a1": json.RawMessage(`{"nameWithOwner":"actions/cache","object":{"oid":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","fileYaml":{"object":{"text":"name: Cache Save\nruns:\n  using: composite\n  steps:\n    - uses: actions/upload-artifact@v4\n"}}}}`),
	}

	deps, ymls, _, err := parseResolveWithFileResponse(data, refs, aliases, nil, "")
	if err != nil {
		t.Fatalf("parseResolveWithFileResponse returned error: %v", err)
	}
	if len(deps) != 2 || len(ymls) != 2 {
		t.Fatalf("expected two deps/ymls, got %d and %d", len(deps), len(ymls))
	}
	keys := map[string]bool{}
	for _, dep := range deps {
		keys[dep.Key()] = true
	}
	if !keys["actions/checkout@v6"] || !keys["actions/cache@v4"] {
		t.Fatalf("unexpected deps: %+v", deps)
	}
	foundFallback := false
	for _, yml := range ymls {
		if strings.Contains(yml, "upload-artifact") {
			foundFallback = true
			break
		}
	}
	if !foundFallback {
		t.Fatalf("expected yaml content from fileYaml fallback, got %#v", ymls)
	}
}

// TestBuildResolveWithFileQueryPeelsAnnotatedTags verifies that the GraphQL
// query unconditionally peels every ref through `^{commit}`. Without the peel,
// `object(expression: "v1")` returns a Tag object for an annotated tag and the
// `... on Commit { oid }` fragment misses, leaving oid empty and the resolver
// reporting `ref "v1" does not exist`. Live fixture:
// nodeselector/actions-test-fixtures has annotated tag `annotated-v1`.
func TestBuildResolveWithFileQueryPeelsAnnotatedTags(t *testing.T) {
	refs := []lockfile.ActionRef{
		{Owner: "nodeselector", Repo: "actions-test-fixtures", Ref: "annotated-v1"},
		{Owner: "actions", Repo: "checkout", Ref: "main"},
		{Owner: "actions", Repo: "cache", Ref: "abc123abc123abc123abc123abc123abc1234567"},
	}
	_, vars, _ := buildResolveWithFileQuery(refs)
	for i, want := range []string{
		"annotated-v1^{commit}",
		"main^{commit}",
		"abc123abc123abc123abc123abc123abc1234567^{commit}",
	} {
		key := fmt.Sprintf("expr%d", i)
		if got := vars[key]; got != want {
			t.Fatalf("vars[%q]=%v, want %q", key, got, want)
		}
	}
	for _, bad := range []any{"annotated-v1", "main"} {
		for i := 0; i < len(refs); i++ {
			key := fmt.Sprintf("expr%d", i)
			if vars[key] == bad {
				t.Fatalf("vars[%q]=%v should have been peeled", key, bad)
			}
		}
	}
}

// TestParseResolveWithFileResponse_AnnotatedTagPeeled mirrors the GraphQL
// response GitHub returns when an annotated tag is peeled with `^{commit}`:
// the peel reaches through the Tag object to the underlying Commit so the
// `... on Commit { oid }` fragment matches normally and the resolver records
// the original ref name alongside the peeled commit SHA.
func TestParseResolveWithFileResponse_AnnotatedTagPeeled(t *testing.T) {
	refs := []lockfile.ActionRef{
		{Owner: "nodeselector", Repo: "actions-test-fixtures", Ref: "annotated-v1"},
	}
	aliases := map[string]int{"a0": 0}
	data := map[string]json.RawMessage{
		"a0": json.RawMessage(`{"nameWithOwner":"nodeselector/actions-test-fixtures","object":{"oid":"ea53476fdc172d8552df5af9658a45a367e4f41d","file":{"object":{"text":"name: Fixture\nruns:\n  using: node20\n"}}}}`),
	}

	deps, _, _, err := parseResolveWithFileResponse(data, refs, aliases, nil, "")
	if err != nil {
		t.Fatalf("parseResolveWithFileResponse returned error: %v", err)
	}
	if len(deps) != 1 {
		t.Fatalf("expected one dep, got %d", len(deps))
	}
	got := deps[0]
	if got.Ref != "annotated-v1" {
		t.Fatalf("expected dep.Ref preserved as %q, got %q", "annotated-v1", got.Ref)
	}
	if got.SHA != "ea53476fdc172d8552df5af9658a45a367e4f41d" {
		t.Fatalf("expected peeled commit oid, got %q", got.SHA)
	}
}

func TestParseResolveWithFileResponseErrors(t *testing.T) {
	refs := []lockfile.ActionRef{
		{Owner: "actions", Repo: "checkout", Ref: "v6"},
		{Owner: "actions", Repo: "setup-go", Ref: "v6"},
		{Owner: "actions", Repo: "cache", Ref: "v4"},
	}
	aliases := map[string]int{"a0": 0, "a1": 1, "a2": 2}
	data := map[string]json.RawMessage{
		"a0": json.RawMessage(`null`),
		"a1": json.RawMessage(`{"nameWithOwner":"actions/setup-go","object":{"oid":""}}`),
	}

	_, _, _, err := parseResolveWithFileResponse(data, refs, aliases, nil, "")
	if err == nil {
		t.Fatal("expected parseResolveWithFileResponse to return aggregated errors")
	}
	for _, want := range []string{
		"actions/checkout@v6: repository not found or not accessible",
		`actions/setup-go@v6: ref "v6" does not exist`,
		"actions/cache@v4: not found in response",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected error to contain %q, got %v", want, err)
		}
	}
}

// TestParseResolveWithFileResponse_SAMLSSO verifies that an org SAML SSO
// enforcement failure (GraphQL FORBIDDEN with extensions.saml_failure: true
// and a null data entry) surfaces a distinct, actionable authorization
// message instead of being collapsed into the generic "repository not found
// or not accessible".
func TestParseResolveWithFileResponse_SAMLSSO(t *testing.T) {
	refs := []lockfile.ActionRef{
		{Owner: "actions", Repo: "checkout", Ref: "v6"},
		{Owner: "unknownorg", Repo: "missing", Ref: "v1"},
	}
	aliases := map[string]int{"a0": 0, "a1": 1}
	data := map[string]json.RawMessage{
		"a0": json.RawMessage(`null`),
		"a1": json.RawMessage(`null`),
	}
	gqlErr := &api.GraphQLError{
		Errors: []api.GraphQLErrorItem{
			{
				Type:       "FORBIDDEN",
				Message:    "Resource protected by organization SAML enforcement.",
				Path:       []interface{}{"a0"},
				Extensions: map[string]interface{}{"saml_failure": true},
			},
		},
	}

	_, _, _, err := parseResolveWithFileResponse(data, refs, aliases, gqlErr, "github.localhost")
	if err == nil {
		t.Fatal("expected aggregated errors")
	}
	msg := err.Error()

	wantSSO := `actions/checkout@v6: SSO authorization required: your token is not authorized for the "actions" organization (SAML enforcement). Authorize it at https://github.localhost/orgs/actions/sso and retry`
	if !strings.Contains(msg, wantSSO) {
		t.Fatalf("expected SSO message %q, got %v", wantSSO, msg)
	}

	// A null entry for an org NOT flagged by SAML must keep the generic message.
	if !strings.Contains(msg, "unknownorg/missing@v1: repository not found or not accessible") {
		t.Fatalf("expected non-SAML null to stay generic, got %v", msg)
	}
	// The SAML-blocked ref must NOT also report the generic message.
	if strings.Contains(msg, "actions/checkout@v6: repository not found or not accessible") {
		t.Fatalf("SAML ref should not also emit generic not-found, got %v", msg)
	}
}

func TestResolveAllRecursiveWithCacheAndCompositeExpansion(t *testing.T) {
	r := &Resolver{
		MaxRecursionDepth: DefaultMaxRecursionDepth,
		cache: map[string]resolvedEntry{
			"actions/checkout@v6": {
				dep: lockfile.Dependency{
					NWO: "actions/checkout", Ref: "v6", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				},
				actionYML: "name: Checkout\nruns:\n  using: node20\n",
			},
		},
		latestRefCache: map[string]string{},
		reachCache:     map[string]ReachabilityStatus{},
	}

	r.cache["owner/composite@v1"] = resolvedEntry{
		dep: lockfile.Dependency{
			NWO: "owner/composite", Ref: "v1", SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		},
		actionYML: "name: Composite\nruns:\n  using: composite\n  steps:\n    - uses: actions/setup-go@v6\n",
	}
	r.cache["actions/setup-go@v6"] = resolvedEntry{
		dep: lockfile.Dependency{
			NWO: "actions/setup-go", Ref: "v6", SHA: "cccccccccccccccccccccccccccccccccccccccc",
		},
		actionYML: "name: Setup Go\nruns:\n  using: node20\n",
	}

	deps, err := r.ResolveAllRecursive([]lockfile.ActionRef{
		{Owner: "actions", Repo: "checkout", Ref: "v6"},
		{Owner: "owner", Repo: "composite", Ref: "v1"},
	})
	if err != nil {
		t.Fatalf("ResolveAllRecursive returned error: %v", err)
	}

	if len(deps) != 3 {
		t.Fatalf("expected three unique deps, got %d: %+v", len(deps), deps)
	}

	// Verify parentMap tracks the child dep key → parent dep key.
	pm := r.ParentMap()
	parents, ok := pm["actions/setup-go@v6"]
	if !ok || len(parents) != 1 || parents[0] != "owner/composite@v1" {
		t.Fatalf("expected parentMap to map actions/setup-go@v6 → [owner/composite@v1], got %v", pm)
	}
}

func TestResolveAllRecursiveMultipleParents(t *testing.T) {
	r := &Resolver{
		MaxRecursionDepth: DefaultMaxRecursionDepth,
		cache: map[string]resolvedEntry{
			"owner/compositeA@v1": {
				dep:       lockfile.Dependency{NWO: "owner/compositeA", Ref: "v1", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
				actionYML: "name: A\nruns:\n  using: composite\n  steps:\n    - uses: shared/dep@v1\n",
			},
			"owner/compositeB@v1": {
				dep:       lockfile.Dependency{NWO: "owner/compositeB", Ref: "v1", SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
				actionYML: "name: B\nruns:\n  using: composite\n  steps:\n    - uses: shared/dep@v1\n",
			},
			"shared/dep@v1": {
				dep:       lockfile.Dependency{NWO: "shared/dep", Ref: "v1", SHA: "cccccccccccccccccccccccccccccccccccccccc"},
				actionYML: "name: Shared\nruns:\n  using: node20\n",
			},
		},
		latestRefCache: map[string]string{},
		reachCache:     map[string]ReachabilityStatus{},
	}

	deps, err := r.ResolveAllRecursive([]lockfile.ActionRef{
		{Owner: "owner", Repo: "compositeA", Ref: "v1"},
		{Owner: "owner", Repo: "compositeB", Ref: "v1"},
	})
	if err != nil {
		t.Fatalf("ResolveAllRecursive returned error: %v", err)
	}

	if len(deps) != 3 {
		t.Fatalf("expected 3 unique deps, got %d: %+v", len(deps), deps)
	}

	pm := r.ParentMap()
	parents, ok := pm["shared/dep@v1"]
	if !ok {
		t.Fatal("expected parentMap to contain shared/dep@v1")
	}
	if len(parents) != 2 {
		t.Fatalf("expected 2 parents for shared/dep@v1, got %d: %v", len(parents), parents)
	}
	// Both composites should be parents (order may vary).
	hasA, hasB := false, false
	for _, p := range parents {
		if p == "owner/compositeA@v1" {
			hasA = true
		}
		if p == "owner/compositeB@v1" {
			hasB = true
		}
	}
	if !hasA || !hasB {
		t.Fatalf("expected both compositeA and compositeB as parents, got %v", parents)
	}
}

func TestResolveAllRecursiveRespectsMaxDepth(t *testing.T) {
	r := &Resolver{
		MaxRecursionDepth: 1,
		cache: map[string]resolvedEntry{
			"owner/composite@v1": {
				dep:       lockfile.Dependency{NWO: "owner/composite", Ref: "v1", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
				actionYML: "name: Composite\nruns:\n  using: composite\n  steps:\n    - uses: owner/nested@v1\n",
			},
			"owner/nested@v1": {
				dep:       lockfile.Dependency{NWO: "owner/nested", Ref: "v1", SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
				actionYML: "name: Nested\nruns:\n  using: composite\n  steps:\n    - uses: actions/checkout@v6\n",
			},
		},
		latestRefCache: map[string]string{},
		reachCache:     map[string]ReachabilityStatus{},
	}

	_, err := r.ResolveAllRecursive([]lockfile.ActionRef{{Owner: "owner", Repo: "composite", Ref: "v1"}})
	if err == nil || !strings.Contains(err.Error(), "exceeded max depth 1") {
		t.Fatalf("expected recursion depth error, got %v", err)
	}
}

// TestResolveAllRecursiveDeepNestedComposites verifies a 3-level chain
// A → B → C produces a properly threaded parentMap so the lockfile writer
// can emit `uses:` at every level.
func TestResolveAllRecursiveDeepNestedComposites(t *testing.T) {
	r := &Resolver{
		MaxRecursionDepth: DefaultMaxRecursionDepth,
		cache: map[string]resolvedEntry{
			"owner/a@v1": {
				dep:       lockfile.Dependency{NWO: "owner/a", Ref: "v1", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
				actionYML: "name: A\nruns:\n  using: composite\n  steps:\n    - uses: owner/b@v1\n",
			},
			"owner/b@v1": {
				dep:       lockfile.Dependency{NWO: "owner/b", Ref: "v1", SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
				actionYML: "name: B\nruns:\n  using: composite\n  steps:\n    - uses: owner/c@v1\n",
			},
			"owner/c@v1": {
				dep:       lockfile.Dependency{NWO: "owner/c", Ref: "v1", SHA: "cccccccccccccccccccccccccccccccccccccccc"},
				actionYML: "name: C\nruns:\n  using: node20\n",
			},
		},
		latestRefCache: map[string]string{},
		reachCache:     map[string]ReachabilityStatus{},
	}

	deps, err := r.ResolveAllRecursive([]lockfile.ActionRef{{Owner: "owner", Repo: "a", Ref: "v1"}})
	if err != nil {
		t.Fatalf("ResolveAllRecursive returned error: %v", err)
	}
	if len(deps) != 3 {
		t.Fatalf("expected 3 deps, got %d: %+v", len(deps), deps)
	}

	pm := r.ParentMap()
	if got := pm["owner/b@v1"]; len(got) != 1 || got[0] != "owner/a@v1" {
		t.Errorf("expected owner/b@v1 parent = [owner/a@v1], got %v", got)
	}
	if got := pm["owner/c@v1"]; len(got) != 1 || got[0] != "owner/b@v1" {
		t.Errorf("expected owner/c@v1 parent = [owner/b@v1], got %v", got)
	}
	// Root has no parents (workflow-direct).
	if _, ok := pm["owner/a@v1"]; ok {
		t.Errorf("expected owner/a@v1 to have no parents, got %v", pm["owner/a@v1"])
	}
}

// TestResolveAllRecursiveSkipsSelfReference verifies that a composite action
// whose `uses:` names its own host repo+ref (a same-tarball routing concern)
// is not recorded as its own transitive dependency.
func TestResolveAllRecursiveSkipsSelfReference(t *testing.T) {
	r := &Resolver{
		MaxRecursionDepth: DefaultMaxRecursionDepth,
		cache: map[string]resolvedEntry{
			"owner/repo@main": {
				dep: lockfile.Dependency{NWO: "owner/repo", Ref: "main", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
				// Self-ref: composite's uses points back at its own NWO@Ref.
				actionYML: "name: Self\nruns:\n  using: composite\n  steps:\n    - uses: owner/repo@main\n",
			},
		},
		latestRefCache: map[string]string{},
		reachCache:     map[string]ReachabilityStatus{},
	}

	deps, err := r.ResolveAllRecursive([]lockfile.ActionRef{{Owner: "owner", Repo: "repo", Ref: "main"}})
	if err != nil {
		t.Fatalf("ResolveAllRecursive returned error: %v", err)
	}
	if len(deps) != 1 {
		t.Fatalf("expected single dep (no self-loop expansion), got %d: %+v", len(deps), deps)
	}
	pm := r.ParentMap()
	if parents, ok := pm["owner/repo@main"]; ok {
		t.Errorf("expected no self-parent for owner/repo@main, got %v", parents)
	}
}

// TestResolveAllRecursiveSiblingSubpathTransitive verifies that a composite
// whose `uses:` names a SIBLING subpath in its own repo+ref (a same-tarball
// edge) is still traversed for its cross-repo transitive deps. Repo layout:
//
//	org/fixtures/nested-composite  (main)  -- uses -->
//	org/fixtures/simple-composite  (main, same tarball) -- uses -->
//	org/fixtures-b                 (main, different repo)
//
// Before the fix the same-tarball edge nested→simple was pruned, so the
// 2-levels-deep org/fixtures-b transitive dep was never discovered or pinned.
func TestResolveAllRecursiveSiblingSubpathTransitive(t *testing.T) {
	const tarballSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	r := &Resolver{
		MaxRecursionDepth: DefaultMaxRecursionDepth,
		cache: map[string]resolvedEntry{
			"org/fixtures/nested-composite@main": {
				dep:       lockfile.Dependency{NWO: "org/fixtures", Path: "nested-composite", Ref: "main", SHA: tarballSHA},
				actionYML: "name: Nested\nruns:\n  using: composite\n  steps:\n    - uses: org/fixtures/simple-composite@main\n",
			},
			"org/fixtures/simple-composite@main": {
				dep:       lockfile.Dependency{NWO: "org/fixtures", Path: "simple-composite", Ref: "main", SHA: tarballSHA},
				actionYML: "name: Simple\nruns:\n  using: composite\n  steps:\n    - uses: org/fixtures-b@main\n",
			},
			"org/fixtures-b@main": {
				dep:       lockfile.Dependency{NWO: "org/fixtures-b", Ref: "main", SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
				actionYML: "name: B\nruns:\n  using: node20\n",
			},
		},
		latestRefCache: map[string]string{},
		reachCache:     map[string]ReachabilityStatus{},
	}

	deps, err := r.ResolveAllRecursive([]lockfile.ActionRef{
		{Owner: "org", Repo: "fixtures", Path: "nested-composite", Ref: "main"},
	})
	if err != nil {
		t.Fatalf("ResolveAllRecursive returned error: %v", err)
	}

	// nested-composite + simple-composite collapse to one tarball entry
	// (org/fixtures@main); org/fixtures-b is the second. Two unique deps.
	if len(deps) != 2 {
		t.Fatalf("expected 2 unique deps, got %d: %+v", len(deps), deps)
	}
	foundB := false
	for _, d := range deps {
		if d.Key() == "org/fixtures-b@main" {
			foundB = true
		}
	}
	if !foundB {
		t.Fatalf("expected 2-levels-deep transitive org/fixtures-b@main to be discovered, got %+v", deps)
	}

	pm := r.ParentMap()
	// The same-tarball edge must NOT create a self-parent on the tarball.
	for _, p := range pm["org/fixtures@main"] {
		if p == "org/fixtures@main" {
			t.Errorf("unexpected same-tarball self-parent edge on org/fixtures@main: %v", pm["org/fixtures@main"])
		}
	}
	// org/fixtures-b's parent is the shared tarball.
	if got := pm["org/fixtures-b@main"]; len(got) != 1 || got[0] != "org/fixtures@main" {
		t.Errorf("expected org/fixtures-b@main parent = [org/fixtures@main], got %v", got)
	}
}

func TestNewWithTransportAndLatestRef(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	reg.Register(
		httpmock.GraphQL(`refs\(refPrefix: "refs/tags/"`),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"repository": map[string]any{
					"refs": map[string]any{
						"nodes": []map[string]string{
							{"name": "v4"},
							{"name": "v5"},
							{"name": "v6"},
						},
					},
				},
			},
		}),
	)

	r, err := NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatalf("NewWithTransport returned error: %v", err)
	}
	if host := r.Hostname(); host != "github.com" {
		t.Fatalf("expected hostname github.com, got %q", host)
	}

	ref, err := r.LatestRef("actions", "checkout")
	if err != nil {
		t.Fatalf("LatestRef returned error: %v", err)
	}
	if ref != "v6" {
		t.Fatalf("expected latest ref v6, got %q", ref)
	}

	// Second call should hit the cache instead of requiring another stub.
	ref, err = r.LatestRef("actions", "checkout")
	if err != nil {
		t.Fatalf("LatestRef cache lookup returned error: %v", err)
	}
	if ref != "v6" {
		t.Fatalf("expected cached latest ref v6, got %q", ref)
	}
}

func TestResolveAllRecursiveWithHTTPTransport(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	reg.Register(
		httpmock.GraphQLForRepo("owner", "composite"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": map[string]any{
					"nameWithOwner": "owner/composite",
					"object": map[string]any{
						"oid": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
						"file": map[string]any{
							"object": map[string]any{
								"text": "name: Composite\nruns:\n  using: composite\n  steps:\n    - uses: actions/checkout@v6\n",
							},
						},
					},
				},
			},
		}),
	)
	reg.Register(
		httpmock.GraphQLForRepo("actions", "checkout"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": map[string]any{
					"nameWithOwner": "actions/checkout",
					"object": map[string]any{
						"oid": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
						"file": map[string]any{
							"object": map[string]any{
								"text": "name: Checkout\nruns:\n  using: node20\n",
							},
						},
					},
				},
			},
		}),
	)

	r, err := NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatalf("NewWithTransport returned error: %v", err)
	}

	deps, err := r.ResolveAllRecursive([]lockfile.ActionRef{{Owner: "owner", Repo: "composite", Ref: "v1"}})
	if err != nil {
		t.Fatalf("ResolveAllRecursive returned error: %v", err)
	}
	if len(deps) != 2 {
		t.Fatalf("expected two deps, got %d: %+v", len(deps), deps)
	}
	if deps[0].NWO != "owner/composite" && deps[1].NWO != "owner/composite" {
		t.Fatalf("expected composite dep to be present, got %+v", deps)
	}
}

func TestCheckReachability_Reachable(t *testing.T) {
	r := &Resolver{
		reachCache: map[string]ReachabilityStatus{},
		checkReachFn: func(owner, repo, sha, ref string) (ReachabilityStatus, string) {
			return Reachable, "ancestor of " + ref
		},
	}
	result := r.CheckReachability("actions", "checkout", "abc123", "v6")
	if result.Status != Reachable {
		t.Fatalf("expected Reachable, got %s (%s)", result.Status, result.Detail)
	}
}

func TestCheckReachability_Unreachable(t *testing.T) {
	r := &Resolver{
		reachCache: map[string]ReachabilityStatus{},
		checkReachFn: func(owner, repo, sha, ref string) (ReachabilityStatus, string) {
			return Unreachable, "commit is not an ancestor of " + ref
		},
	}
	result := r.CheckReachability("evil", "repo", "deadbeef", "v1")
	if result.Status != Unreachable {
		t.Fatalf("expected Unreachable, got %s (%s)", result.Status, result.Detail)
	}
}

func TestCheckReachability_Unknown(t *testing.T) {
	r := &Resolver{
		reachCache: map[string]ReachabilityStatus{},
		checkReachFn: func(owner, repo, sha, ref string) (ReachabilityStatus, string) {
			return ReachabilityUnknown, "clone failed"
		},
	}
	result := r.CheckReachability("actions", "checkout", "abc123", "v6")
	if result.Status != ReachabilityUnknown {
		t.Fatalf("expected Unknown, got %s (%s)", result.Status, result.Detail)
	}
}

func TestCheckReachability_CachesResults(t *testing.T) {
	calls := 0
	r := &Resolver{
		reachCache: map[string]ReachabilityStatus{},
		checkReachFn: func(owner, repo, sha, ref string) (ReachabilityStatus, string) {
			calls++
			return Reachable, "ancestor of " + ref
		},
	}

	r1 := r.CheckReachability("actions", "checkout", "abc123", "v6")
	r2 := r.CheckReachability("actions", "checkout", "abc123", "v6")

	if r1.Status != Reachable || r2.Status != Reachable {
		t.Fatalf("expected both calls to return Reachable, got %s and %s", r1.Status, r2.Status)
	}
	if r2.Detail != "cached" {
		t.Fatalf("expected second call to be cached, got detail %q", r2.Detail)
	}
	if calls != 1 {
		t.Fatalf("expected checkReachFn called once, got %d", calls)
	}
}

func TestCheckReachabilityAll_DeduplicatesRequests(t *testing.T) {
	calls := 0
	r := &Resolver{
		reachCache: map[string]ReachabilityStatus{},
		checkReachFn: func(owner, repo, sha, ref string) (ReachabilityStatus, string) {
			calls++
			return Reachable, "ancestor of " + ref
		},
	}

	deps := []lockfile.Dependency{
		{NWO: "actions/checkout", Ref: "v6", SHA: "aaa"},
		{NWO: "actions/checkout", Ref: "v6", SHA: "aaa"}, // duplicate
		{NWO: "actions/setup-go", Ref: "v6", SHA: "bbb"},
	}

	results := r.CheckReachabilityAll(deps)
	if len(results) != 2 {
		t.Fatalf("expected 2 unique results, got %d: %+v", len(results), results)
	}
	if calls != 2 {
		t.Fatalf("expected 2 calls (deduped), got %d", calls)
	}
}

func TestCheckReachability_SHAAsRef_ChecksViaBranchCommits(t *testing.T) {
	sha := "abc123abc123abc123abc123abc123abc123abc1"

	t.Run("on a branch", func(t *testing.T) {
		reg := &httpmock.Registry{}
		// Bare-SHA ref: main HEAD == sha → exact match → Reachable.
		reg.Register(
			httpmock.REST("GET", `repos/actions/checkout/branches`),
			httpmock.JSONResponse(branchListResponse("main", sha)),
		)
		r, err := NewWithTransport("github.com", reg)
		if err != nil {
			t.Fatal(err)
		}
		result := r.CheckReachability("actions", "checkout", sha, sha)
		if result.Status != Reachable {
			t.Fatalf("expected Reachable, got %s (%s)", result.Status, result.Detail)
		}
		if !strings.Contains(result.Detail, "bare SHA") {
			t.Fatalf("expected detail to mention bare SHA, got %q", result.Detail)
		}
		reg.Verify(t)
	})

	t.Run("not on any branch", func(t *testing.T) {
		reg := &httpmock.Registry{}
		reg.Register(
			httpmock.REST("GET", `repos/actions/checkout/branches`),
			httpmock.JSONResponse(branchListResponse()),
		)
		r, err := NewWithTransport("github.com", reg)
		if err != nil {
			t.Fatal(err)
		}
		result := r.CheckReachability("actions", "checkout", sha, sha)
		if result.Status != Unreachable {
			t.Fatalf("expected Unreachable, got %s (%s)", result.Status, result.Detail)
		}
		if !strings.Contains(result.Detail, "fork-network") {
			t.Fatalf("expected detail to mention fork-network, got %q", result.Detail)
		}
		reg.Verify(t)
	})
}

func TestCheckReachability_OnBranch_Reachable(t *testing.T) {
	sha := "abc123abc123abc123abc123abc123abc123abc1"
	reg := &httpmock.Registry{}
	// Fast path: releases/v6 HEAD == sha → exact match → Reachable.
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/branches`),
		httpmock.JSONResponse(branchListResponse("releases/v6", sha)),
	)

	r, err := NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatal(err)
	}

	result := r.CheckReachability("actions", "checkout", sha, "v6")
	if result.Status != Reachable {
		t.Fatalf("expected Reachable, got %s (%s)", result.Status, result.Detail)
	}
	reg.Verify(t)
}

// TestCheckReachability_ForkInjection_Unreachable verifies that a fork-network
// commit (not on any branch of the canonical repo) is detected as Unreachable.
func TestCheckReachability_ForkInjection_Unreachable(t *testing.T) {
	forkSHA := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

	reg := &httpmock.Registry{}
	// Empty branch list: fork commits have no branches in the upstream repo.
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/branches`),
		httpmock.JSONResponse(branchListResponse()),
	)

	r, err := NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatal(err)
	}

	result := r.CheckReachability("actions", "checkout", forkSHA, "tampered")
	if result.Status != Unreachable {
		t.Fatalf("expected Unreachable for fork injection, got %s (%s)", result.Status, result.Detail)
	}
	if !strings.Contains(result.Detail, "fork-network") {
		t.Fatalf("expected detail to mention fork-network, got %q", result.Detail)
	}
	reg.Verify(t)
}

// TestCheckReachability_BranchListError_ReturnsUnknown verifies graceful
// degradation when the branches endpoint fails.
func TestCheckReachability_BranchListError_ReturnsUnknown(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/branches`),
		httpmock.StatusResponse(500),
	)

	r, err := NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatal(err)
	}

	result := r.CheckReachability("actions", "checkout", "abc123abc123abc123abc123abc123abc123abc1", "v6")
	if result.Status != ReachabilityUnknown {
		t.Fatalf("expected Unknown when branches API fails, got %s (%s)", result.Status, result.Detail)
	}
	reg.Verify(t)
}

// TestCheckReachability_Disabled_ReturnsUnknown verifies that
// DisableReachability=true bypasses the security check and returns Unknown.
func TestCheckReachability_Disabled_ReturnsUnknown(t *testing.T) {
	r, err := NewWithTransport("github.com", &httpmock.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	r.DisableReachability = true

	result := r.CheckReachability("actions", "checkout", "abc123", "v6")
	if result.Status != ReachabilityUnknown {
		t.Fatalf("expected Unknown when disabled, got %s (%s)", result.Status, result.Detail)
	}
}

func TestCheckAncestry_Confirmed(t *testing.T) {
	pinnedSHA := "abc123abc123abc123abc123abc123abc123abc1"
	liveSHA := "def456def456def456def456def456def456def4"

	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "repos/actions/checkout/compare/"),
		httpmock.JSONResponse(map[string]any{
			"status": "ahead",
			"merge_base_commit": map[string]any{
				"sha": pinnedSHA,
			},
		}),
	)

	r, err := NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatal(err)
	}

	status, _ := r.CheckAncestry("actions", "checkout", pinnedSHA, liveSHA)
	if status != AncestryConfirmed {
		t.Fatalf("expected AncestryConfirmed, got %d", status)
	}
	reg.Verify(t)
}

func TestCheckAncestry_NotAncestor_DifferentMergeBase(t *testing.T) {
	pinnedSHA := "abc123abc123abc123abc123abc123abc123abc1"
	liveSHA := "def456def456def456def456def456def456def4"

	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "repos/actions/checkout/compare/"),
		httpmock.JSONResponse(map[string]any{
			"status": "diverged",
			"merge_base_commit": map[string]any{
				"sha": "unrelated_000000000000000000000000000000",
			},
		}),
	)

	r, err := NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatal(err)
	}

	status, detail := r.CheckAncestry("actions", "checkout", pinnedSHA, liveSHA)
	if status != AncestryNotAncestor {
		t.Fatalf("expected AncestryNotAncestor, got %d", status)
	}
	if !strings.Contains(detail, "not the pinned SHA") {
		t.Fatalf("expected detail about merge base mismatch, got %q", detail)
	}
	reg.Verify(t)
}

func TestCheckAncestry_NotAncestor_404(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "repos/actions/checkout/compare/"),
		httpmock.StatusResponse(404),
	)

	r, err := NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatal(err)
	}

	status, detail := r.CheckAncestry("actions", "checkout", "abc123abc123abc123abc123abc123abc123abc1", "def456def456def456def456def456def456def4")
	if status != AncestryNotAncestor {
		t.Fatalf("expected AncestryNotAncestor for 404, got %d", status)
	}
	if !strings.Contains(detail, "not found") {
		t.Fatalf("expected 'not found' detail, got %q", detail)
	}
	reg.Verify(t)
}

func TestCheckAncestry_NotAncestor_409(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "repos/actions/checkout/compare/"),
		httpmock.StatusResponse(409),
	)

	r, err := NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatal(err)
	}

	status, detail := r.CheckAncestry("actions", "checkout", "abc123abc123abc123abc123abc123abc123abc1", "def456def456def456def456def456def456def4")
	if status != AncestryNotAncestor {
		t.Fatalf("expected AncestryNotAncestor for 409, got %d", status)
	}
	if !strings.Contains(detail, "no common ancestor") {
		t.Fatalf("expected 'no common ancestor' detail, got %q", detail)
	}
	reg.Verify(t)
}

func TestCheckAncestry_Unknown_RateLimit(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "repos/actions/checkout/compare/"),
		httpmock.StatusResponse(429),
	)

	r, err := NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatal(err)
	}

	status, detail := r.CheckAncestry("actions", "checkout", "abc123abc123abc123abc123abc123abc123abc1", "def456def456def456def456def456def456def4")
	if status != AncestryUnknown {
		t.Fatalf("expected AncestryUnknown for rate limit, got %d", status)
	}
	if !strings.Contains(detail, "rate limited") {
		t.Fatalf("expected 'rate limited' detail, got %q", detail)
	}
	reg.Verify(t)
}
