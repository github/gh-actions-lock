package httpmock

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"runtime/debug"
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
			Query string `json:"query"`
		}
		_ = decodeJSONBody(req, &body)

		return re.MatchString(body.Query)
	}
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
