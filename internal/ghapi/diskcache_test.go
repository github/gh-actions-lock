package ghapi

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPublicCachePersistsPartialProgress(t *testing.T) {
	dir := t.TempDir()
	calls := map[string]int{}
	inner := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls[req.URL.Path]++
		code := http.StatusOK
		if strings.HasSuffix(req.URL.Path, "/second") && calls[req.URL.Path] == 1 {
			code = http.StatusTooManyRequests
		}
		return &http.Response{StatusCode: code, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(req.URL.Path))}, nil
	})
	for range 2 {
		cache := publicCacheTransport{inner: inner, dir: dir, ttl: time.Hour}
		for _, path := range []string{"first", "second"} {
			req, err := http.NewRequest(http.MethodGet, "https://api.github.com/repos/ocean/"+path, nil)
			require.NoError(t, err)
			resp, err := cache.RoundTrip(req)
			require.NoError(t, err)
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			assert.Equal(t, req.URL.Path, string(body))
		}
	}
	assert.Equal(t, 1, calls["/repos/ocean/first"])
	assert.Equal(t, 2, calls["/repos/ocean/second"])
}

func TestPublicCacheRefreshesInvalidEntries(t *testing.T) {
	for _, scenario := range []string{"expired", "future", "corrupt"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			calls := 0
			cache := publicCacheTransport{dir: dir, ttl: time.Hour, inner: roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
			})}
			req, err := http.NewRequest(http.MethodGet, "https://api.github.com/repos/ocean/actions", nil)
			require.NoError(t, err)
			resp, err := cache.RoundTrip(req)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			files, err := os.ReadDir(dir)
			require.NoError(t, err)
			require.Len(t, files, 1)
			path := filepath.Join(dir, files[0].Name())
			switch scenario {
			case "expired":
				old := time.Now().Add(-2 * time.Hour)
				require.NoError(t, os.Chtimes(path, old, old))
			case "future":
				future := time.Now().Add(time.Hour)
				require.NoError(t, os.Chtimes(path, future, future))
			case "corrupt":
				require.NoError(t, os.WriteFile(path, []byte("broken"), 0600))
			}
			resp, err = cache.RoundTrip(req)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			assert.Equal(t, 2, calls)
		})
	}
}

func TestPublicCacheEarlyRefresh(t *testing.T) {
	for _, scenario := range []struct {
		name       string
		age        time.Duration
		selected   bool
		code       int
		header     http.Header
		err        error
		incomplete bool
		cancel     bool
		wantBody   string
		wantError  error
		wantCalls  int
	}{
		{name: "fresh", age: time.Hour, selected: true, code: 200, wantBody: "cached", wantCalls: 0},
		{name: "not selected", age: 20 * time.Hour, code: 200, wantBody: "cached", wantCalls: 0},
		{name: "refreshed", age: 20 * time.Hour, selected: true, code: 200, wantBody: "updated", wantCalls: 1},
		{name: "primary rate limit", age: 20 * time.Hour, selected: true, code: 403, header: http.Header{"X-Ratelimit-Remaining": {"0"}}, wantBody: "cached", wantCalls: 1},
		{name: "secondary rate limit", age: 20 * time.Hour, selected: true, code: 403, header: http.Header{"Retry-After": {"60"}}, wantBody: "cached", wantCalls: 1},
		{name: "too many requests", age: 20 * time.Hour, selected: true, code: 429, wantBody: "cached", wantCalls: 1},
		{name: "server error", age: 20 * time.Hour, selected: true, code: 503, wantBody: "cached", wantCalls: 1},
		{name: "network error", age: 20 * time.Hour, selected: true, err: io.ErrUnexpectedEOF, wantBody: "cached", wantCalls: 1},
		{name: "incomplete body", age: 20 * time.Hour, selected: true, code: 200, incomplete: true, wantBody: "cached", wantCalls: 1},
		{name: "deleted", age: 20 * time.Hour, selected: true, code: 404, wantBody: "updated", wantCalls: 1},
		{name: "forbidden", age: 20 * time.Hour, selected: true, code: 403, wantBody: "updated", wantCalls: 1},
		{name: "expired", age: 25 * time.Hour, selected: true, err: io.ErrUnexpectedEOF, wantError: io.ErrUnexpectedEOF, wantCalls: 1},
		{name: "canceled", age: 20 * time.Hour, selected: true, cancel: true, err: context.Canceled, wantError: context.Canceled, wantCalls: 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			dir := t.TempDir()
			cache := publicCacheTransport{dir: dir, ttl: 24 * time.Hour, inner: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("cached"))}, nil
			})}
			req, err := http.NewRequest(http.MethodGet, "https://api.github.com/repos/ocean/actions", nil)
			require.NoError(t, err)
			resp, err := cache.RoundTrip(req)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			files, err := os.ReadDir(dir)
			require.NoError(t, err)
			require.Len(t, files, 1)
			path := filepath.Join(dir, files[0].Name())
			old := time.Now().Add(-scenario.age)
			require.NoError(t, os.Chtimes(path, old, old))
			before, err := os.Stat(path)
			require.NoError(t, err)
			calls := 0
			cache.refresh = func() bool { return scenario.selected }
			cache.inner = roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				if scenario.err != nil {
					return nil, scenario.err
				}
				var body io.ReadCloser = io.NopCloser(strings.NewReader("updated"))
				if scenario.incomplete {
					body = io.NopCloser(iotest.ErrReader(io.ErrUnexpectedEOF))
				}
				return &http.Response{StatusCode: scenario.code, Header: scenario.header, Body: body}, nil
			})
			if scenario.cancel {
				ctx, cancel := context.WithCancel(req.Context())
				cancel()
				req = req.WithContext(ctx)
			}
			resp, err = cache.RoundTrip(req)
			assert.Equal(t, scenario.wantCalls, calls)
			if scenario.wantError != nil {
				require.ErrorIs(t, err, scenario.wantError)
				return
			}
			require.NoError(t, err)
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			assert.Equal(t, scenario.wantBody, string(body))
			if scenario.wantBody == "cached" {
				assert.Equal(t, http.StatusOK, resp.StatusCode)
				after, err := os.Stat(path)
				require.NoError(t, err)
				assert.Equal(t, before.ModTime(), after.ModTime())
			} else if scenario.code == http.StatusOK {
				after, err := os.Stat(path)
				require.NoError(t, err)
				assert.True(t, after.ModTime().After(before.ModTime()))
				cache.refresh = nil
				resp, err = cache.RoundTrip(req)
				require.NoError(t, err)
				body, err = io.ReadAll(resp.Body)
				require.NoError(t, err)
				require.NoError(t, resp.Body.Close())
				assert.Equal(t, "updated", string(body))
				assert.Equal(t, scenario.wantCalls, calls)
			} else {
				assert.Equal(t, scenario.code, resp.StatusCode)
				_, err := os.Stat(path)
				assert.ErrorIs(t, err, os.ErrNotExist)
			}
		})
	}
}

func TestPublicCacheSkipsOtherRequests(t *testing.T) {
	for _, scenario := range []string{"authorization", "cookie", "http", "tenant", "graphql", "post", "accept", "api-version"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			cache := publicCacheTransport{dir: t.TempDir(), ttl: time.Hour, inner: roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
			})}
			for i := range 2 {
				req, err := http.NewRequest(http.MethodGet, "https://api.github.com/repos/ocean/actions", nil)
				require.NoError(t, err)
				switch scenario {
				case "authorization":
					req.Header.Set("Authorization", "token test")
				case "cookie":
					req.Header.Set("Cookie", "session=test")
				case "http":
					req.URL.Scheme = "http"
				case "tenant":
					req.URL.Host = "api.tenant.ghe.com"
				case "graphql":
					req.URL.Path = "/graphql"
				case "post":
					req.Method = http.MethodPost
				case "accept":
					req.Header.Set("Accept", []string{"application/json", "application/vnd.github.raw+json"}[i])
				case "api-version":
					req.Header.Set("X-GitHub-Api-Version", []string{"2022-11-28", "2026-03-10"}[i])
				}
				resp, err := cache.RoundTrip(req)
				require.NoError(t, err)
				require.NoError(t, resp.Body.Close())
			}
			assert.Equal(t, 2, calls)
		})
	}
}

func TestPublicCacheDoesNotStoreErrors(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			dir := t.TempDir()
			cache := publicCacheTransport{dir: dir, ttl: time.Hour, inner: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: code, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("error"))}, nil
			})}
			req, err := http.NewRequest(http.MethodGet, "https://api.github.com/repos/ocean/actions", nil)
			require.NoError(t, err)
			resp, err := cache.RoundTrip(req)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			files, err := os.ReadDir(dir)
			require.NoError(t, err)
			assert.Empty(t, files)
		})
	}
}

func TestClientPublicCache(t *testing.T) {
	t.Setenv("GH_ACTIONS_LOCK_CACHE_DIR", t.TempDir())
	t.Setenv("GH_ACTIONS_LOCK_CACHE_TTL", "1h")
	t.Setenv(DotcomTokenEnv, "")
	t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "0")
	calls := map[string]int{}
	inner := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls[req.URL.Host]++
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"id":42}`))}, nil
	})
	for range 2 {
		client, err := New("tenant.ghe.com", WithClientTransport(inner))
		require.NoError(t, err)
		var result struct{ ID int }
		require.NoError(t, client.public.rest.Get("repos/ocean/actions", &result))
		assert.Equal(t, 42, result.ID)
		require.NoError(t, client.public.anonGet(context.Background(), "repos/ocean/actions", &result))
		require.NoError(t, client.local.rest.Get("repos/ocean/actions", &result))
	}
	assert.Equal(t, 2, calls["api.github.com"])
	assert.Equal(t, 2, calls["api.tenant.ghe.com"])
}

func TestPublicCacheIncompleteBody(t *testing.T) {
	dir := t.TempDir()
	cache := publicCacheTransport{dir: dir, ttl: time.Hour, inner: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(iotest.ErrReader(io.ErrUnexpectedEOF))}, nil
	})}
	req, err := http.NewRequest(http.MethodGet, "https://api.github.com/repos/ocean/actions", nil)
	require.NoError(t, err)
	_, err = cache.RoundTrip(req)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	files, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, files)
}

func TestPublicCacheConcurrentWriters(t *testing.T) {
	cache := publicCacheTransport{dir: t.TempDir(), ttl: time.Hour, inner: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("complete response"))}, nil
	})}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			req, err := http.NewRequest(http.MethodGet, "https://api.github.com/repos/ocean/actions", nil)
			assert.NoError(t, err)
			resp, err := cache.RoundTrip(req)
			if !assert.NoError(t, err) {
				return
			}
			body, err := io.ReadAll(resp.Body)
			assert.NoError(t, err)
			assert.NoError(t, resp.Body.Close())
			assert.Equal(t, "complete response", string(body))
		})
	}
	wg.Wait()
	files, err := os.ReadDir(cache.dir)
	require.NoError(t, err)
	assert.Len(t, files, 1)
}

func TestPublicCacheConfiguration(t *testing.T) {
	t.Setenv("GH_ACTIONS_LOCK_CACHE_DIR", t.TempDir())
	for _, ttl := range []string{"bad", "0s", "-1h"} {
		t.Run(ttl, func(t *testing.T) {
			t.Setenv("GH_ACTIONS_LOCK_CACHE_TTL", ttl)
			_, err := withPublicCache(http.DefaultTransport)
			require.ErrorContains(t, err, "positive duration")
		})
	}
}
