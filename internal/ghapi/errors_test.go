package ghapi

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/cli/go-gh/v2/pkg/api"
)

func httpErr(status int, headers http.Header) error {
	return &api.HTTPError{RequestURL: &url.URL{}, StatusCode: status, Headers: headers}
}

func TestStatusCode(t *testing.T) {
	if _, ok := StatusCode(nil); ok {
		t.Fatal("nil error should not report a status code")
	}
	if _, ok := StatusCode(errors.New("transport boom")); ok {
		t.Fatal("plain error should not report a status code")
	}
	code, ok := StatusCode(fmt.Errorf("wrapped: %w", httpErr(404, nil)))
	if !ok || code != 404 {
		t.Fatalf("got (%d, %v), want (404, true) through wrapping", code, ok)
	}
}

func TestIsNotFound(t *testing.T) {
	if !IsNotFound(httpErr(http.StatusNotFound, nil)) {
		t.Fatal("404 should be not-found")
	}
	if IsNotFound(httpErr(http.StatusForbidden, nil)) {
		t.Fatal("403 should not be not-found")
	}
	if IsNotFound(errors.New("boom")) {
		t.Fatal("non-HTTP error should not be not-found")
	}
}

func TestRateLimitVsPermission(t *testing.T) {
	exhausted := http.Header{}
	exhausted.Set("X-RateLimit-Remaining", "0")

	tooMany := httpErr(http.StatusTooManyRequests, nil)
	secondary := httpErr(http.StatusForbidden, exhausted)
	plainForbidden := httpErr(http.StatusForbidden, nil)
	unauthorized := httpErr(http.StatusUnauthorized, nil)

	if !IsRateLimited(tooMany) || !IsRateLimited(secondary) {
		t.Fatal("429 and budget-exhausted 403 should be rate-limited")
	}
	retryAfter := http.Header{}
	retryAfter.Set("Retry-After", "60")
	if !IsRateLimited(httpErr(http.StatusForbidden, retryAfter)) || IsPermissionDenied(httpErr(http.StatusForbidden, retryAfter)) {
		t.Fatal("403 with Retry-After is a secondary rate limit, not permission-denied")
	}
	if IsRateLimited(plainForbidden) {
		t.Fatal("403 with budget remaining should not be rate-limited")
	}
	if !IsPermissionDenied(plainForbidden) || !IsPermissionDenied(unauthorized) {
		t.Fatal("plain 403 and 401 should be permission-denied")
	}
	if IsPermissionDenied(secondary) {
		t.Fatal("rate-limit 403 should not be reported as permission-denied")
	}
}

func TestIsTransient(t *testing.T) {
	notFound := fmt.Errorf("a/b@v1: %w", httpErr(http.StatusNotFound, nil))
	unavailable := fmt.Errorf("c/d@v1: %w", httpErr(http.StatusServiceUnavailable, nil))
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"5xx", unavailable, true},
		{"404", notFound, false},
		{"404 joined before 5xx", errors.Join(notFound, unavailable), true},
		{"nested joins", errors.Join(errors.Join(notFound), errors.Join(unavailable)), true},
		{"only answers", errors.Join(notFound, httpErr(http.StatusUnprocessableEntity, nil)), false},
		{"401", httpErr(http.StatusUnauthorized, nil), false},
		{"connection refused", &url.Error{Op: "Post", URL: "https://api.github.com/graphql", Err: &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}}, true},
		{"untrusted certificate", &url.Error{Op: "Get", URL: "https://api.github.com", Err: &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}}, false},
		{"retries ran out", &url.Error{Op: "Get", URL: "https://api.github.com", Err: &UnknownOutcomeError{Cause: io.ErrUnexpectedEOF}}, true},
		{"graphql rate limited", &api.GraphQLError{Errors: []api.GraphQLErrorItem{{Type: "RATE_LIMITED", Message: "API rate limit exceeded"}}}, true},
		{"graphql not found", &api.GraphQLError{Errors: []api.GraphQLErrorItem{{Type: "NOT_FOUND", Path: []any{"a0"}}}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsTransient(tt.err); got != tt.want {
				t.Errorf("IsTransient = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestConfigError(t *testing.T) {
	ghes, err := url.Parse("https://ghes.example.com/api/v3/repos/a/b")
	if err != nil {
		t.Fatal(err)
	}
	dotcom, err := url.Parse("https://api.github.com/graphql")
	if err != nil {
		t.Fatal(err)
	}
	untrusted := &url.Error{Op: "Post", URL: "https://api.github.com/graphql", Err: &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}}
	refused := &url.Error{Op: "Post", URL: "https://api.github.com/graphql", Err: &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}}
	rejected := &api.HTTPError{StatusCode: http.StatusUnauthorized, RequestURL: dotcom}
	sources := map[string]string{"github.com": "oauth_token", "ghes.example.com": "oauth_token", "env.example.com": "GH_ENTERPRISE_TOKEN"}
	orig := tokenSource
	t.Cleanup(func() { tokenSource = orig })
	tokenSource = func(host string) string { return sources[host] }
	envHost, err := url.Parse("https://env.example.com/api/v3/repos/a/b")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"token from the environment", &api.HTTPError{StatusCode: http.StatusUnauthorized, RequestURL: envHost}, "GitHub rejected the token in GH_ENTERPRISE_TOKEN (401); check it, or unset it to use `gh auth login`"},
		{"rejected token after an outage", fmt.Errorf("resolving: %w", errors.Join(refused, fmt.Errorf("a/b@v1: %w", rejected))), "GitHub rejected the token (401); run `gh auth login`"},
		{"untrusted certificate after a 404 under a wrapper", fmt.Errorf("resolving: %w", errors.Join(httpErr(http.StatusNotFound, nil), errors.Join(untrusted))), "TLS certificate for api.github.com is not trusted; check HTTPS_PROXY or your CA bundle"},
		{"untrusted certificate", fmt.Errorf("checking pins: %w", untrusted), "TLS certificate for api.github.com is not trusted; check HTTPS_PROXY or your CA bundle"},
		{"rejected token", rejected, "GitHub rejected the token (401); run `gh auth login`"},
		{"rejected token on another host", &api.HTTPError{StatusCode: http.StatusUnauthorized, RequestURL: ghes}, "GitHub rejected the token (401); run `gh auth login --hostname ghes.example.com`"},
		{"joined behind a 404", errors.Join(httpErr(http.StatusNotFound, nil), untrusted), "TLS certificate for api.github.com is not trusted; check HTTPS_PROXY or your CA bundle"},
		{"permission denied is per repository", httpErr(http.StatusForbidden, nil), ""},
		{"outage", &url.Error{Op: "Post", URL: "https://api.github.com/graphql", Err: &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}}, ""},
		{"nil", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ConfigError(tt.err)
			if tt.want == "" {
				if got != nil {
					t.Errorf("ConfigError = %q, want nil", got)
				}
				return
			}
			if got == nil || got.Error() != tt.want {
				t.Errorf("ConfigError = %v, want %q", got, tt.want)
			}
		})
	}
}

// TestHTTPErrorLiteralsSetRequestURL: api.HTTPError.Error formats a nil
// RequestURL by faulting inside url.URL.String, which fmt recovers. On
// windows/amd64 a recovered hardware fault can corrupt the heap
// (golang/go#81238), so no literal may leave RequestURL nil.
func TestHTTPErrorLiteralsSetRequestURL(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			sel, ok := lit.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "HTTPError" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "api" {
				return true
			}
			for _, e := range lit.Elts {
				if kv, ok := e.(*ast.KeyValueExpr); ok {
					if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "RequestURL" {
						return true
					}
				}
			}
			t.Errorf("%s: api.HTTPError literal without RequestURL", fset.Position(lit.Pos()))
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
