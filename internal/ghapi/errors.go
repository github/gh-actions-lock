package ghapi

import (
	"errors"
	"net/http"

	"github.com/cli/go-gh/v2/pkg/api"
)

// StatusCode reports the HTTP status code carried by err when it originates
// from a REST call. ok is false when err is nil, a transport failure, or a
// GraphQL error that carries no HTTP status. It lets callers classify failures
// without importing go-gh, keeping the API transport internal to this package.
func StatusCode(err error) (code int, ok bool) {
	var httpErr *api.HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode, true
	}
	return 0, false
}

// IsNotFound reports whether err is a 404 from the GitHub REST API.
func IsNotFound(err error) bool {
	code, ok := StatusCode(err)
	return ok && code == http.StatusNotFound
}

// IsPermissionDenied reports whether err is a 401, or a 403 that is not a
// rate-limit response. Rate-limit 403s are reported by IsRateLimited instead so
// callers can back off rather than treat the repo as inaccessible.
func IsPermissionDenied(err error) bool {
	code, ok := StatusCode(err)
	if !ok {
		return false
	}
	if code == http.StatusUnauthorized {
		return true
	}
	return code == http.StatusForbidden && !IsRateLimited(err)
}

// IsRateLimited reports whether err is a primary rate-limit response (HTTP 429)
// or a secondary one (HTTP 403 with the rate-limit budget exhausted).
func IsRateLimited(err error) bool {
	var httpErr *api.HTTPError
	if !errors.As(err, &httpErr) {
		return false
	}
	if httpErr.StatusCode == http.StatusTooManyRequests {
		return true
	}
	return httpErr.StatusCode == http.StatusForbidden &&
		httpErr.Headers.Get("X-RateLimit-Remaining") == "0"
}
