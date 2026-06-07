package ghapi

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cli/go-gh/v2/pkg/api"
)

func TestBuildActionFileQuery(t *testing.T) {
	refs := []ActionFileRequest{
		{Owner: "actions", Repo: "checkout", Ref: "v6"},
		{Owner: "actions", Repo: "cache", Path: "save", Ref: "v4"},
	}

	query, vars, aliases := buildActionFileQuery(refs)
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

func TestParseActionFileResponse(t *testing.T) {
	refs := []ActionFileRequest{
		{Owner: "actions", Repo: "checkout", Ref: "v6"},
		{Owner: "actions", Repo: "cache", Path: "save", Ref: "v4"},
	}
	aliases := map[string]int{"a0": 0, "a1": 1}

	data := map[string]json.RawMessage{
		"a0": json.RawMessage(`{"nameWithOwner":"actions/checkout","object":{"oid":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","file":{"object":{"text":"name: Checkout\nruns:\n  using: node20\n"}}}}`),
		"a1": json.RawMessage(`{"nameWithOwner":"actions/cache","object":{"oid":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","fileYaml":{"object":{"text":"name: Cache Save\nruns:\n  using: composite\n  steps:\n    - uses: actions/upload-artifact@v4\n"}}}}`),
	}

	results := parseActionFileResponse(data, refs, aliases, nil, "")
	if len(results) != 2 {
		t.Fatalf("expected two results, got %d", len(results))
	}
	for _, r := range results {
		if r.Err != nil {
			t.Fatalf("unexpected error for %s/%s: %v", r.Owner, r.Repo, r.Err)
		}
	}
	if results[0].CommitOID != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("expected checkout OID, got %q", results[0].CommitOID)
	}
	if !strings.Contains(results[1].ActionYML, "upload-artifact") {
		t.Fatal("expected yaml content from fileYaml fallback")
	}
}

func TestParseActionFileResponse_AnnotatedTagPeeled(t *testing.T) {
	refs := []ActionFileRequest{
		{Owner: "nodeselector", Repo: "actions-test-fixtures", Ref: "annotated-v1"},
	}
	aliases := map[string]int{"a0": 0}
	data := map[string]json.RawMessage{
		"a0": json.RawMessage(`{"nameWithOwner":"nodeselector/actions-test-fixtures","object":{"oid":"ea53476fdc172d8552df5af9658a45a367e4f41d","file":{"object":{"text":"name: Fixture\nruns:\n  using: node20\n"}}}}`),
	}

	results := parseActionFileResponse(data, refs, aliases, nil, "")
	if len(results) != 1 {
		t.Fatalf("expected one result, got %d", len(results))
	}
	if results[0].Err != nil {
		t.Fatalf("unexpected error: %v", results[0].Err)
	}
	if results[0].Ref != "annotated-v1" {
		t.Fatalf("expected Ref preserved as %q, got %q", "annotated-v1", results[0].Ref)
	}
	if results[0].CommitOID != "ea53476fdc172d8552df5af9658a45a367e4f41d" {
		t.Fatalf("expected peeled commit oid, got %q", results[0].CommitOID)
	}
}

func TestParseActionFileResponse_Errors(t *testing.T) {
	refs := []ActionFileRequest{
		{Owner: "actions", Repo: "checkout", Ref: "v6"},
		{Owner: "actions", Repo: "setup-go", Ref: "v6"},
		{Owner: "actions", Repo: "cache", Ref: "v4"},
	}
	aliases := map[string]int{"a0": 0, "a1": 1, "a2": 2}
	data := map[string]json.RawMessage{
		"a0": json.RawMessage(`null`),
		"a1": json.RawMessage(`{"nameWithOwner":"actions/setup-go","object":{"oid":""}}`),
	}

	results := parseActionFileResponse(data, refs, aliases, nil, "")
	if results[0].Err == nil || !strings.Contains(results[0].Err.Error(), "not found or not accessible") {
		t.Fatalf("expected 'not found' error for checkout, got %v", results[0].Err)
	}
	if results[1].Err == nil || !strings.Contains(results[1].Err.Error(), "does not exist") {
		t.Fatalf("expected 'does not exist' error for setup-go, got %v", results[1].Err)
	}
	if results[2].Err == nil || !strings.Contains(results[2].Err.Error(), "not found in response") {
		t.Fatalf("expected 'not found in response' error for cache, got %v", results[2].Err)
	}
}

func TestParseActionFileResponse_SAMLSSO(t *testing.T) {
	refs := []ActionFileRequest{
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

	results := parseActionFileResponse(data, refs, aliases, gqlErr, "github.localhost")
	if results[0].Err == nil || !strings.Contains(results[0].Err.Error(), "SSO authorization required") {
		t.Fatalf("expected SSO message for checkout, got %v", results[0].Err)
	}
	if !strings.Contains(results[0].Err.Error(), "github.localhost/orgs/actions/sso") {
		t.Fatalf("expected SSO URL, got %v", results[0].Err)
	}
	if results[1].Err == nil || !strings.Contains(results[1].Err.Error(), "not found or not accessible") {
		t.Fatalf("expected generic error for unknownorg, got %v", results[1].Err)
	}
	if strings.Contains(results[0].Err.Error(), "not found or not accessible") {
		t.Fatal("SAML ref should not emit generic not-found")
	}
}

func TestActionFileRequest_NWO(t *testing.T) {
	r := ActionFileRequest{Owner: "actions", Repo: "checkout"}
	if got := r.NWO(); got != "actions/checkout" {
		t.Fatalf("expected actions/checkout, got %q", got)
	}
}

func TestSSORequiredMessage(t *testing.T) {
	msg := ssoRequiredMessage("github.com", "myorg")
	if !strings.Contains(msg, "SSO authorization required") {
		t.Fatalf("missing SSO prefix: %s", msg)
	}
	if !strings.Contains(msg, "github.com/orgs/myorg/sso") {
		t.Fatalf("missing SSO URL: %s", msg)
	}
}

func TestSSORequiredMessage_EmptyHost(t *testing.T) {
	msg := ssoRequiredMessage("", "myorg")
	if !strings.Contains(msg, "github.com/orgs/myorg/sso") {
		t.Fatalf("expected default host, got: %s", msg)
	}
}
