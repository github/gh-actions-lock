package ghapi

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// --- retry transport tests ---

func TestShouldRetry_429(t *testing.T) {
	rt := &retryTransport{maxRetries: 3}
	resp := &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}}
	if !rt.shouldRetry(resp) {
		t.Fatal("expected 429 to be retryable")
	}
}

func TestShouldRetry_5xx(t *testing.T) {
	rt := &retryTransport{maxRetries: 3}
	for _, code := range []int{500, 502, 503, 504} {
		resp := &http.Response{StatusCode: code, Header: http.Header{}}
		if !rt.shouldRetry(resp) {
			t.Fatalf("expected %d to be retryable", code)
		}
	}
}

func TestShouldRetry_403WithRetryAfter(t *testing.T) {
	rt := &retryTransport{maxRetries: 3}
	resp := &http.Response{
		StatusCode: http.StatusForbidden,
		Header:     http.Header{"Retry-After": []string{"60"}},
	}
	if !rt.shouldRetry(resp) {
		t.Fatal("expected 403 with Retry-After to be retryable")
	}
}

func TestShouldRetry_403WithRateLimitReset(t *testing.T) {
	rt := &retryTransport{maxRetries: 3}
	resp := &http.Response{
		StatusCode: http.StatusForbidden,
		Header: http.Header{
			"X-Ratelimit-Reset":     []string{fmt.Sprintf("%d", time.Now().Add(30*time.Second).Unix())},
			"X-Ratelimit-Remaining": []string{"0"},
		},
	}
	if !rt.shouldRetry(resp) {
		t.Fatal("expected 403 with rate limit headers to be retryable")
	}
}

func TestShouldRetry_403NoHeaders(t *testing.T) {
	rt := &retryTransport{maxRetries: 3}
	resp := &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{}}
	if rt.shouldRetry(resp) {
		t.Fatal("expected plain 403 (no rate limit headers) to NOT be retryable")
	}
}

func TestShouldRetry_200(t *testing.T) {
	rt := &retryTransport{maxRetries: 3}
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}}
	if rt.shouldRetry(resp) {
		t.Fatal("expected 200 to NOT be retryable")
	}
}

func TestRetryWait_RetryAfterHeader(t *testing.T) {
	rt := &retryTransport{maxRetries: 3}
	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Retry-After": []string{"5"}},
	}
	got := rt.retryWait(resp, 0)
	if got != 5*time.Second {
		t.Fatalf("expected 5s, got %v", got)
	}
}

func TestRetryWait_RateLimitResetHeader(t *testing.T) {
	rt := &retryTransport{maxRetries: 3}
	resetAt := time.Now().Add(10 * time.Second)
	resp := &http.Response{
		StatusCode: http.StatusForbidden,
		Header: http.Header{
			"X-Ratelimit-Reset":     []string{fmt.Sprintf("%d", resetAt.Unix())},
			"X-Ratelimit-Remaining": []string{"0"},
		},
	}
	got := rt.retryWait(resp, 0)
	// Should be approximately 11s (10s until reset + 1s buffer).
	if got < 9*time.Second || got > 15*time.Second {
		t.Fatalf("expected ~11s, got %v", got)
	}
}

func TestRetryWait_ExponentialBackoff(t *testing.T) {
	rt := &retryTransport{maxRetries: 3}
	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Header:     http.Header{},
	}
	for attempt, want := range []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
	} {
		got := rt.retryWait(resp, attempt)
		if got != want {
			t.Fatalf("attempt %d: expected %v, got %v", attempt, want, got)
		}
	}
}

func TestRetryWait_CappedAt10s(t *testing.T) {
	rt := &retryTransport{maxRetries: 10}
	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Header:     http.Header{},
	}
	got := rt.retryWait(resp, 10) // 2^10 = 1024s, should cap at 10s
	if got != 10*time.Second {
		t.Fatalf("expected cap at 10s, got %v", got)
	}
}

func TestRetryTransport_ResendsBodyOnRetry(t *testing.T) {
	wantBody := `{"query":"some graphql"}`
	var attempts atomic.Int32

	inner := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		n := attempts.Add(1)
		got, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("attempt %d: reading body: %v", n, err)
		}
		if string(got) != wantBody {
			t.Fatalf("attempt %d: body = %q, want %q", n, string(got), wantBody)
		}
		if n < 3 {
			return &http.Response{
				StatusCode: http.StatusInternalServerError,
				Header:     http.Header{},
				Body:       io.NopCloser(bytes.NewReader(nil)),
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{},
			Body:       io.NopCloser(bytes.NewReader([]byte(`{"data":{}}`))),
		}, nil
	})

	rt := &retryTransport{inner: inner, maxRetries: 3, sleepFn: func(context.Context, time.Duration) {}}
	req, _ := http.NewRequest("POST", "https://api.github.com/graphql",
		io.NopCloser(bytes.NewReader([]byte(wantBody))))
	req.ContentLength = int64(len(wantBody))

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("expected 3 attempts, got %d", got)
	}
}

// roundTripFunc adapts a function into an http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

