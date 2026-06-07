package resolve

import (
	"context"
	"net/http"
	"testing"

	"github.com/github/gh-actions-pin/internal/ghapi/httpmock"
)

func TestCheckAncestry_SameSHA(t *testing.T) {
	r := &Resolver{}
	status, detail := r.CheckAncestry(context.Background(), "o", "r", "abc123", "abc123")
	if status != AncestryConfirmed {
		t.Fatalf("expected AncestryConfirmed, got %v", status)
	}
	if detail != "pinned SHA equals live SHA" {
		t.Fatalf("unexpected detail: %s", detail)
	}
}

func TestCheckAncestry_SameSHACaseInsensitive(t *testing.T) {
	r := &Resolver{}
	status, _ := r.CheckAncestry(context.Background(), "o", "r", "ABC123", "abc123")
	if status != AncestryConfirmed {
		t.Fatal("expected case-insensitive match to confirm ancestry")
	}
}

func TestCheckAncestry_Confirmed(t *testing.T) {
	pinned := "aaaa"
	live := "bbbb"

	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "repos/owner/repo/compare/aaaa...bbbb"),
		httpmock.JSONResponse(map[string]any{
			"status":            "behind",
			"merge_base_commit": map[string]string{"sha": pinned},
		}),
	)

	r := newTestResolver(t, reg)
	status, detail := r.CheckAncestry(context.Background(), "owner", "repo", pinned, live)
	if status != AncestryConfirmed {
		t.Fatalf("expected AncestryConfirmed, got %v; detail: %s", status, detail)
	}
}

func TestCheckAncestry_NotAncestor(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "repos/owner/repo/compare/aaaa...bbbb"),
		httpmock.JSONResponse(map[string]any{
			"status":            "diverged",
			"merge_base_commit": map[string]string{"sha": "cccc"},
		}),
	)

	r := newTestResolver(t, reg)
	status, _ := r.CheckAncestry(context.Background(), "owner", "repo", "aaaa", "bbbb")
	if status != AncestryNotAncestor {
		t.Fatalf("expected AncestryNotAncestor, got %v", status)
	}
}

func TestCheckAncestry_404ReturnsNotAncestor(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "repos/owner/repo/compare/aaaa...bbbb"),
		httpmock.StatusResponse(http.StatusNotFound),
	)

	r := newTestResolver(t, reg)
	status, _ := r.CheckAncestry(context.Background(), "owner", "repo", "aaaa", "bbbb")
	if status != AncestryNotAncestor {
		t.Fatalf("expected AncestryNotAncestor for 404, got %v", status)
	}
}

func TestCheckAncestry_409ReturnsNotAncestor(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "repos/owner/repo/compare/aaaa...bbbb"),
		httpmock.StatusResponse(http.StatusConflict),
	)

	r := newTestResolver(t, reg)
	status, _ := r.CheckAncestry(context.Background(), "owner", "repo", "aaaa", "bbbb")
	if status != AncestryNotAncestor {
		t.Fatalf("expected AncestryNotAncestor for 409, got %v", status)
	}
}

func TestCheckAncestry_500ReturnsUnknown(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "repos/owner/repo/compare/aaaa...bbbb"),
		httpmock.StatusResponse(http.StatusInternalServerError),
	)

	r := newTestResolver(t, reg)
	status, _ := r.CheckAncestry(context.Background(), "owner", "repo", "aaaa", "bbbb")
	if status != AncestryUnknown {
		t.Fatalf("expected AncestryUnknown for 500, got %v", status)
	}
}

func TestShortHex(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"short stays unchanged", "abc", "abc"},
		{"exactly 12 stays unchanged", "123456789012", "123456789012"},
		{"long gets truncated", "1234567890123", "123456789012"},
		{"full sha truncated", "d746ffe35508b1917358783b479e04febd2b8f71", "d746ffe35508"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shortHex(tt.in)
			if got != tt.want {
				t.Errorf("shortHex(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// newTestResolver creates a Resolver with a fake HTTP transport for testing.
func newTestResolver(t *testing.T, reg *httpmock.Registry) *Resolver {
	t.Helper()
	r, err := New("github.com", nil, WithTransport(reg))
	if err != nil {
		t.Fatal(err)
	}
	return r
}
