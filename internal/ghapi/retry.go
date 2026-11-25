package ghapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// This transport is the only retry layer: callers above it never retry, and
// never split a batch because of an outage.
const (
	backoffBase = time.Second

	dialTimeout           = 10 * time.Second
	tlsHandshakeTimeout   = 10 * time.Second
	responseHeaderTimeout = 30 * time.Second
	requestTimeout        = 60 * time.Second
)

// retryPolicy bounds how hard a run retries.
type retryPolicy struct {
	attempts   int
	backoffCap time.Duration
	// window is how long a run keeps retrying once GitHub stops answering.
	// Any answer closes it. A rate-limit wait that ends past it isn't made.
	window time.Duration
}

var (
	// patientPolicy is for CI: nobody is watching, and a job that waits
	// out a short outage or rate limit beats a red check.
	patientPolicy = retryPolicy{attempts: 8, backoffCap: 30 * time.Second, window: 2 * time.Minute}
	// quickPolicy is for a terminal: retry a blip, then report and exit
	// so the user can decide.
	quickPolicy = retryPolicy{attempts: 3, backoffCap: 2 * time.Second, window: 10 * time.Second}
)

// runPolicy picks the retry policy for this process. Dependabot sets neither
// CI variable, but nobody watches its runs either.
func runPolicy() retryPolicy {
	if os.Getenv("CI") == "true" || os.Getenv("GITHUB_ACTIONS") == "true" || DependabotProxy() {
		return patientPolicy
	}
	return quickPolicy
}

// UnknownOutcomeError reports a request that ended without an answer from
// GitHub: the connection failed or timed out on every attempt. Retrying
// later may succeed. RetryAt, when set, is the earliest time GitHub said a
// retry could succeed.
type UnknownOutcomeError struct {
	Cause   error
	RetryAt time.Time
}

func (e *UnknownOutcomeError) Error() string { return e.Cause.Error() }

func (e *UnknownOutcomeError) Unwrap() error { return e.Cause }

// retryNotify receives one line per retry wait. Set it with SetRetryNotifier.
var retryNotify atomic.Pointer[func(string)]

// SetRetryNotifier routes retry progress ("retrying in 20s, attempt 3/8")
// to fn, so a long wait doesn't look like a hang. nil silences it.
func SetRetryNotifier(fn func(string)) {
	if fn == nil {
		retryNotify.Store(nil)
		return
	}
	retryNotify.Store(&fn)
}

// retryBudget is shared by every transport of one Client so a run's
// requests agree on when an outage started.
type retryBudget struct {
	policy retryPolicy

	mu sync.Mutex
	// since is when GitHub last stopped answering; zero while it answers.
	since time.Time
	// noticed throttles notices to one per reason per second, since many
	// requests wait out the same outage at once.
	noticed map[string]time.Time
	// retried is the longest outage the run waited through.
	retried time.Duration

	now    func() time.Time
	sleep  func(context.Context, time.Duration)
	jitter func(max time.Duration) time.Duration
	notify func(string)
}

func newRetryBudget() *retryBudget {
	return &retryBudget{
		policy:  runPolicy(),
		noticed: map[string]time.Time{},
		now:     time.Now,
		sleep:   DefaultSleep,
		jitter: func(max time.Duration) time.Duration {
			if max <= 0 {
				return 0
			}
			return rand.N(max + 1)
		},
		notify: func(msg string) {
			if fn := retryNotify.Load(); fn != nil {
				(*fn)(msg)
			}
		},
	}
}

// allow reports whether a wait of d still ends inside the outage window,
// opening the window if this is the first failure since an answer.
func (b *retryBudget) allow(d time.Duration) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if b.since.IsZero() {
		b.since = now
	}
	if now.Add(d).After(b.since.Add(b.policy.window)) {
		return false
	}
	b.retried = max(b.retried, now.Add(d).Sub(b.since))
	return true
}

// RetriedFor is the longest the run spent retrying through one outage.
func (c *Client) RetriedFor() time.Duration {
	if c == nil || c.budget == nil {
		return 0
	}
	c.budget.mu.Lock()
	defer c.budget.mu.Unlock()
	return c.budget.retried
}

// answered closes the outage window: GitHub is responding again.
func (b *retryBudget) answered() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.since = time.Time{}
}

func (b *retryBudget) notice(reason, msg string) {
	b.mu.Lock()
	now := b.now()
	last, seen := b.noticed[reason]
	if seen && now.Sub(last) < time.Second {
		b.mu.Unlock()
		return
	}
	b.noticed[reason] = now
	b.mu.Unlock()
	b.notify(msg)
}

// retryTransport retries requests that ended without an answer: transport
// failures, 5xx, and rate limits. Everything it sends is an idempotent read
// (REST GETs and GraphQL queries), so a retry is always safe.
type retryTransport struct {
	inner  http.RoundTripper
	budget *retryBudget
}

func newRetryTransport(inner http.RoundTripper, budget *retryBudget) *retryTransport {
	if inner == nil {
		inner = http.DefaultTransport
	}
	if budget == nil {
		budget = newRetryBudget()
	}
	return &retryTransport{inner: inner, budget: budget}
}

func (rt *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	noRetry, _ := req.Context().Value(noRetryKey{}).(bool)
	// Snapshot the body so POST requests can be replayed on retry.
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("reading request body for retry: %w", err)
		}
	}
	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithTimeout(req.Context(), requestTimeout)
		try := req.Clone(ctx)
		if body != nil {
			try.Body = io.NopCloser(bytes.NewReader(body))
			try.ContentLength = int64(len(body))
		}
		resp, err := rt.inner.RoundTrip(try)
		if err != nil {
			cancel()
			if req.Context().Err() != nil {
				return nil, req.Context().Err()
			}
			if !retryableTransportErr(err) {
				return nil, err
			}
			if noRetry || !rt.wait(req.Context(), attempt, time.Time{}, "GitHub API unreachable ("+transportReason(err)+")") {
				return nil, &UnknownOutcomeError{Cause: err}
			}
			continue
		}
		if resp.Request == nil {
			resp.Request = try
		}
		if retryAt, ok := retryableResponse(resp, rt.budget.now()); ok {
			reason := fmt.Sprintf("GitHub API unavailable (%d %s)", resp.StatusCode, http.StatusText(resp.StatusCode))
			if resp.StatusCode < 500 {
				reason = "GitHub API rate limit"
			}
			if !noRetry && rt.wait(req.Context(), attempt, retryAt, reason) {
				drain(resp.Body)
				cancel()
				continue
			}
		} else {
			rt.budget.answered()
		}
		resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
		return resp, nil
	}
}

// wait sleeps before the next attempt and reports whether one should be
// made. Backoff is exponential with full jitter; a wait GitHub asked for
// (retryAt) is honored if it ends inside the outage window, and otherwise
// the caller gets the answer now rather than a run that stalls past it.
func (rt *retryTransport) wait(ctx context.Context, attempt int, retryAt time.Time, reason string) bool {
	p := rt.budget.policy
	if attempt >= p.attempts || ctx.Err() != nil {
		return false
	}
	d := rt.budget.jitter(min(p.backoffCap, backoffBase<<(attempt-1)))
	if !retryAt.IsZero() {
		// Never retry a stale reset immediately.
		d = max(retryAt.Sub(rt.budget.now()), backoffBase)
	}
	if !rt.budget.allow(d) {
		return false
	}
	next := fmt.Sprintf("retrying in %s, attempt %d/%d", d.Round(time.Second), attempt+1, p.attempts)
	if !retryAt.IsZero() && d >= time.Minute {
		next = fmt.Sprintf("waiting until %s, attempt %d/%d", rt.budget.now().Add(d).UTC().Format("15:04 UTC"), attempt+1, p.attempts)
	}
	rt.budget.notice(reason, reason+"; "+next)
	rt.budget.sleep(ctx, d)
	return ctx.Err() == nil
}

// transportReason names a transport failure briefly for a progress line.
func transportReason(err error) string {
	var dns *net.DNSError
	var op *net.OpError
	switch {
	case errors.As(err, &dns):
		return "DNS lookup failed"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return "connection reset"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &op) && op.Timeout():
		return "timed out"
	}
	var timeout interface{ Timeout() bool }
	if errors.As(err, &timeout) && timeout.Timeout() {
		return "timed out"
	}
	return "network error"
}

// retryableResponse reports whether resp is an outage or rate limit worth
// retrying, and when GitHub said a retry could succeed (zero if it didn't).
func retryableResponse(resp *http.Response, now time.Time) (time.Time, bool) {
	switch {
	case resp.StatusCode >= 500, resp.StatusCode == http.StatusTooManyRequests, rateLimited403(resp.StatusCode, resp.Header),
		graphQLRateLimitedResponse(resp):
		return retryAfter(resp.Header, now), true
	}
	return time.Time{}, false
}

// graphQLRateLimitedResponse reports a GraphQL response GitHub rejected for
// its rate limit, which arrives as HTTP 200 with a RATE_LIMITED error. It
// reads the body and puts it back for the caller.
func graphQLRateLimitedResponse(resp *http.Response) bool {
	if resp.StatusCode != http.StatusOK || resp.Request == nil || !strings.HasSuffix(resp.Request.URL.Path, "/graphql") {
		return false
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil {
		return false
	}
	var payload struct {
		Errors []struct {
			Type string `json:"type"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return false
	}
	for _, e := range payload.Errors {
		if e.Type == "RATE_LIMITED" {
			return true
		}
	}
	return false
}

// rateLimited403 reports a 403 that is a rate limit rather than a
// permission answer: a secondary limit (Retry-After) or an exhausted
// primary budget.
func rateLimited403(code int, h http.Header) bool {
	return code == http.StatusForbidden && (h.Get("Retry-After") != "" || h.Get("X-RateLimit-Remaining") == "0")
}

// retryAfter returns when a rate-limited request may be retried, from
// Retry-After (seconds or an HTTP date) or X-RateLimit-Reset.
func retryAfter(h http.Header, now time.Time) time.Time {
	if ra := h.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil && secs >= 0 {
			return now.Add(time.Duration(secs) * time.Second)
		}
		if t, err := http.ParseTime(ra); err == nil {
			return t
		}
	}
	if h.Get("X-RateLimit-Remaining") == "0" {
		if ts, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			return time.Unix(ts, 0).Add(time.Second)
		}
	}
	return time.Time{}
}

// RetryAt returns the earliest time GitHub said a retry of the request
// behind err could succeed, or the zero time if it didn't say. For a joined
// error it reads the branch cause picks.
func RetryAt(err error) time.Time {
	if err == nil {
		return time.Time{}
	}
	return retryAt(cause(err))
}

func retryAt(err error) time.Time {
	var unknown *UnknownOutcomeError
	if errors.As(err, &unknown) && !unknown.RetryAt.IsZero() {
		return unknown.RetryAt
	}
	if h, ok := errHeaders(err); ok {
		return retryAfter(h, time.Now())
	}
	return time.Time{}
}

// retryableTransportErr reports whether a transport failure may clear up on
// retry. A certificate GitHub's TLS chain doesn't satisfy is configuration
// (a proxy or CA bundle), not an outage.
func retryableTransportErr(err error) bool {
	return !errors.Is(err, context.Canceled) && !untrustedCert(err)
}

// untrustedCert reports a TLS certificate the client doesn't trust.
func untrustedCert(err error) bool {
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var verify *tls.CertificateVerificationError
	return errors.As(err, &unknownAuthority) || errors.As(err, &hostname) ||
		errors.As(err, &invalid) || errors.As(err, &verify)
}

func drain(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 64<<10))
	body.Close()
}

// cancelOnClose releases an attempt's timeout once the caller has read the
// response, not when RoundTrip returns.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// apiTransport is the base transport for GitHub API requests: the default
// transport, with timeouts so a stalled connection fails instead of hanging
// the run.
func apiTransport(insecure bool) http.RoundTripper {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return http.DefaultTransport
	}
	t = t.Clone()
	t.DialContext = (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext
	t.TLSHandshakeTimeout = tlsHandshakeTimeout
	t.ResponseHeaderTimeout = responseHeaderTimeout
	if insecure {
		t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // integration tests only
	}
	return t
}
