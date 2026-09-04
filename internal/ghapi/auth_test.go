package ghapi

import (
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestSecureTokensStayBoundToTheirHosts(t *testing.T) {
	t.Setenv("GH_TOKEN", "ambient-must-not-win")

	tokens := map[string]string{
		"tenant.ghe.com": "tenant-sentinel",
		"github.com":     "dotcom-sentinel",
	}
	var calls [][]string
	output := func(name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		return []byte(tokens[args[len(args)-1]] + "\n"), nil
	}

	var gotAuth []string
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		gotAuth = append(gotAuth, req.Header.Get("Authorization"))
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(`{}`)),
		}, nil
	})

	for _, host := range []string{"tenant.ghe.com", "github.com"} {
		token, err := secureToken(host, output)
		if err != nil {
			t.Fatalf("secureToken(%q): %v", host, err)
		}
		client, err := New(host, WithClientAuthToken(token), WithClientTransport(transport))
		if err != nil {
			t.Fatalf("New(%q): %v", host, err)
		}
		if err := client.rest.Do(http.MethodGet, "user", nil, nil); err != nil {
			t.Fatalf("request to %q: %v", host, err)
		}
	}

	wantCalls := [][]string{
		{"gh", "auth", "token", "--secure-storage", "--hostname", "tenant.ghe.com"},
		{"gh", "auth", "token", "--secure-storage", "--hostname", "github.com"},
	}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("calls = %#v, want %#v", calls, wantCalls)
	}
	wantAuth := []string{"token tenant-sentinel", "token dotcom-sentinel"}
	if !reflect.DeepEqual(gotAuth, wantAuth) {
		t.Fatal("clients did not receive their distinct host tokens")
	}
}

func TestSecureTokenMissingDotcomAuthIsActionableAndSecretFree(t *testing.T) {
	const sentinel = "must-not-appear"
	_, err := secureToken("github.com", func(string, ...string) ([]byte, error) {
		return []byte(sentinel), errors.New(sentinel)
	})
	if err == nil {
		t.Fatal("secureToken() error = nil")
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Fatalf("error exposed credential: %v", err)
	}
	if want := "gh auth login --hostname github.com"; !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q, want actionable command %q", err, want)
	}
}
