package ghapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
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

func TestBatchLevelGraphQLErr(t *testing.T) {
	tests := []struct {
		name    string
		gqlErr  *api.GraphQLError
		wantErr bool
	}{
		{name: "nil", gqlErr: nil, wantErr: false},
		{
			name: "cost limit (no path) is batch-level",
			gqlErr: &api.GraphQLError{Errors: []api.GraphQLErrorItem{
				{Message: "query has cost too high", Type: "MAX_NODE_LIMIT_EXCEEDED"},
			}},
			wantErr: true,
		},
		{
			name: "per-alias error (has path) is not batch-level",
			gqlErr: &api.GraphQLError{Errors: []api.GraphQLErrorItem{
				{Message: "could not resolve", Path: []interface{}{"a3"}},
			}},
			wantErr: false,
		},
		{
			name: "no-path saml block is batch-level (splits to attribute SSO)",
			gqlErr: &api.GraphQLError{Errors: []api.GraphQLErrorItem{
				{Message: "saml", Extensions: map[string]interface{}{"saml_failure": true}},
			}},
			wantErr: true,
		},
		{
			name: "per-alias saml block (has path) is not batch-level",
			gqlErr: &api.GraphQLError{Errors: []api.GraphQLErrorItem{
				{Message: "saml", Path: []interface{}{"a2"}, Extensions: map[string]interface{}{"saml_failure": true}},
			}},
			wantErr: false,
		},
		{
			name: "saml block alongside a real cost error still reports batch-level",
			gqlErr: &api.GraphQLError{Errors: []api.GraphQLErrorItem{
				{Message: "saml", Extensions: map[string]interface{}{"saml_failure": true}},
				{Message: "cost too high"},
			}},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := batchLevelGraphQLErr(tc.gqlErr)
			if (got != nil) != tc.wantErr {
				t.Fatalf("batchLevelGraphQLErr() = %v, wantErr=%v", got, tc.wantErr)
			}
		})
	}
}

// splitTransport rejects any batch with more than one alias as a cost-limit
// failure and resolves single-alias batches successfully, modeling GitHub's
// per-query cost ceiling so the adaptive split can be observed end-to-end.
type splitTransport struct {
	mu        sync.Mutex
	calls     int
	multiRejs int
	oids      map[string]string // "owner/name" -> commit oid
}

func (s *splitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	var payload struct {
		Variables map[string]any `json:"variables"`
	}
	_ = json.Unmarshal(body, &payload)

	// Count aliases by ownerN keys present in variables.
	var idxs []int
	for i := 0; ; i++ {
		if _, ok := payload.Variables[fmt.Sprintf("owner%d", i)]; !ok {
			break
		}
		idxs = append(idxs, i)
	}

	s.mu.Lock()
	s.calls++
	if len(idxs) > 1 {
		s.multiRejs++
	}
	s.mu.Unlock()

	if len(idxs) > 1 {
		return jsonHTTP(map[string]any{
			"data": nil,
			"errors": []map[string]any{
				{"message": "query has cost too high", "type": "MAX_NODE_LIMIT_EXCEEDED"},
			},
		})
	}

	owner, _ := payload.Variables["owner0"].(string)
	name, _ := payload.Variables["name0"].(string)
	oid := s.oids[owner+"/"+name]
	data := map[string]any{
		"a0": map[string]any{
			"nameWithOwner": owner + "/" + name,
			"object": map[string]any{
				"oid":  oid,
				"file": map[string]any{"object": map[string]any{"text": "name: x\nruns:\n  using: node20\n"}},
			},
		},
	}
	return jsonHTTP(map[string]any{"data": data})
}

func jsonHTTP(v any) (*http.Response, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(b)),
	}, nil
}

func TestResolveActionFiles_AdaptiveSplit(t *testing.T) {
	tr := &splitTransport{oids: map[string]string{
		"actions/checkout": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"actions/setup-go": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"actions/cache":    "cccccccccccccccccccccccccccccccccccccccc",
	}}
	c, err := New("github.com", WithClientTransport(tr))
	if err != nil {
		t.Fatal(err)
	}
	refs := []ActionFileRequest{
		{Owner: "actions", Repo: "checkout", Ref: "v6"},
		{Owner: "actions", Repo: "setup-go", Ref: "v6"},
		{Owner: "actions", Repo: "cache", Ref: "v3"},
	}

	results := c.ResolveActionFiles(context.Background(), refs)
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	wantOID := map[string]string{
		"checkout": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"setup-go": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"cache":    "cccccccccccccccccccccccccccccccccccccccc",
	}
	for i, r := range results {
		if r.Err != nil {
			t.Fatalf("result %d (%s) unexpected error: %v", i, r.Repo, r.Err)
		}
		if r.CommitOID != wantOID[r.Repo] {
			t.Fatalf("result %d (%s) oid=%q, want %q", i, r.Repo, r.CommitOID, wantOID[r.Repo])
		}
	}
	// Order must survive the split: input order is checkout, setup-go, cache.
	if results[0].Repo != "checkout" || results[1].Repo != "setup-go" || results[2].Repo != "cache" {
		t.Fatalf("result order not preserved across split: %s, %s, %s",
			results[0].Repo, results[1].Repo, results[2].Repo)
	}
	if tr.multiRejs == 0 {
		t.Fatal("expected at least one multi-alias batch to be rejected (the split trigger)")
	}
}

func TestResolveActionFiles_SingleRefBatchErrorSurfaced(t *testing.T) {
	// A single-ref batch can't be split further, so a batch-level cost error
	// must surface on the ref rather than being mislabeled "not found".
	tr := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return jsonHTTP(map[string]any{
			"data": nil,
			"errors": []map[string]any{
				{"message": "query has cost too high", "type": "MAX_NODE_LIMIT_EXCEEDED"},
			},
		})
	})
	c, err := New("github.com", WithClientTransport(tr))
	if err != nil {
		t.Fatal(err)
	}
	results := c.ResolveActionFiles(context.Background(),
		[]ActionFileRequest{{Owner: "actions", Repo: "checkout", Ref: "v6"}})
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Err == nil {
		t.Fatal("expected an error for the single rejected ref")
	}
	if strings.Contains(results[0].Err.Error(), "not found in response") {
		t.Fatalf("batch-level error was mislabeled as not-found: %v", results[0].Err)
	}
	if !strings.Contains(results[0].Err.Error(), "cost too high") {
		t.Fatalf("expected cost error surfaced, got %v", results[0].Err)
	}
}

func TestParseActionFileResponse_NoPathSamlSurfacesSSO(t *testing.T) {
	// A SAML block with no alias path, on a single-ref batch (the state after
	// ResolveActionFiles has split down): the missing alias must surface the
	// actionable SSO message for that owner, not "not found in response".
	refs := []ActionFileRequest{{Owner: "secureorg", Repo: "private-action", Ref: "v1"}}
	aliases := map[string]int{"a0": 0}
	gqlErr := &api.GraphQLError{Errors: []api.GraphQLErrorItem{
		{Message: "Resource protected by organization SAML enforcement",
			Extensions: map[string]interface{}{"saml_failure": true}},
	}}
	// data has no a0 alias (the whole query was rejected).
	results := parseActionFileResponse(map[string]json.RawMessage{}, refs, aliases, gqlErr, "github.com")
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Err == nil {
		t.Fatal("expected an error for the saml-blocked ref")
	}
	msg := results[0].Err.Error()
	if strings.Contains(msg, "not found in response") {
		t.Fatalf("no-path saml mislabeled as not-found: %v", msg)
	}
	if !strings.Contains(msg, "SSO authorization required") || !strings.Contains(msg, "secureorg") {
		t.Fatalf("expected actionable SSO message for secureorg, got %v", msg)
	}
}
