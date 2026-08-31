package tag

import (
	"context"
	"fmt"
	"testing"

	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
)

// TestMatchesSHA verifies a pin matches the tag's peeled commit SHA.
// Resolution peels annotated tags to the commit before any SHA is written,
// so the commit SHA is the only identifier callers compare against.
func TestMatchesSHA(t *testing.T) {
	tag := Info{Name: "v9.0.0", SHA: "3a2844b"}

	if !tag.MatchesSHA("3a2844b") {
		t.Error("expected match on peeled commit SHA")
	}
	if !tag.MatchesSHA("3A2844B") {
		t.Error("expected case-insensitive match on commit SHA")
	}
	if tag.MatchesSHA("deadbee") {
		t.Error("unexpected match on unrelated SHA")
	}
	if tag.MatchesSHA("") {
		t.Error("empty SHA should never match")
	}
}

// TestListTags_SemverOrdering verifies tags are ordered by semantic version,
// not lexically. A string compare would place "v9.0.0" ahead of "v10.0.0";
// semver ordering must put v10 first.
func TestListTags_SemverOrdering(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/tags`),
		httpmock.JSONResponse([]map[string]any{
			{"name": "v9.0.0", "commit": map[string]any{"sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}},
			{"name": "v10.0.0", "commit": map[string]any{"sha": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}},
			{"name": "v8.1.0", "commit": map[string]any{"sha": "cccccccccccccccccccccccccccccccccccccccc"}},
		}),
	)
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/releases`),
		httpmock.JSONResponse([]map[string]any{}),
	)

	tl := NewListerForTest(t, reg)
	tags, err := tl.ListTags(context.Background(), "actions", "checkout")
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}

	var got []string
	for _, tg := range tags {
		got = append(got, tg.Name)
	}
	want := []string{"v10.0.0", "v9.0.0", "v8.1.0"}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("semver ordering wrong: expected %v, got %v", want, got)
		}
	}
}

func TestBestAncestorTag_RefFamily(t *testing.T) {
	const headSHA = "ffffffffffffffffffffffffffffffffffffffff"

	tests := []struct {
		name        string
		ref         string
		tags        any
		ancestorSHA string
		want        string
	}{
		{
			name: "major ref excludes another major",
			ref:  "v18",
			tags: httpmock.TagListResponse(
				"v3.12.0", "3333333333333333333333333333333333333333",
			),
		},
		{
			name:        "major ref accepts its major",
			ref:         "v4",
			tags:        httpmock.TagListResponse("v4.2.1", "4444444444444444444444444444444444444444"),
			ancestorSHA: "4444444444444444444444444444444444444444",
			want:        "v4.2.1",
		},
		{
			name: "minor ref accepts its minor",
			ref:  "v4.2",
			tags: httpmock.TagListResponse(
				"v4.3.0", "4343434343434343434343434343434343434343",
				"v4.2.1", "4242424242424242424242424242424242424242",
			),
			ancestorSHA: "4242424242424242424242424242424242424242",
			want:        "v4.2.1",
		},
		{
			name:        "bare SHA accepts any family",
			tags:        httpmock.TagListResponse("v3.12.0", "3333333333333333333333333333333333333333"),
			ancestorSHA: "3333333333333333333333333333333333333333",
			want:        "v3.12.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := &httpmock.Registry{}
			reg.Register(
				httpmock.REST("GET", `repos/actions/checkout/tags`),
				httpmock.JSONResponse(tt.tags),
			)
			reg.Register(
				httpmock.REST("GET", `repos/actions/checkout/releases`),
				httpmock.JSONResponse([]map[string]any{}),
			)
			if tt.ancestorSHA != "" {
				reg.Register(
					httpmock.REST("GET", fmt.Sprintf(`repos/actions/checkout/compare/%s\.\.\.%s`, tt.ancestorSHA, headSHA)),
					httpmock.JSONResponse(httpmock.CompareAncestorResponse(tt.ancestorSHA)),
				)
			}

			tl := NewListerForTest(t, reg)
			got, err := tl.BestAncestorTag(context.Background(), "actions", "checkout", headSHA, tt.ref)
			if err != nil {
				t.Fatalf("BestAncestorTag: %v", err)
			}
			if got != tt.want {
				t.Fatalf("BestAncestorTag() = %q, want %q", got, tt.want)
			}
		})
	}
}
