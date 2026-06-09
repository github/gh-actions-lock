package main

import (
	"encoding/json"
	"testing"

	"github.com/github/gh-actions-pin/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// outdatedJSON is the decoded shape of `outdated --json` output.
type outdatedJSON struct {
	CLIVersion       string `json:"cli_version"`
	LockfileVersion  string `json:"lockfile_version"`
	AvailableUpdates []struct {
		NWO          string `json:"nwo"`
		CurrentRef   string `json:"current_ref"`
		CurrentSHA   string `json:"current_sha"`
		AvailableRef string `json:"available_ref"`
		AvailableSHA string `json:"available_sha"`
		Precision    string `json:"precision"`
	} `json:"available_updates"`
}

func registerTags(reg *httpmock.Registry, owner, repo string, nameSHA ...string) {
	entries := make([]map[string]any, 0, len(nameSHA)/2)
	for i := 0; i+1 < len(nameSHA); i += 2 {
		entries = append(entries, map[string]any{
			"name":   nameSHA[i],
			"commit": map[string]any{"sha": nameSHA[i+1]},
		})
	}
	reg.Register(
		httpmock.REST("GET", `repos/`+owner+`/`+repo+`/tags`),
		httpmock.JSONResponse(entries),
	)
}

func TestOutdatedCommand_OffersNewerMajor(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	const (
		oldSHA = "de0fac2e4500dabe0009e67214ff5f5447ce83dd"
		v6SHA  = "11111111111111111111111111111111111111aa"
	)
	registerTags(reg, "actions", "checkout",
		"v4", oldSHA,
		"v6", v6SHA,
		"v6.1.2", "22222222222222222222222222222222222222bb",
	)

	writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
`,
		"actions/checkout@v4:sha1-"+oldSHA,
	)

	stdout, _, err := runCommandWithHTTP(t, reg,
		"outdated", "--json=available_updates",
	)
	require.ErrorIs(t, err, errSilent, "updates available → exit 1")

	var got outdatedJSON
	require.NoError(t, json.Unmarshal([]byte(stdout), &got), "stdout must be parseable JSON despite exit 1")
	assert.Equal(t, "v0.0.1", got.LockfileVersion)
	require.Len(t, got.AvailableUpdates, 1)
	u := got.AvailableUpdates[0]
	assert.Equal(t, "actions/checkout", u.NWO)
	assert.Equal(t, "v4", u.CurrentRef)
	assert.Equal(t, oldSHA, u.CurrentSHA)
	assert.Equal(t, "v6", u.AvailableRef, "major float offers the highest major-form tag, not v6.1.2")
	assert.Equal(t, v6SHA, u.AvailableSHA)
	assert.Equal(t, "major", u.Precision)
}

func TestOutdatedCommand_FullPinOffersFullTag(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	const (
		oldSHA  = "de0fac2e4500dabe0009e67214ff5f5447ce83dd"
		fullSHA = "33333333333333333333333333333333333333cc"
	)
	registerTags(reg, "actions", "setup-go",
		"v5", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"v5.1.0", oldSHA,
		"v6", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"v6.1.2", fullSHA,
	)

	writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/setup-go@v5.1.0
`,
		"actions/setup-go@v5.1.0:sha1-"+oldSHA,
	)

	stdout, _, err := runCommandWithHTTP(t, reg, "outdated", "--json")
	require.ErrorIs(t, err, errSilent)

	var got outdatedJSON
	require.NoError(t, json.Unmarshal([]byte(stdout), &got))
	require.Len(t, got.AvailableUpdates, 1)
	u := got.AvailableUpdates[0]
	assert.Equal(t, "v5.1.0", u.CurrentRef)
	assert.Equal(t, "v6.1.2", u.AvailableRef, "full pin offers the highest full tag, not the v6 major float")
	assert.Equal(t, fullSHA, u.AvailableSHA)
	assert.Equal(t, "full", u.Precision)
}

func TestOutdatedCommand_UpToDateExitsZero(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	const v6SHA = "11111111111111111111111111111111111111aa"
	registerTags(reg, "actions", "checkout",
		"v4", "de0fac2e4500dabe0009e67214ff5f5447ce83dd",
		"v6", v6SHA,
	)

	writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
`,
		"actions/checkout@v6:sha1-"+v6SHA,
	)

	stdout, _, err := runCommandWithHTTP(t, reg, "outdated", "--json=available_updates")
	require.NoError(t, err, "no updates available → exit 0")

	var got outdatedJSON
	require.NoError(t, json.Unmarshal([]byte(stdout), &got))
	assert.Empty(t, got.AvailableUpdates, "available_updates must be an empty array, not absent")
}

func TestOutdatedCommand_RejectsUnknownJSONField(t *testing.T) {
	writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
`,
		"actions/checkout@v4:sha1-de0fac2e4500dabe0009e67214ff5f5447ce83dd",
	)

	_, _, err := runCommandWithHTTP(t, &httpmock.Registry{}, "outdated", "--json=bogus")
	require.Error(t, err)
	assert.NotErrorIs(t, err, errSilent, "a bad --json selector is a usage error (exit 2), not a findings exit")
}
