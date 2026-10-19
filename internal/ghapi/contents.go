package ghapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/github/gh-actions-lock/internal/filesource"
)

// ReadFile returns the contents of path in owner/repo at ref. A missing file
// wraps fs.ErrNotExist. Pass a commit SHA as ref for a consistent snapshot.
func (c *Client) ReadFile(ctx context.Context, owner, repo, ref, path string) ([]byte, error) {
	raw, err := c.contents(ctx, owner, repo, ref, path)
	if err != nil {
		return nil, err
	}
	var file struct {
		Type     string `json:"type"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("%s: %s is not a file", path, describeContents(raw))
	}
	if file.Type != "file" {
		return nil, fmt.Errorf("%s is not a file (type %q)", path, file.Type)
	}
	if file.Encoding != "base64" {
		return nil, fmt.Errorf("%s: unsupported encoding %q", path, file.Encoding)
	}
	data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(file.Content, "\n", ""))
	if err != nil {
		return nil, fmt.Errorf("decoding %s: %w", path, err)
	}
	return data, nil
}

// ListDir lists the immediate children of dir in owner/repo at ref. A missing
// directory wraps fs.ErrNotExist.
func (c *Client) ListDir(ctx context.Context, owner, repo, ref, dir string) ([]filesource.Entry, error) {
	raw, err := c.contents(ctx, owner, repo, ref, dir)
	if err != nil {
		return nil, err
	}
	var items []struct {
		Name string `json:"name"`
		Path string `json:"path"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	// The Contents API silently truncates directories at 1,000 entries.
	// A partial inventory must not produce a successful verification.
	if len(items) >= 1000 {
		return nil, fmt.Errorf("%s: directory listing may be truncated at the Contents API limit of 1000 entries", dir)
	}
	entries := make([]filesource.Entry, 0, len(items))
	for _, it := range items {
		if it.Name == "" || it.Name == "." || it.Name == ".." ||
			strings.ContainsAny(it.Name, "/\\") || it.Path != path.Join(dir, it.Name) || it.Type == "" {
			return nil, fmt.Errorf("%s: invalid directory entry %q", dir, it.Name)
		}
		entries = append(entries, filesource.Entry{Name: it.Name, Path: it.Path, IsDir: it.Type == "dir"})
	}
	return entries, nil
}

func describeContents(raw json.RawMessage) string {
	if len(raw) > 0 && raw[0] == '[' {
		return "a directory"
	}
	return "unexpected content"
}

func (c *Client) contents(ctx context.Context, owner, repo, ref, path string) (json.RawMessage, error) {
	c, err := c.ForRepo(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	segs := strings.Split(strings.Trim(path, "/"), "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	endpoint := fmt.Sprintf("repos/%s/%s/contents/%s?ref=%s",
		url.PathEscape(owner), url.PathEscape(repo), strings.Join(segs, "/"), url.QueryEscape(ref))

	var raw json.RawMessage
	err = c.rest.DoWithContext(ctx, http.MethodGet, endpoint, nil, &raw)
	if err != nil && IsSAMLEnforcement(err) && c.SSOFallbackEligible(ctx, owner) {
		err = c.anonGet(ctx, endpoint, &raw)
	}
	if err != nil {
		var httpErr *api.HTTPError
		if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("%s/%s@%s:%s: %w", owner, repo, ref, path, fs.ErrNotExist)
		}
		return nil, fmt.Errorf("reading %s/%s@%s:%s: %w", owner, repo, ref, path, c.ssoErr(owner, err))
	}
	return raw, nil
}
