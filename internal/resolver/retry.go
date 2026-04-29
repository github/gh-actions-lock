package resolver

import (
	"math"
	"net/http"
	"time"
)

// retryTransport wraps an http.RoundTripper with retry logic for transient
// server errors (5xx) and rate limits (429).
type retryTransport struct {
	inner      http.RoundTripper
	maxRetries int
}

// newRetryTransport wraps t with up to maxRetries retries and exponential backoff.
func newRetryTransport(t http.RoundTripper, maxRetries int) http.RoundTripper {
	if t == nil {
		t = http.DefaultTransport
	}
	return &retryTransport{inner: t, maxRetries: maxRetries}
}

func (rt *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var resp *http.Response
	var err error

	for attempt := 0; attempt <= rt.maxRetries; attempt++ {
		resp, err = rt.inner.RoundTrip(req)
		if err != nil {
			// Network-level error — retry.
			if attempt < rt.maxRetries {
				rt.backoff(attempt)
				continue
			}
			return nil, err
		}
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			if attempt < rt.maxRetries {
				resp.Body.Close()
				rt.backoff(attempt)
				continue
			}
		}
		return resp, nil
	}
	return resp, err
}

func (rt *retryTransport) backoff(attempt int) {
	delay := time.Duration(math.Pow(2, float64(attempt))) * 500 * time.Millisecond
	if delay > 5*time.Second {
		delay = 5 * time.Second
	}
	time.Sleep(delay)
}
