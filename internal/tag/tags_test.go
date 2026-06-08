package tag

import (
	"context"
	"testing"

	"github.com/github/gh-actions-pin/internal/ghapi/httpmock"
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
