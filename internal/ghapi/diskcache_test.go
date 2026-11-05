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
