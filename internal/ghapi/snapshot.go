package ghapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

type snapshotKey struct{}

// WithSnapshotRedirects limits API redirects to the same origin and preserves
// any pinned ref query. Repository renames remain supported without allowing
// a verification to read content from another host or a mutable default branch.
func WithSnapshotRedirects(ctx context.Context) context.Context {
	return context.WithValue(ctx, snapshotKey{}, true)
}

type snapshotTransport struct{ inner http.RoundTripper }

func (t snapshotTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.inner.RoundTrip(req)
	if err != nil || req.Context().Value(snapshotKey{}) != true {
		return resp, err
	}
	switch resp.StatusCode {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
	default:
		return resp, nil
	}
	location := resp.Header.Get("Location")
	if location == "" {
		return resp, nil
	}
	next, err := req.URL.Parse(location)
	if err == nil && next.User == nil && next.Scheme == req.URL.Scheme &&
		strings.EqualFold(next.Host, req.URL.Host) {
		ref := req.URL.Query().Get("ref")
		if ref == "" || next.Query().Get("ref") == ref {
			return resp, nil
		}
	}
	resp.Body.Close()
	return nil, fmt.Errorf("refusing API redirect that changes the snapshot origin or ref")
}
