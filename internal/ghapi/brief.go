package ghapi

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"syscall"

	"github.com/cli/go-gh/v2/pkg/api"
)

// Brief omits HTTP request URLs and keeps authorization guidance, shortening
// other HTTP failures to their status. The run log and --json keep the full error.
func Brief(err error) string {
	var ssoErr *SSORateLimitedError
	if errors.As(err, &ssoErr) {
		return ssoErr.Error()
	}
	if AnonymousFallbackLimited(err) {
		return "github.com rate limit for anonymous public lookups exhausted; " + AnonymousFallbackHint
	}
	if code, ok := StatusCode(err); ok {
		msg := fmt.Sprintf("GitHub returned %d %s", code, http.StatusText(code))
		var httpErr *api.HTTPError
		if organizationDenied(err) && errors.As(err, &httpErr) {
			msg += ": " + httpErr.Message
		}
		return msg
	}
	return err.Error()
}

// BriefError is an error whose message is short enough for the terminal.
// Err carries the full message for --json and the run log.
type BriefError struct {
	Msg string
	Err error
}

func (e *BriefError) Error() string { return e.Msg }

func (e *BriefError) Unwrap() error { return e.Err }

// Verbose returns the full message.
func (e *BriefError) Verbose() string { return e.Err.Error() }

// Outage describes a transient failure for the terminal, e.g. "GitHub API
// unreachable (connection refused)", and reports whether it was a rate
// limit. For a joined error it describes the branch cause picks.
func Outage(err error) (reason string, limited bool) {
	if err == nil {
		return "GitHub API unreachable", false
	}
	err = cause(err)
	if rateLimited(err) {
		if publicAuthOf(err) == publicAnonymous {
			return "github.com rate limit for anonymous public lookups exhausted", true
		}
		return "GitHub API rate limit exhausted", true
	}
	if code, ok := StatusCode(err); ok {
		return fmt.Sprintf("GitHub API returned %d %s", code, http.StatusText(code)), false
	}
	return "GitHub API unreachable (" + transportCause(err) + ")", false
}

// transportCause names a transport failure without its request URL.
func transportCause(err error) string {
	var errno syscall.Errno
	var dnsErr *net.DNSError
	var netErr net.Error
	var urlErr *url.Error
	switch {
	case errors.As(err, &errno):
		return errno.Error()
	case errors.As(err, &dnsErr):
		return dnsErr.Err
	case errors.As(err, &netErr) && netErr.Timeout():
		return "timed out"
	case errors.As(err, &urlErr):
		return urlErr.Err.Error()
	}
	return err.Error()
}
