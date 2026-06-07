// Package resolve resolves action refs to commit SHAs, recursively discovers
// transitive dependencies, and verifies commit reachability.
package resolve

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/github/gh-actions-pin/internal/dep"
	"github.com/github/gh-actions-pin/internal/ghapi"
	"github.com/github/gh-actions-pin/internal/pinpool"
	"github.com/github/gh-actions-pin/internal/profile"
	"github.com/github/gh-actions-pin/internal/syncmap"
	"golang.org/x/sync/singleflight"
)

// reachabilityConcurrency bounds how many per-dependency reachability checks
// run in parallel in the REST fallback path.
const reachabilityConcurrency = 8

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

// WithCheckReachabilityFunc overrides the default REST-based reachability
// check. Intended for tests that want deterministic branch-discovery results.
func WithCheckReachabilityFunc(fn func(ctx context.Context, owner, repo, sha, ref string) (ReachabilityStatus, string)) Option {
	return func(r *Resolver) { r.checkReachFn = fn }
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

	// Domain-level caches: action resolution, reachability, branch hints.
	cache              syncmap.Map[ghapi.ActionRef, resolvedEntry]
	latestRefCache     syncmap.Map[ghapi.Repo, string]
	reachCache         syncmap.Map[ghapi.Reach, reachCacheEntry]
	reachSF            singleflight.Group
	reachInFlight      sync.Map // sfKey → struct{}, tracks deps submitted to pool
	branchHintBySHA    syncmap.Map[ghapi.NWOSha, string]
	releaseBranchCache syncmap.Map[ghapi.Repo, []ghapi.BranchHead]
	releaseBranchSF    singleflight.Group
	tagObjectCache     syncmap.Map[ghapi.NWOSha, tagPeel]

	// Test overrides (injected via With* options).
	checkReachFn func(ctx context.Context, owner, repo, sha, ref string) (ReachabilityStatus, string)
	nowFn        func() time.Time
	sleepFn      func(context.Context, time.Duration)

	// OnResolveProgress is called when a resolution batch makes progress.
	OnResolveProgress func(done, total int)
	// OnVerifyProgress is called when reachability verification makes progress.
	OnVerifyProgress func(done, total int)
	progressMu       sync.Mutex

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
	return r, nil
}

// --- Seeding (post-construction, deps come from lockfile loaded after resolver) ---

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

// SeedFromLockfile pre-warms the resolution and reachability caches so
// repeat runs skip redundant API calls. Do NOT call with --rescan: seeding
// would hide ref movement and skip reachability checks.
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
			resolvedEntry{dep: d},
		)
		r.reachCache.Put(
			ghapi.ForReach(owner, repo, d.SHA, d.Ref),
			reachCacheEntry{status: Reachable, detail: "seeded from lockfile"},
		)
	}
}

// --- Accessors ---

// Hostname returns the GitHub host the resolver is targeting.
func (r *Resolver) Hostname() string { return r.hostname }

// GHClient returns the unified API client.
func (r *Resolver) GHClient() *ghapi.Client { return r.gh }

// RepoIDs returns the numeric owner ID and repo ID for a NWO.
func (r *Resolver) RepoIDs(ctx context.Context, owner, repo string) (int64, int64, error) {
	return r.gh.RepoIDs(ctx, owner, repo)
}

// branchHint returns the branch previously recorded as containing sha in
// owner/repo, or "" if no hint exists.
func (r *Resolver) branchHint(owner, repo, sha string) string {
	hint, _ := r.branchHintBySHA.Get(ghapi.ForNWOSha(owner, repo, sha))
	return hint
}

// --- Cache helpers (package-internal) ---

func (r *Resolver) putReachCache(owner, repo, sha, ref string, status ReachabilityStatus, detail string) {
	r.reachCache.Put(ghapi.ForReach(owner, repo, sha, ref), reachCacheEntry{status: status, detail: detail})
}

func (r *Resolver) getReachCache(owner, repo, sha, ref string) (status ReachabilityStatus, detail string, ok bool) {
	entry, hit := r.reachCache.Get(ghapi.ForReach(owner, repo, sha, ref))
	if !hit {
		return "", "", false
	}
	return entry.status, entry.detail, true
}

// claimReachability marks a reachability key as in-flight. Returns true if
// this caller is the first to claim.
func (r *Resolver) claimReachability(key string) bool {
	_, loaded := r.reachInFlight.LoadOrStore(key, struct{}{})
	return !loaded
}

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

// FireVerifyProgress fires OnVerifyProgress. Safe from multiple goroutines.
func (r *Resolver) FireVerifyProgress(done, total int) {
	if r.OnVerifyProgress == nil {
		return
	}
	r.progressMu.Lock()
	defer r.progressMu.Unlock()
	r.OnVerifyProgress(done, total)
}
