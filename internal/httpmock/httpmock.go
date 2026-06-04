package httpmock

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"testing"
)

// Matcher determines whether a stub matches an outbound request.
type Matcher func(req *http.Request) bool

// Responder builds an HTTP response for a matched request.
type Responder func(req *http.Request) (*http.Response, error)

type stub struct {
	stack     string
	matched   bool
	matcher   Matcher
	responder Responder
}

// Registry records requests and serves stubbed responses. It mirrors the
// cli/cli pkg/httpmock usage pattern for deterministic API tests.
type Registry struct {
	mu       sync.Mutex
	stubs    []*stub
	Requests []*http.Request
}

// Register adds a matcher/response pair to the registry.
func (r *Registry) Register(m Matcher, resp Responder) {
	r.stubs = append(r.stubs, &stub{
		stack:     string(debug.Stack()),
		matcher:   m,
		responder: resp,
	})
}

// Verify fails the test if any registered stubs were never exercised.
func (r *Registry) Verify(t *testing.T) {
	t.Helper()

	var unmatched []string
	for _, s := range r.stubs {
		if !s.matched {
			unmatched = append(unmatched, s.stack)
		}
	}
	if len(unmatched) == 0 {
		return
	}

	var b strings.Builder
	for i, stack := range unmatched {
		fmt.Fprintf(&b, "Stub %d:\n\t%s", i+1, stack)
		if i < len(unmatched)-1 {
			b.WriteString("\n")
		}
	}
	t.Fatalf("%d HTTP stubs unmatched:\n%s", len(unmatched), b.String())
}

// RoundTrip satisfies http.RoundTripper.
func (r *Registry) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, s := range r.stubs {
		if s.matched || !s.matcher(req) {
			continue
		}
		s.matched = true
		r.Requests = append(r.Requests, req)
		return s.responder(req)
	}

	return nil, fmt.Errorf("no registered HTTP stubs matched %v", req.URL)
}

// GraphQL matches a POST GraphQL request whose query matches q.
func GraphQL(q string) Matcher {
	re := regexp.MustCompile(q)

	return func(req *http.Request) bool {
		if !strings.EqualFold(req.Method, http.MethodPost) {
			return false
		}
		if req.URL.Path != "/graphql" && req.URL.Path != "/api/graphql" {
			return false
		}

		var body struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = decodeJSONBody(req, &body)

		// Match against the query string. Older callers wrote regexes
		// over inlined `repository(owner: "actions", ...)` literals;
		// queries that use GraphQL variables also expose those values
		// in the variables map, so we synthesize a deterministic
		// "varKey=varValue" haystack and let the regex search both.
		if re.MatchString(body.Query) {
			return true
		}
		return re.MatchString(varsHaystack(body.Variables))
	}
}

// GraphQLForRepo matches any GraphQL request whose query or variables
// reference the given repository owner/name. This is the preferred matcher
// for queries that pass owner/name through GraphQL `$variables` rather
// than interpolating them as string literals.
func GraphQLForRepo(owner, repo string) Matcher {
	return func(req *http.Request) bool {
		if !strings.EqualFold(req.Method, http.MethodPost) {
			return false
		}
		if req.URL.Path != "/graphql" && req.URL.Path != "/api/graphql" {
			return false
		}

		var body struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = decodeJSONBody(req, &body)

		// Inline literal form: e.g. `owner: "actions", name: "checkout"`.
		inline := fmt.Sprintf(`owner: %q, name: %q`, owner, repo)
		if strings.Contains(body.Query, inline) {
			return true
		}
		// Variable form: pair an owner var with the matching name var
		// (owner0/name0, owner1/name1, ...). This avoids a false match
		// when one repo's owner happens to equal another repo's name.
		for k, v := range body.Variables {
			if !strings.HasPrefix(k, "owner") {
				continue
			}
			if vs, _ := v.(string); vs != owner {
				continue
			}
			suffix := strings.TrimPrefix(k, "owner")
			if name, ok := body.Variables["name"+suffix].(string); ok && name == repo {
				return true
			}
		}
		return false
	}
}

func varsHaystack(vars map[string]any) string {
	if len(vars) == 0 {
		return ""
	}
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&sb, "%s=%v\n", k, vars[k])
	}
	return sb.String()
}

// JSONResponse turns a value into a 200 JSON response.
func JSONResponse(body any) Responder {
	return func(req *http.Request) (*http.Response, error) {
		b, _ := json.Marshal(body)
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type": []string{"application/json"},
			},
			Body:    io.NopCloser(bytes.NewBuffer(b)),
			Request: req,
			Status:  "200 OK",
		}
		return resp, nil
	}
}

// GraphQLQuery inspects the outbound GraphQL request before returning a body.
func GraphQLQuery(body string, cb func(query string, variables map[string]any)) Responder {
	return func(req *http.Request) (*http.Response, error) {
		var payload struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := decodeJSONBody(req, &payload); err != nil {
			return nil, err
		}
		cb(payload.Query, payload.Variables)

		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type": []string{"application/json"},
			},
			Body:    io.NopCloser(bytes.NewBufferString(body)),
			Request: req,
			Status:  "200 OK",
		}
		return resp, nil
	}
}

// REST matches a request by method and URL path pattern (regex).
func REST(method, pathPattern string) Matcher {
	re := regexp.MustCompile(pathPattern)

	return func(req *http.Request) bool {
		if !strings.EqualFold(req.Method, method) {
			return false
		}
		return re.MatchString(req.URL.Path)
	}
}

// RESTWithQuery is like REST but also requires querySubstring to appear in the
// request's raw query string. Use this to distinguish calls that share a URL
// path but differ by query parameter (e.g. branches?protected=true vs
// branches?per_page=100).
func RESTWithQuery(method, pathPattern, querySubstring string) Matcher {
	re := regexp.MustCompile(pathPattern)

	return func(req *http.Request) bool {
		if !strings.EqualFold(req.Method, method) {
			return false
		}
		return re.MatchString(req.URL.Path) && strings.Contains(req.URL.RawQuery, querySubstring)
	}
}

// StatusResponse returns a response with the given status code and empty body.
func StatusResponse(code int) Responder {
	return func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: code,
			Header:     http.Header{},
			Body:       io.NopCloser(bytes.NewBuffer(nil)),
			Request:    req,
			Status:     fmt.Sprintf("%d", code),
		}, nil
	}
}

func decodeJSONBody(req *http.Request, dest any) error {
	b, err := readBody(req)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dest)
}

func readBody(req *http.Request) ([]byte, error) {
	bodyCopy := &bytes.Buffer{}
	r := io.TeeReader(req.Body, bodyCopy)
	req.Body = io.NopCloser(bodyCopy)
	return io.ReadAll(r)
}
