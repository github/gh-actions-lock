package resolver

import (
	"encoding/json"
	"strings"
	"testing"

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

	query, aliases := buildResolveWithFileQuery(refs)
	if len(aliases) != 2 {
		t.Fatalf("expected two aliases, got %+v", aliases)
	}
	for _, want := range []string{
		`a0: repository(owner: "actions", name: "checkout")`,
		`object(expression: "v6")`,
		`file(path: "action.yml")`,
		`a1: repository(owner: "actions", name: "cache")`,
		`file(path: "save/action.yml")`,
		`fileYaml: file(path: "save/action.yaml")`,
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("query missing %q:\n%s", want, query)
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

	deps, ymls, err := parseResolveWithFileResponse(data, refs, aliases)
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
	if !keys["actions/checkout@v6"] || !keys["actions/cache/save@v4"] {
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

	_, _, err := parseResolveWithFileResponse(data, refs, aliases)
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
		httpmock.GraphQL(`repository\(owner: "owner", name: "composite"\)`),
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
		httpmock.GraphQL(`repository\(owner: "actions", name: "checkout"\)`),
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
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	reg.Register(
		httpmock.REST("GET", `/repos/actions/checkout/compare/`),
		httpmock.JSONResponse(map[string]any{"status": "identical"}),
	)

	r, err := NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatalf("NewWithTransport returned error: %v", err)
	}

	result := r.CheckReachability("actions", "checkout", "abc123", "v6")
	if result.Status != Reachable {
		t.Fatalf("expected Reachable, got %s (%s)", result.Status, result.Detail)
	}
}

func TestCheckReachability_Ahead(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	reg.Register(
		httpmock.REST("GET", `/repos/actions/checkout/compare/`),
		httpmock.JSONResponse(map[string]any{"status": "ahead"}),
	)

	r, err := NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatalf("NewWithTransport returned error: %v", err)
	}

	result := r.CheckReachability("actions", "checkout", "abc123", "v6")
	if result.Status != Reachable {
		t.Fatalf("expected Reachable for ahead status, got %s (%s)", result.Status, result.Detail)
	}
}

func TestCheckReachability_Behind(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	reg.Register(
		httpmock.REST("GET", `/repos/actions/checkout/compare/`),
		httpmock.JSONResponse(map[string]any{"status": "behind"}),
	)

	r, err := NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatalf("NewWithTransport returned error: %v", err)
	}

	result := r.CheckReachability("actions", "checkout", "abc123", "v6")
	if result.Status != Reachable {
		t.Fatalf("expected Reachable for behind status (tag rolled back), got %s (%s)", result.Status, result.Detail)
	}
}

func TestCheckReachability_Diverged_Unreachable(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	reg.Register(
		httpmock.REST("GET", `/repos/evil/repo/compare/`),
		httpmock.JSONResponse(map[string]any{"status": "diverged"}),
	)

	r, err := NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatalf("NewWithTransport returned error: %v", err)
	}

	result := r.CheckReachability("evil", "repo", "deadbeef", "v1")
	if result.Status != Unreachable {
		t.Fatalf("expected Unreachable for diverged status, got %s (%s)", result.Status, result.Detail)
	}
	if !strings.Contains(result.Detail, "fork network") {
		t.Fatalf("expected fork network detail, got %q", result.Detail)
	}
}

func TestCheckReachability_404_Unreachable(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	reg.Register(
		httpmock.REST("GET", `/repos/evil/repo/compare/`),
		httpmock.StatusResponse(404),
	)

	r, err := NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatalf("NewWithTransport returned error: %v", err)
	}

	result := r.CheckReachability("evil", "repo", "deadbeef", "v1")
	if result.Status != Unreachable {
		t.Fatalf("expected Unreachable for 404, got %s (%s)", result.Status, result.Detail)
	}
	if !strings.Contains(result.Detail, "not found") {
		t.Fatalf("expected 'not found' detail, got %q", result.Detail)
	}
}

func TestCheckReachability_ServerError_Unknown(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	reg.Register(
		httpmock.REST("GET", `/repos/actions/checkout/compare/`),
		httpmock.StatusResponse(500),
	)

	r, err := NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatalf("NewWithTransport returned error: %v", err)
	}

	result := r.CheckReachability("actions", "checkout", "abc123", "v6")
	if result.Status != ReachabilityUnknown {
		t.Fatalf("expected Unknown for 500, got %s (%s)", result.Status, result.Detail)
	}
}

func TestCheckReachability_CachesResults(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	// Only one stub — second call must hit cache
	reg.Register(
		httpmock.REST("GET", `/repos/actions/checkout/compare/`),
		httpmock.JSONResponse(map[string]any{"status": "identical"}),
	)

	r, err := NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatalf("NewWithTransport returned error: %v", err)
	}

	r1 := r.CheckReachability("actions", "checkout", "abc123", "v6")
	r2 := r.CheckReachability("actions", "checkout", "abc123", "v6")

	if r1.Status != Reachable || r2.Status != Reachable {
		t.Fatalf("expected both calls to return Reachable, got %s and %s", r1.Status, r2.Status)
	}
	if r2.Detail != "cached" {
		t.Fatalf("expected second call to be cached, got detail %q", r2.Detail)
	}
}

func TestCheckReachabilityAll_DeduplicatesRequests(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	// Only one stub for checkout — dedup should prevent double-call
	reg.Register(
		httpmock.REST("GET", `/repos/actions/checkout/compare/`),
		httpmock.JSONResponse(map[string]any{"status": "identical"}),
	)
	reg.Register(
		httpmock.REST("GET", `/repos/actions/setup-go/compare/`),
		httpmock.JSONResponse(map[string]any{"status": "ahead"}),
	)

	r, err := NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatalf("NewWithTransport returned error: %v", err)
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
}
