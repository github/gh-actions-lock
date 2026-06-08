package ghapi

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/cli/go-gh/v2/pkg/api"
)

func httpErr(status int, headers http.Header) error {
	return &api.HTTPError{StatusCode: status, Headers: headers}
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
