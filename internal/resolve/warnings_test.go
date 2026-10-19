package resolve

import (
	"context"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/ghapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveWithWarnings(t *testing.T) {
	for _, locked := range []bool{false, true} {
		name := "live cache"
		if locked {
			name = "locked manifest cache"
		}
		t.Run(name, func(t *testing.T) {
			r := &Resolver{MaxRecursionDepth: DefaultMaxRecursionDepth}
			var entries []dep.Dependency
			for repo, manifest := range map[string]string{
				"parent": "runs:\n  using: composite\n  steps:\n    - uses: docker://alpine:3\n    - uses: docker://alpine:3\n    - uses: o/child@v1\n",
				"child":  "runs:\n  using: docker\n  image: docker://ubuntu:24.04\n",
			} {
				d := dep.Dependency{NWO: "o/" + repo, Ref: "v1", SHA: parityTestCommit}
				entry := resolvedEntry{dep: d, actionYML: manifest}
				if locked {
					r.lockedContentCache.Put(ghapi.ForActionRef("o", repo, "", d.SHA), entry)
				} else {
					r.cache.Put(ghapi.ForActionRef("o", repo, "", d.Ref), entry)
				}
				entries = append(entries, d)
			}
			if !locked {
				entries = nil
			}
			for range 2 {
				deps, parents, warnings, err := r.ResolveWithWarnings(context.Background(),
					[]parserlock.ActionRef{{Owner: "o", Repo: "parent", Ref: "v1"}}, entries)
				require.NoError(t, err)
				assert.Len(t, deps, 2)
				assert.Equal(t, []string{"o/parent@v1"}, parents["o/child@v1"])
				assert.Equal(t, []string{
					"docker://alpine:3 (pulled by o/parent@v1) is not covered by the actions lockfile; container image tags are mutable",
					"docker://ubuntu:24.04 (pulled by o/child@v1) is not covered by the actions lockfile; container image tags are mutable",
				}, warnings)
			}
		})
	}
}

func TestResolveWithWarningsSkipsImmutableImagesAndDockerfiles(t *testing.T) {
	for _, image := range []string{"docker://alpine@sha256:0123456789abcdef", "Dockerfile", "./Dockerfile"} {
		t.Run(image, func(t *testing.T) {
			r := seedCache(&Resolver{MaxRecursionDepth: DefaultMaxRecursionDepth}, map[ghapi.ActionRef]resolvedEntry{
				ghapi.ForActionRef("o", "r", "", "v1"): {
					dep:       dep.Dependency{NWO: "o/r", Ref: "v1", SHA: parityTestCommit},
					actionYML: "runs:\n  using: docker\n  image: " + image + "\n",
				},
			})
			_, _, warnings, err := r.ResolveWithWarnings(context.Background(),
				[]parserlock.ActionRef{{Owner: "o", Repo: "r", Ref: "v1"}}, nil)
			require.NoError(t, err)
			assert.Empty(t, warnings)
		})
	}
}

const parityTestCommit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
