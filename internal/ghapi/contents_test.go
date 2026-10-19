package ghapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/github/gh-actions-lock/internal/filesource"
	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newContentsClient(t *testing.T, reg *httpmock.Registry) *Client {
	t.Helper()
	c, err := New("github.com", WithClientTransport(reg))
	require.NoError(t, err)
	return c
}

func TestReadFile(t *testing.T) {
	body := "name: ci\non: push\n"
	encoded := base64.StdEncoding.EncodeToString([]byte(body))
	// The API wraps base64 at 60 columns.
	wrapped := encoded[:10] + "\n" + encoded[10:]

	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.RESTWithQuery(http.MethodGet, `^/repos/octo/api/contents/\.github/workflows/ci\.yml$`, "ref=abc123"),
		httpmock.JSONResponse(map[string]any{"type": "file", "encoding": "base64", "content": wrapped}),
	)
	c := newContentsClient(t, reg)

	got, err := c.ReadFile(context.Background(), "octo", "api", "abc123", ".github/workflows/ci.yml")
	require.NoError(t, err)
	assert.Equal(t, body, string(got), "ref is sent and wrapped base64 is decoded")
	reg.Verify(t)
}

func TestReadFile_Errors(t *testing.T) {
	tests := []struct {
		name     string
		respond  httpmock.Responder
		notExist bool
	}{
		{name: "404 wraps fs.ErrNotExist", respond: httpmock.StatusResponse(http.StatusNotFound), notExist: true},
		{name: "server error is not a not-exist", respond: httpmock.StatusResponse(http.StatusInternalServerError)},
		{name: "directory rejected", respond: httpmock.JSONResponse([]map[string]any{{"name": "a", "type": "file"}})},
		{name: "unsupported encoding", respond: httpmock.JSONResponse(map[string]any{"type": "file", "encoding": "none", "content": ""})},
		{name: "symlink rejected", respond: httpmock.JSONResponse(map[string]any{"type": "symlink"})},
		{name: "invalid base64", respond: httpmock.JSONResponse(map[string]any{"type": "file", "encoding": "base64", "content": "%%%"})},
		{name: "unauthorized is not absence", respond: httpmock.StatusResponse(http.StatusUnauthorized)},
		{name: "forbidden is not absence", respond: httpmock.StatusResponse(http.StatusForbidden)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := &httpmock.Registry{}
			reg.Register(httpmock.REST(http.MethodGet, `/contents/`), tt.respond)
			c := newContentsClient(t, reg)

			_, err := c.ReadFile(context.Background(), "octo", "api", "main", "x.yml")
			require.Error(t, err)
			assert.Equal(t, tt.notExist, errorsIsNotExist(err))
		})
	}
}

func TestListDir_RejectsIncompleteOrMalformedInventory(t *testing.T) {
	tests := []struct {
		name  string
		items []map[string]any
	}{
		{name: "path traversal", items: []map[string]any{{"name": "../ci.yml", "path": ".github/ci.yml", "type": "file"}}},
		{name: "mismatched path", items: []map[string]any{{"name": "ci.yml", "path": ".github/elsewhere/ci.yml", "type": "file"}}},
		{name: "missing name", items: []map[string]any{{"path": ".github/workflows/ci.yml", "type": "file"}}},
		{name: "missing type", items: []map[string]any{{"name": "ci.yml", "path": ".github/workflows/ci.yml"}}},
	}
	limit := make([]map[string]any, 1000)
	for i := range limit {
		name := fmt.Sprintf("ci-%d.yml", i)
		limit[i] = map[string]any{"name": name, "path": ".github/workflows/" + name, "type": "file"}
	}
	t.Run("999 entries remain supported", func(t *testing.T) {
		reg := &httpmock.Registry{}
		reg.Register(httpmock.REST(http.MethodGet, `/contents/`), httpmock.JSONResponse(limit[:999]))
		entries, err := newContentsClient(t, reg).ListDir(context.Background(), "octo", "api", remoteContentsSHA, ".github/workflows")
		require.NoError(t, err)
		assert.Len(t, entries, 999)
		reg.Verify(t)
	})
	tests = append(tests, struct {
		name  string
		items []map[string]any
	}{name: "potentially truncated inventory", items: limit})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := &httpmock.Registry{}
			reg.Register(httpmock.REST(http.MethodGet, `/contents/`), httpmock.JSONResponse(tt.items))
			_, err := newContentsClient(t, reg).ListDir(context.Background(), "octo", "api", remoteContentsSHA, ".github/workflows")
			require.Error(t, err)
			assert.NotErrorIs(t, err, fs.ErrNotExist)
			reg.Verify(t)
		})
	}
}

const remoteContentsSHA = "0123456789abcdef0123456789abcdef01234567"

func errorsIsNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }

func TestListDir(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.RESTWithQuery(http.MethodGet, `^/repos/octo/api/contents/\.github/workflows$`, "ref=abc123"),
		httpmock.JSONResponse([]map[string]any{
			{"name": "ci.yml", "path": ".github/workflows/ci.yml", "type": "file"},
			{"name": "sub", "path": ".github/workflows/sub", "type": "dir"},
		}),
	)
	c := newContentsClient(t, reg)

	got, err := c.ListDir(context.Background(), "octo", "api", "abc123", ".github/workflows")
	require.NoError(t, err)
	assert.Equal(t, []filesource.Entry{
		{Name: "ci.yml", Path: ".github/workflows/ci.yml"},
		{Name: "sub", Path: ".github/workflows/sub", IsDir: true},
	}, got)
	reg.Verify(t)
}

func TestListDir_Errors(t *testing.T) {
	t.Run("404 wraps fs.ErrNotExist", func(t *testing.T) {
		reg := &httpmock.Registry{}
		reg.Register(httpmock.REST(http.MethodGet, `/contents/`), httpmock.StatusResponse(http.StatusNotFound))
		_, err := newContentsClient(t, reg).ListDir(context.Background(), "octo", "api", "main", ".github/workflows")
		require.Error(t, err)
		assert.True(t, errorsIsNotExist(err))
	})
	t.Run("file rejected", func(t *testing.T) {
		reg := &httpmock.Registry{}
		reg.Register(httpmock.REST(http.MethodGet, `/contents/`), httpmock.JSONResponse(map[string]any{"type": "file"}))
		_, err := newContentsClient(t, reg).ListDir(context.Background(), "octo", "api", "main", "x")
		require.Error(t, err)
		assert.False(t, errorsIsNotExist(err))
	})
}

func samlTransport() roundTripFunc {
	return func(req *http.Request) (*http.Response, error) {
		resp, _ := jsonHTTP(map[string]any{"message": "Resource protected by organization SAML enforcement. You must grant your Personal Access token access to this organization."})
		resp.StatusCode = http.StatusForbidden
		resp.Request = req
		return resp, nil
	}
}

func TestContents_SSOAnonymousFallback(t *testing.T) {
	body := "name: ci\n"
	file := map[string]any{"type": "file", "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(body))}
	dir := []map[string]any{{"name": "ci.yml", "path": ".github/workflows/ci.yml", "type": "file"}}

	tests := []struct {
		name   string
		status int
		check  func(t *testing.T, c *Client)
	}{
		{
			name:   "file read recovers anonymously",
			status: http.StatusOK,
			check: func(t *testing.T, c *Client) {
				got, err := c.ReadFile(context.Background(), "acme", "widget", "abc", ".github/workflows/ci.yml")
				require.NoError(t, err)
				assert.Equal(t, body, string(got))
			},
		},
		{
			name:   "directory listing recovers anonymously",
			status: http.StatusOK,
			check: func(t *testing.T, c *Client) {
				got, err := c.ListDir(context.Background(), "acme", "widget", "abc", ".github/workflows")
				require.NoError(t, err)
				assert.Equal(t, []filesource.Entry{{Name: "ci.yml", Path: ".github/workflows/ci.yml"}}, got)
			},
		},
		{
			name:   "anonymous 404 still maps to not-exist",
			status: http.StatusNotFound,
			check: func(t *testing.T, c *Client) {
				_, err := c.ReadFile(context.Background(), "acme", "widget", "abc", "missing.yml")
				assert.ErrorIs(t, err, fs.ErrNotExist)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			anonProbeCache = sync.Map{}
			anonRateLimited = sync.Map{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodHead:
					w.WriteHeader(http.StatusOK)
				case tt.status != http.StatusOK:
					w.WriteHeader(tt.status)
				case strings.HasSuffix(r.URL.Path, "/workflows"):
					_ = json.NewEncoder(w).Encode(dir)
				default:
					_ = json.NewEncoder(w).Encode(file)
				}
			}))
			defer srv.Close()
			c, err := New("github.com", WithClientTransport(samlTransport()))
			require.NoError(t, err)
			c.anonBaseURL = srv.URL
			c.anonHTTP = srv.Client()

			tt.check(t, c)
		})
	}
}

func TestContents_SSOWithoutAnonymousAccessKeepsSAMLError(t *testing.T) {
	anonProbeCache = sync.Map{}
	anonRateLimited = sync.Map{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c, err := New("github.com", WithClientTransport(samlTransport()))
	require.NoError(t, err)
	c.anonBaseURL = srv.URL
	c.anonHTTP = srv.Client()

	_, err = c.ReadFile(context.Background(), "private-org", "widget", "abc", "a.yml")
	require.Error(t, err)
	assert.True(t, IsSAMLEnforcement(err))
	assert.NotErrorIs(t, err, fs.ErrNotExist)
}
