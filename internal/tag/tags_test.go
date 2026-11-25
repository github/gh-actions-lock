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

func TestBestPatchTagForSHA_RefFamily(t *testing.T) {
	const sha = "ffffffffffffffffffffffffffffffffffffffff"

	tests := []struct {
		name string
		ref  string
		tags any
		want string
	}{
		{
			name: "major ref excludes another major at the same commit",
			ref:  "v18",
			tags: httpmock.TagListResponse("v3.12.0", sha),
		},
		{
			name: "major ref accepts its major",
			ref:  "v4",
			tags: httpmock.TagListResponse("v5.0.0", sha, "v4.2.1", sha),
			want: "v4.2.1",
		},
		{
			name: "minor ref accepts its minor",
			ref:  "v4.2",
			tags: httpmock.TagListResponse("v4.3.0", sha, "v4.2.1", sha),
			want: "v4.2.1",
		},
		{
			name: "prefixed ref stays in its prefix family",
			ref:  "actions/verify/v0",
			tags: httpmock.TagListResponse("v0.9.0", sha, "other/v0.6.0", sha, "actions/verify/v0.5.1", sha, "actions/verify/v0.4.0", sha),
			want: "actions/verify/v0.5.1",
		},
		{
			name: "unprefixed ref ignores prefixed tags",
			ref:  "v0",
			tags: httpmock.TagListResponse("actions/verify/v0.6.0", sha, "v0.1.0", sha),
			want: "v0.1.0",
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

			tl := NewListerForTest(t, reg)
			got, err := tl.BestPatchTagForSHA(context.Background(), "actions", "checkout", sha, tt.ref)
			if err != nil {
				t.Fatalf("BestPatchTagForSHA: %v", err)
			}
			if got != tt.want {
				t.Fatalf("BestPatchTagForSHA() = %q, want %q", got, tt.want)
			}
		})
	}
}

// A multi-action repository's family can sort entirely past the first page of
// tags, so narrowing a prefixed ref reads every page.
func TestBestPatchTagForSHA_PrefixedFamilyOnLaterPage(t *testing.T) {
	const sha = "ffffffffffffffffffffffffffffffffffffffff"
	var first []string
	for i := range 100 {
		first = append(first, fmt.Sprintf("zeta/v1.0.%d", i), "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")
	}
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(httpmock.RESTWithQuery("GET", `^/repos/o/r/tags$`, "&page=1"), httpmock.JSONResponse(httpmock.TagListResponse(first...)))
	reg.Register(httpmock.RESTWithQuery("GET", `^/repos/o/r/tags$`, "&page=2"), httpmock.JSONResponse(httpmock.TagListResponse("verify/v0", sha, "verify/v0.5.1", sha)))

	tl := NewListerForTest(t, reg)
	got, err := tl.BestPatchTagForSHA(context.Background(), "o", "r", sha, "verify/v0")
	if err != nil {
		t.Fatalf("BestPatchTagForSHA: %v", err)
	}
	if got != "verify/v0.5.1" {
		t.Fatalf("BestPatchTagForSHA() = %q, want %q", got, "verify/v0.5.1")
	}
}

func TestListTags_PrefixedFloatingMajor(t *testing.T) {
	const sha = "ffffffffffffffffffffffffffffffffffffffff"
	reg := &httpmock.Registry{}
	reg.Register(httpmock.REST("GET", `^/repos/o/r/tags$`),
		httpmock.JSONResponse(httpmock.TagListResponse("verify/v2", sha, "verify/v1.2.3", sha)))
	reg.Register(httpmock.REST("GET", `^/repos/o/r/releases$`), httpmock.JSONResponse([]any{}))
	tl := NewListerForTest(t, reg)

	tags, err := tl.ListTags(context.Background(), "o", "r")
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	if len(tags) != 2 || tags[0].Name != "verify/v1.2.3" || tags[1].Name != "verify/v2" {
		t.Fatalf("ListTags order = %+v, want verify/v1.2.3 before the floating verify/v2", tags)
	}
	if !tags[1].IsMajor {
		t.Errorf("verify/v2 IsMajor = false, want true")
	}

	sugg, err := tl.SuggestTagsForSHA(context.Background(), "o", "r", sha)
	if err != nil {
		t.Fatalf("SuggestTagsForSHA: %v", err)
	}
	for _, s := range sugg {
		if s.Tag.Name == "verify/v2" && s.Preferred {
			t.Errorf("floating verify/v2 marked preferred: %+v", sugg)
		}
	}
}

func TestListTags_UnprefixedFamilyFirst(t *testing.T) {
	const sha = "ffffffffffffffffffffffffffffffffffffffff"
	reg := &httpmock.Registry{}
	reg.Register(httpmock.REST("GET", `^/repos/o/r/tags$`),
		httpmock.JSONResponse(httpmock.TagListResponse("build/v9.0.0", sha, "verify/v8.0.0", sha, "lint/v7.0.0", sha, "v4.2.1", sha)))
	reg.Register(httpmock.REST("GET", `^/repos/o/r/releases$`), httpmock.JSONResponse([]any{}))
	tl := NewListerForTest(t, reg)

	sugg, err := tl.SuggestTagsForSHA(context.Background(), "o", "r", sha)
	if err != nil {
		t.Fatalf("SuggestTagsForSHA: %v", err)
	}
	if len(sugg) == 0 || sugg[0].Tag.Name != "v4.2.1" {
		t.Fatalf("suggestions = %+v, want v4.2.1 first", sugg)
	}
}
