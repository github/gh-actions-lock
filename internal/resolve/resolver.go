// Package resolve resolves action refs to commit SHAs, recursively discovers
// transitive dependencies, and verifies commit reachability.
package resolve

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/ghapi"
	"github.com/github/gh-actions-lock/internal/pinpool"
	"github.com/github/gh-actions-lock/internal/profile"
	"github.com/github/gh-actions-lock/internal/syncmap"
	"golang.org/x/sync/singleflight"
)

// DefaultMaxRecursionDepth matches the runner's composite action recursion limit.
const DefaultMaxRecursionDepth = 10

// Option configures a Resolver at construction time. Pass to New.
type Option func(*Resolver)

// WithTransport overrides the HTTP transport. Use in tests with httpmock.
func WithTransport(t http.RoundTripper) Option {
	return func(r *Resolver) { r.transport = t }
}

// WithProfile attaches profiling instrumentation.
func WithProfile(p *profile.Session) Option {
	return func(r *Resolver) { r.profile = p }
}

// WithNowFn overrides time.Now for rate-limit retry timing in tests.
func WithNowFn(fn func() time.Time) Option {
	return func(r *Resolver) {
		if fn != nil {
			r.nowFn = fn
		}
	}
}

// WithSleepFn overrides the context-aware sleep used for rate-limit waits.
func WithSleepFn(fn func(context.Context, time.Duration)) Option {
	return func(r *Resolver) {
		if fn != nil {
			r.sleepFn = fn
		}
	}
}

// Resolver resolves action refs to commit SHAs.
type Resolver struct {
	gh       *ghapi.Client
	hostname string

	// MaxRecursionDepth caps transitive composite action resolution depth.
	MaxRecursionDepth int

	// Construction-time options applied before ghapi.Client is built.
	transport http.RoundTripper // nil → use default authenticated transport
	profile   *profile.Session  // nil → no profiling

	// Domain-level caches: action resolution, branch hints.
	cache              syncmap.Map[ghapi.ActionRef, resolvedEntry]
	latestRefCache     syncmap.Map[ghapi.Repo, string]
	branchHintBySHA    syncmap.Map[ghapi.NWOSha, string]
	releaseBranchCache syncmap.Map[ghapi.Repo, []ghapi.BranchHead]
	releaseBranchSF    singleflight.Group
	tagObjectCache     syncmap.Map[ghapi.NWOSha, tagPeel]

	// writtenNames holds refs that resolve under the name they are written
	// with even when the repository was renamed. Set before resolving.
	writtenNames map[ghapi.ActionRef]bool

	// Test overrides (injected via With* options).
	nowFn   func() time.Time
	sleepFn func(context.Context, time.Duration)

	// OnResolveProgress is called when a resolution batch makes progress.
	OnResolveProgress func(done, total int)
	progressMu        sync.Mutex

	// Pool is the shared worker pool for parallel resolution and reachability.
	Pool *pinpool.Pool
}

// New creates a Resolver for the given hostname and pool. Use With*
// options to inject a test transport, profiling, or test overrides.
func New(hostname string, pool *pinpool.Pool, opts ...Option) (*Resolver, error) {
	r := &Resolver{
		hostname:          hostname,
		Pool:              pool,
		MaxRecursionDepth: DefaultMaxRecursionDepth,
		nowFn:             time.Now,
		sleepFn:           ghapi.DefaultSleep,
	}
	for _, o := range opts {
		o(r)
	}
	var ghOpts []ghapi.ClientOption
	if r.transport != nil {
		ghOpts = append(ghOpts, ghapi.WithClientTransport(r.transport))
	}
	if r.profile != nil {
		ghOpts = append(ghOpts, ghapi.WithClientProfile(r.profile))
	}
	c, err := ghapi.New(hostname, ghOpts...)
	if err != nil {
		return nil, err
	}
	r.gh = c
	r.hostname = c.Hostname
	return r, nil
}

// --- Seeding (post-construction, deps come from lockfile loaded after resolver) ---

// SeedHosts binds recorded repositories before caches or network work begin.
func (r *Resolver) SeedHosts(deps []dep.Dependency) error {
	for _, d := range deps {
		owner, repo := d.OwnerRepo()
		if err := r.gh.PinHost(owner, repo, d.Hostname); err != nil {
			return err
		}
	}
	return nil
}

// SeedBranchHints records a branch-of-record for each dep so subsequent
// containing-branch scans try that branch first. Hints from a previous
// lockfile are advisory: a miss falls through to a full branch scan.
func (r *Resolver) SeedBranchHints(deps []dep.Dependency) {
	for _, d := range deps {
		if d.Branch == "" || d.SHA == "" {
			continue
		}
		owner, repo := d.OwnerRepo()
		if owner == "" || repo == "" {
			continue
		}
		r.branchHintBySHA.Put(ghapi.ForNWOSha(owner, repo, d.SHA), d.Branch)
	}
}

// SeedFromLockfile pre-warms the resolution cache so repeat runs skip
// redundant API calls. Do NOT call with --relock: seeding would hide
// ref movement.
func (r *Resolver) SeedFromLockfile(deps []dep.Dependency) {
	for _, d := range deps {
		if d.SHA == "" || d.Ref == "" {
			continue
		}
		owner, repo := d.OwnerRepo()
		if owner == "" || repo == "" {
			continue
		}
		r.cache.Put(
			ghapi.ForActionRef(owner, repo, d.Path, d.Ref),
			resolvedEntry{dep: d, seeded: true},
		)
	}
}

// Seeded reports whether ref resolved from a lockfile seed rather than a
// live read, so nothing has checked its manifest this run.
func (r *Resolver) Seeded(ref parserlock.ActionRef) bool {
	e, ok := r.cache.Get(cacheKey(ref))
	return ok && e.seeded
}

// KeepWrittenNames makes refs resolve under the name they are written with,
// as a remote composite's do: nothing rewrites the files that hold them, and
// the runner looks each one up as written. Call it before resolving.
func (r *Resolver) KeepWrittenNames(refs []parserlock.ActionRef) {
	if r.writtenNames == nil {
		r.writtenNames = make(map[ghapi.ActionRef]bool, len(refs))
	}
	for _, ref := range refs {
		r.writtenNames[ghapi.ForActionRef(ref.Owner, ref.Repo, "", ref.Ref)] = true
	}
}

// keepsWrittenName reports whether ref, written as orig and resolving to
// the canonical name, stays as written. That holds when an action file
// writes either spelling of the repository at that ref, under any path:
// lock entries are keyed by repository and ref, so redirecting an alias
// onto the action file's name would merge the two, and narrowing the
// merged entry would drop the key the action file needs.
func (r *Resolver) keepsWrittenName(orig parserlock.ActionRef, canonical string) bool {
	if r.writtenNames[ghapi.ForActionRef(orig.Owner, orig.Repo, "", orig.Ref)] {
		return true
	}
	owner, repo, ok := strings.Cut(canonical, "/")
	return ok && r.writtenNames[ghapi.ForActionRef(owner, repo, "", orig.Ref)]
}

// Redirect re-labels the entry seeded for owner/repo under its canonical
// name, keeping the locked commit: a rename of the same repository is no
// reason to advance. orig is the ref as written, which the rewrite must
// match exactly. Unseeded refs are left to live resolution.
func (r *Resolver) Redirect(owner, repo string, orig parserlock.ActionRef, canonical string) {
	if r.keepsWrittenName(orig, canonical) {
		return
	}
	for _, key := range []ghapi.ActionRef{ghapi.ForActionRef(owner, repo, "", orig.Ref), ghapi.ForActionRef(owner, repo, orig.Path, orig.Ref)} {
		e, ok := r.cache.Get(key)
		if !ok {
			continue
		}
		e.dep.NWO = canonical
		if !slices.Contains(e.dep.OriginalRefs, orig) {
			e.dep.OriginalRefs = append(e.dep.OriginalRefs, orig)
		}
		r.cache.Put(key, e)
	}
}

// --- Accessors ---

// Hostname returns the GitHub host the resolver is targeting.
func (r *Resolver) Hostname() string { return r.hostname }

// GHClient returns the unified API client.
func (r *Resolver) GHClient() *ghapi.Client { return r.gh }

// RepoIDs returns the numeric owner ID and repo ID for a NWO.
func (r *Resolver) RepoIDs(ctx context.Context, hostname, owner, repo string) (int64, int64, error) {
	client, err := r.gh.ForRepo(ctx, owner, repo)
	if err != nil {
		return 0, 0, err
	}
	if hostname != client.Hostname {
		return 0, 0, fmt.Errorf("%s/%s resolved on %s but metadata requested from %s", owner, repo, client.Hostname, hostname)
	}
	return client.RepoIDs(ctx, owner, repo)
}

// branchHint returns the branch previously recorded as containing sha in
// owner/repo, or "" if no hint exists.
func (r *Resolver) branchHint(owner, repo, sha string) string {
	hint, _ := r.branchHintBySHA.Get(ghapi.ForNWOSha(owner, repo, sha))
	return hint
}

// --- Cache helpers (package-internal) ---

// --- Progress ---

// FireResolveProgress fires OnResolveProgress. Safe from multiple goroutines.
func (r *Resolver) FireResolveProgress(done, total int) {
	if r.OnResolveProgress == nil {
		return
	}
	r.progressMu.Lock()
	defer r.progressMu.Unlock()
	r.OnResolveProgress(done, total)
}
