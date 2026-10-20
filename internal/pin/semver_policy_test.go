package pin

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/tag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSemverNarrowingCurrentPolicy(t *testing.T) {
	const sha = "4444444444444444444444444444444444444444"
	for _, tc := range []struct {
		name, ref, tag, want string
		recorded             bool
		transitive           bool
		noNarrow             bool
		sameOwner            bool
		listingFails         bool
		tagSHA               string
	}{
		{name: "new major ref narrows", ref: "v4", tag: "v4.3.1", want: "v4.3.1"},
		{name: "new minor ref narrows", ref: "v4.3", tag: "v4.3.1", want: "v4.3.1"},
		{name: "exact tag stays exact", ref: "v4.3.1", tag: "v4.3.1", want: "v4.3.1"},
		{name: "prerelease uses a stable alias at the same commit", ref: "v4.3.1-rc.1", tag: "v4.3.1", want: "v4.3.1"},
		{name: "prerelease without a stable alias stays intact", ref: "v4.3.1-rc.1", tag: "v4.3.1-rc.1", want: "v4.3.1-rc.1"},
		{name: "no full tag preserves partial ref", ref: "v4", tag: "v4", want: "v4"},
		{name: "other commit cannot supply an exact tag", ref: "v4", tag: "v4.3.1", tagSHA: "5555555555555555555555555555555555555555", want: "v4"},
		{name: "other minor cannot supply an exact tag", ref: "v4.3", tag: "v4.4.1", want: "v4.3"},
		{name: "tag lookup failure preserves partial ref", ref: "v4", want: "v4", listingFails: true},
		{name: "recorded major ref stays sticky", ref: "v4", tag: "v4.3.1", want: "v4", recorded: true},
		{name: "recorded minor ref stays sticky", ref: "v4.3", tag: "v4.3.1", want: "v4.3", recorded: true},
		{name: "owner context does not disable version narrowing", ref: "v4", tag: "v4.3.1", want: "v4.3.1", sameOwner: true},
		{name: "external composite declaration stays intact", ref: "v4", tag: "v4.3.1", want: "v4", transitive: true},
		{name: "explicit opt out preserves partial ref", ref: "v4", tag: "v4.3.1", want: "v4", noNarrow: true},
	} {
		for _, keepSHA := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/keep-sha=%t", tc.name, keepSHA), func(t *testing.T) {
				reg := &httpmock.Registry{}
				if tc.listingFails {
					reg.Register(httpmock.REST("GET", `repos/actions/checkout/tags`), httpmock.StatusResponse(http.StatusServiceUnavailable))
				} else {
					tagSHA := tc.tagSHA
					if tagSHA == "" {
						tagSHA = sha
					}
					reg.Register(httpmock.REST("GET", `repos/actions/checkout/tags`),
						httpmock.JSONResponse(httpmock.TagListResponse(tc.tag, tagSHA)))
				}
				opts := PlanOptions{
					Tagger: tag.NewListerForTest(t, reg), NoNarrow: tc.noNarrow, KeepSHA: keepSHA,
				}
				if tc.sameOwner {
					opts.RepoOwner = "actions"
					opts.RepoName = "checkout"
				}
				ref := parserlock.ActionRef{Owner: "actions", Repo: "checkout", Ref: tc.ref}
				deps := []dep.Dependency{{NWO: ref.NWO(), Ref: tc.ref, SHA: sha}}
				rewrites := make(map[string]string)
				if tc.recorded {
					store, err := lockfile.LoadState(t.TempDir(), fakeMeta{})
					require.NoError(t, err)
					require.NoError(t, store.Set(context.Background(), ".github/workflows/ci.yml", deps, nil,
						map[string]bool{deps[0].Key(): true}))
					opts.Store = store
					opts.prevImpreciseNWO = impreciseDirectNWOs(store)
					entries := []Entry{{NWO: ref.NWO(), Ref: tc.ref, SHA: sha, Resolution: Verified, Direct: true}}
					rewrites = narrowVerifiedEntries(context.Background(), entries, opts, []parserlock.ActionRef{ref})
					assert.Equal(t, tc.want, entries[0].Ref)
					assert.Equal(t, sha, entries[0].SHA)
					assert.Empty(t, entries[0].AutoFixedRef)
				} else {
					var refs []parserlock.ActionRef
					if !tc.transitive {
						refs = []parserlock.ActionRef{ref}
					}
					narrowDirectDeps(context.Background(), opts, deps, lockfile.NewDirectTracker(refs, deps),
						rewrites, make(map[int]bool))
					assert.Equal(t, tc.want, deps[0].Ref)
					assert.Equal(t, sha, deps[0].SHA)
				}
				if tc.want == tc.ref {
					assert.Empty(t, rewrites)
				} else {
					assert.Equal(t, map[string]string{ref.NWO() + "@" + tc.ref: ref.NWO() + "@" + tc.want}, rewrites)
				}
			})
		}
	}
}
