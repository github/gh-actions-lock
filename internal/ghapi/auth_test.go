package ghapi

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSecureTokensStayBoundToTheirHosts(t *testing.T) {
	t.Setenv("GH_TOKEN", "ambient-must-not-win")
	t.Setenv("GITHUB_TOKEN", "ambient-must-not-win")
	t.Setenv("GH_ENTERPRISE_TOKEN", "ambient-must-not-win")
	t.Setenv("GITHUB_ENTERPRISE_TOKEN", "ambient-must-not-win")
	t.Setenv("GH_CONFIG_DIR", "/config-must-survive")
	t.Setenv("HOME", "/home-must-survive")
	fakeGH(t, `
test -z "${GH_TOKEN+x}" || exit 90
test -z "${GITHUB_TOKEN+x}" || exit 91
test -z "${GH_ENTERPRISE_TOKEN+x}" || exit 92
test -z "${GITHUB_ENTERPRISE_TOKEN+x}" || exit 93
test "$GH_CONFIG_DIR" = "/config-must-survive" || exit 94
test "$HOME" = "/home-must-survive" || exit 95
test "$#" -eq 4 || exit 96
test "$1" = "auth" || exit 97
test "$2" = "token" || exit 98
test "$3" = "--hostname" || exit 99
case "$4" in
	tenant.ghe.com) printf 'tenant-sentinel\n' ;;
	github.com) printf 'dotcom-sentinel\n' ;;
	*) exit 100 ;;
esac
`)

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
		token, err := SecureToken(host)
		if err != nil {
			t.Fatalf("SecureToken(%q): %v", host, err)
		}
		client, err := New(host, WithClientAuthToken(token), WithClientTransport(transport))
		if err != nil {
			t.Fatalf("New(%q): %v", host, err)
		}
		if err := client.rest.Do(http.MethodGet, "user", nil, nil); err != nil {
			t.Fatalf("request to %q: %v", host, err)
		}
	}

	wantAuth := []string{"token tenant-sentinel", "token dotcom-sentinel"}
	if !reflect.DeepEqual(gotAuth, wantAuth) {
		t.Fatal("clients did not receive their distinct host tokens")
	}
}

func TestSecureTokenMissingDotcomAuthIsActionableAndSecretFree(t *testing.T) {
	fakeGH(t, `printf 'no oauth token found for github.com\n' >&2; exit 1`)
	_, err := SecureToken("github.com")
	if err == nil {
		t.Fatal("SecureToken() error = nil")
	}
	if want := "gh auth login --hostname github.com"; !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q, want actionable command %q", err, want)
	}
}

func TestSecureTokenReportsCommandFailureWithoutStderr(t *testing.T) {
	const sentinel = "stderr-must-not-appear"
	fakeGH(t, `printf '`+sentinel+`\n' >&2; exit 7`)
	_, err := SecureToken("github.com")
	if err == nil {
		t.Fatal("SecureToken() error = nil")
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Fatalf("error exposed subprocess stderr: %v", err)
	}
	if strings.Contains(err.Error(), "gh auth login") {
		t.Fatalf("command failure misreported as missing auth: %v", err)
	}
	if !strings.Contains(err.Error(), "failed (exit status 7)") {
		t.Fatalf("error = %q, want exit status", err)
	}
}

func TestSecureTokenReportsMissingExecutable(t *testing.T) {
	t.Setenv("GH_PATH", filepath.Join(t.TempDir(), "missing-gh"))
	_, err := SecureToken("github.com")
	if err == nil {
		t.Fatal("SecureToken() error = nil")
	}
	if strings.Contains(err.Error(), "gh auth login") {
		t.Fatalf("executable failure misreported as missing auth: %v", err)
	}
	if !strings.Contains(err.Error(), "running gh credential lookup") {
		t.Fatalf("error = %q, want executable failure", err)
	}
}

func fakeGH(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "gh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_PATH", "")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}
