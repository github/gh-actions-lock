package ghapi

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func withPublicCache(inner http.RoundTripper) (http.RoundTripper, error) {
	dir := os.Getenv("GH_ACTIONS_LOCK_CACHE_DIR")
	if dir == "" {
		return inner, nil
	}
	ttl := 24 * time.Hour
	if value := os.Getenv("GH_ACTIONS_LOCK_CACHE_TTL"); value != "" {
		var err error
		ttl, err = time.ParseDuration(value)
		if err != nil || ttl <= 0 {
			return nil, fmt.Errorf("GH_ACTIONS_LOCK_CACHE_TTL must be a positive duration, got %q", value)
		}
	}
	return publicCacheTransport{
		inner:   inner,
		dir:     filepath.Join(dir, "v1"),
		ttl:     ttl,
		refresh: func() bool { return rand.IntN(100) == 0 },
	}, nil
}

type publicCacheTransport struct {
	inner   http.RoundTripper
	dir     string
	ttl     time.Duration
	refresh func() bool
}

func (t publicCacheTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet || req.URL.Scheme != "https" || req.URL.Host != "api.github.com" ||
		!strings.HasPrefix(req.URL.Path, "/repos/") || req.Header.Get("Authorization") != "" || req.Header.Get("Cookie") != "" {
		return t.inner.RoundTrip(req)
	}
	key := sha256.Sum256([]byte(req.URL.String() + "\n" + req.Header.Get("Accept") + "\n" + req.Header.Get("X-GitHub-Api-Version")))
	path := filepath.Join(t.dir, fmt.Sprintf("%x", key))
	var cached *http.Response
	if info, err := os.Stat(path); err == nil {
		age := time.Since(info.ModTime())
		if age >= 0 && age < t.ttl {
			if data, err := os.ReadFile(path); err == nil {
				if resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(data)), req); err == nil && resp.StatusCode == http.StatusOK {
					if age < t.ttl/2 || t.refresh == nil || !t.refresh() {
						return resp, nil
					}
					cached = resp
				}
			}
		}
	}
	resp, err := t.fetch(req, path)
	if cached != nil {
		retryable := err != nil
		if resp != nil {
			_, retryable = retryableResponse(resp, time.Now())
		}
		if retryable && req.Context().Err() == nil {
			if resp != nil {
				resp.Body.Close()
			}
			return cached, nil
		}
		cached.Body.Close()
		if err == nil && resp.StatusCode != http.StatusOK {
			os.Remove(path)
		}
	}
	return resp, err
}

func (t publicCacheTransport) fetch(req *http.Request, path string) (*http.Response, error) {
	resp, err := t.inner.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		return resp, err
	}
	data, err := httputil.DumpResponse(resp, true)
	if err != nil {
		resp.Body.Close()
		return nil, err
	}
	t.store(path, data)
	return resp, nil
}

func (t publicCacheTransport) store(path string, data []byte) {
	if err := os.MkdirAll(t.dir, 0700); err != nil {
		return
	}
	file, err := os.CreateTemp(t.dir, ".response-*")
	if err != nil {
		return
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr == nil && closeErr == nil {
		os.Rename(file.Name(), path)
	}
}
