package tag

import (
	"context"
	"testing"
	"time"

	"github.com/github/gh-actions-lock/internal/ghapi"
	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckCandidateAge(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		date time.Time
		days int
		want bool
	}{
		{"exact boundary passes", now.Add(-7 * 24 * time.Hour), 7, false},
		{"one second too young fails", now.Add(-7*24*time.Hour + time.Second), 7, true},
		{"future timestamp fails", now.Add(time.Hour), 7, true},
		{"large policy does not overflow", now.Add(-30 * 24 * time.Hour), int(^uint(0) >> 1), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkCandidateAge(tc.date, "test date", tc.days, now)
			assert.Equal(t, tc.want, err != nil)
		})
	}
}

func TestCheckAdoption(t *testing.T) {
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	old := now.Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	fresh := now.Add(-time.Hour).Format(time.RFC3339)
	for _, tc := range []struct {
		name          string
		commitDate    string
		releaseDate   string
		hasRelease    bool
		ref           string
		releaseStatus int
		want          string
	}{
		{name: "old release and commit pass", commitDate: old, releaseDate: old, hasRelease: true},
		{name: "fresh release on old commit fails", commitDate: old, releaseDate: fresh, hasRelease: true, want: "release v1.0.0 published_at"},
		{name: "moved old release to fresh commit fails", commitDate: fresh, releaseDate: old, hasRelease: true, want: "commit committer date"},
		{name: "no release uses committer date", commitDate: old},
		{name: "branch with fresh commit fails", ref: "main", commitDate: fresh, want: "commit committer date"},
		{name: "SHA with fresh release fails", ref: sha, commitDate: old, releaseDate: fresh, hasRelease: true, want: "release v1.0.0 published_at"},
		{name: "missing commit date fails", want: "missing or invalid commit committer date"},
		{name: "invalid commit date fails", commitDate: "not a date", want: "missing or invalid commit committer date"},
		{name: "missing release date fails", commitDate: old, hasRelease: true, want: "missing or invalid release"},
		{name: "invalid release date fails", commitDate: old, releaseDate: "not a date", hasRelease: true, want: "missing or invalid release"},
		{name: "release API failure fails closed", commitDate: old, releaseStatus: 403, want: "cannot establish cooldown release age"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := &httpmock.Registry{}
			reg.Register(httpmock.REST("GET", `/commits/`+sha+`$`), httpmock.JSONResponse(map[string]any{
				"sha": sha, "commit": map[string]any{"committer": map[string]any{"date": tc.commitDate}},
			}))
			reg.Register(httpmock.REST("GET", `/tags$`), httpmock.JSONResponse(httpmock.TagListResponse("v1.0.0", sha)))
			var releases []map[string]any
			if tc.hasRelease {
				releases = append(releases, map[string]any{"tag_name": "v1.0.0", "published_at": tc.releaseDate})
			}
			if tc.releaseStatus != 0 {
				reg.Register(httpmock.REST("GET", `/releases$`), httpmock.StatusResponse(tc.releaseStatus))
			} else {
				reg.Register(httpmock.REST("GET", `/releases$`), httpmock.JSONResponse(releases))
			}
			// Branches have no release; the matching commit tag still can.
			for range 2 {
				reg.Register(httpmock.REST("GET", `/releases/tags/`), httpmock.StatusResponse(404))
			}
			client, err := ghapi.New("github.com", ghapi.WithClientTransport(reg))
			require.NoError(t, err)
			lister := NewLister(client, CooldownConfig{DefaultDays: 7})
			ref := tc.ref
			if ref == "" {
				ref = "v1.0.0"
			}
			err = lister.CheckAdoption(context.Background(), "o", "r", ref, sha, now)
			if tc.want == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.want)
			}
		})
	}
}

func TestAdoptionDateFallbackAndCache(t *testing.T) {
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-time.Hour).Format(time.RFC3339)
	reg := &httpmock.Registry{}
	reg.Register(httpmock.REST("GET", `/commits/`+sha+`$`), httpmock.JSONResponse(map[string]any{
		"sha": sha, "commit": map[string]any{"committer": map[string]any{"date": "2020-01-01T00:00:00Z"}},
	}))
	reg.Register(httpmock.REST("GET", `/tags$`), httpmock.JSONResponse([]any{}))
	reg.Register(httpmock.REST("GET", `/releases$`), httpmock.JSONResponse([]any{}))
	reg.Register(httpmock.REST("GET", `/releases/tags/v1.0.0$`), httpmock.JSONResponse(map[string]any{
		"tag_name": "v1.0.0", "published_at": fresh,
	}))
	client, err := ghapi.New("github.com", ghapi.WithClientTransport(reg))
	require.NoError(t, err)
	lister := NewLister(client, CooldownConfig{DefaultDays: 7})
	for range 2 {
		err := lister.CheckAdoption(context.Background(), "o", "r", "v1.0.0", sha, now)
		require.ErrorContains(t, err, "release v1.0.0 published_at")
	}
	reg.Verify(t)
}

func TestDisabledAdoptionDoesNotReadMetadata(t *testing.T) {
	reg := &httpmock.Registry{}
	client, err := ghapi.New("github.com", ghapi.WithClientTransport(reg))
	require.NoError(t, err)
	lister := NewLister(client, CooldownConfig{DefaultDays: 7, RepoOverrides: map[string]int{"O/R": 0}})
	assert.NoError(t, lister.CheckAdoption(context.Background(), "o", "r", "main", "sha", time.Now()))
	reg.Verify(t)
}
