package ghapi

import (
	"context"
	"encoding/base64"
	"errors"
	"io/fs"
	"net/http"
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
