package tag

import (
	"context"
	"testing"
	"time"

	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
)

func TestCooledTag(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	daysAgo := func(d int) string { return now.Add(-time.Duration(d) * 24 * time.Hour).Format(time.RFC3339) }
	const (
		newSHA = "1111111111111111111111111111111111111111"
		midSHA = "2222222222222222222222222222222222222222"
		oldSHA = "3333333333333333333333333333333333333333"
		preSHA = "4444444444444444444444444444444444444444"
	)
	tags := httpmock.TagListResponse(
		"v4", newSHA, "v4.3", newSHA, "v4.3.0", newSHA,
		"v4.3.0-rc.1", preSHA,
		"v4.2.0", midSHA,
		"v4.1.0", oldSHA,
		"v3.9.0", oldSHA,
	)

	tests := []struct {
		name     string
		ref      string
		days     int
		releases []map[string]any
		want     string
		wantSHA  string
		wantOK   bool
	}{
		{
			name: "fresh release holds back to newest release that clears the cooldown",
			ref:  "v4", days: 7,
			releases: []map[string]any{
				{"tag_name": "v4.3.0", "published_at": daysAgo(1)},
				{"tag_name": "v4.3.0-rc.1", "published_at": daysAgo(10), "prerelease": true},
				{"tag_name": "v4.2.0", "published_at": daysAgo(5)},
				{"tag_name": "v4.1.0", "published_at": daysAgo(20)},
				{"tag_name": "v3.9.0", "published_at": daysAgo(8)},
			},
			want: "v4.1.0", wantSHA: oldSHA, wantOK: true,
		},
		{
			name: "current release that clears the cooldown is kept",
			ref:  "v4", days: 7,
			releases: []map[string]any{
				{"tag_name": "v4.3.0", "published_at": daysAgo(8)},
				{"tag_name": "v4.1.0", "published_at": daysAgo(20)},
			},
		},
		{
			name: "no older release clears the cooldown",
			ref:  "v4", days: 7,
			releases: []map[string]any{
				{"tag_name": "v4.3.0", "published_at": daysAgo(1)},
				{"tag_name": "v4.2.0", "published_at": daysAgo(2)},
			},
		},
		{
			name: "major ref never falls back outside its major",
			ref:  "v4", days: 7,
			releases: []map[string]any{
				{"tag_name": "v4.3.0", "published_at": daysAgo(1)},
				{"tag_name": "v4.2.0", "published_at": daysAgo(2)},
				{"tag_name": "v4.1.0", "published_at": daysAgo(3)},
				{"tag_name": "v3.9.0", "published_at": daysAgo(20)},
			},
		},
		{
			name: "minor ref never falls back outside its minor",
			ref:  "v4.3", days: 7,
			releases: []map[string]any{
				{"tag_name": "v4.3.0", "published_at": daysAgo(1)},
				{"tag_name": "v4.2.0", "published_at": daysAgo(20)},
			},
		},
		{
			name: "undated current release is kept",
			ref:  "v4", days: 7,
			releases: []map[string]any{
				{"tag_name": "v4.1.0", "published_at": daysAgo(20)},
			},
		},
		{
			name: "exact version is not held back",
			ref:  "v4.3.0", days: 7,
			releases: []map[string]any{
				{"tag_name": "v4.3.0", "published_at": daysAgo(1)},
				{"tag_name": "v4.1.0", "published_at": daysAgo(20)},
			},
		},
		{
			name: "zero cooldown is not held back",
			ref:  "v4", days: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := &httpmock.Registry{}
			reg.RegisterReusable(httpmock.REST("GET", `repos/o/r/tags`), httpmock.JSONResponse(tags))
			reg.RegisterReusable(httpmock.REST("GET", `repos/o/r/releases`), httpmock.JSONResponse(tt.releases))

			got, ok := NewListerForTest(t, reg).CooledTag(context.Background(), "o", "r", tt.ref, tt.days, now)

			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.want, got.Name)
			assert.Equal(t, tt.wantSHA, got.SHA)
		})
	}
}
