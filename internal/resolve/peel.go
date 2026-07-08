package resolve

import (
	"context"

	"github.com/github/gh-actions-lock/internal/ghapi"
)

// PeelTagObject reports whether sha is an annotated tag object in owner/repo
// and, if so, the commit SHA it dereferences to. Tag-of-tag chains are peeled
// server-side. Returns ok=false for lightweight tags, plain commits, unknown
// SHAs, or any lookup failure (fail open).
func (r *Resolver) PeelTagObject(ctx context.Context, owner, repo, sha string) (commit string, ok bool) {
	return r.peelTagObject(ctx, owner, repo, sha)
}

// IsKnownTagObject reports whether (owner, repo, sha) is already cached as
// an annotated tag object. Cache-only — never issues a network call.
func (r *Resolver) IsKnownTagObject(owner, repo, sha string) bool {
	key := ghapi.ForNWOSha(owner, repo, sha)
	cached, hit := r.tagObjectCache.Get(key)
	return hit && cached.isTag
}

func (r *Resolver) peelTagObject(ctx context.Context, owner, repo, sha string) (string, bool) {
	key := ghapi.ForNWOSha(owner, repo, sha)
	if cached, hit := r.tagObjectCache.Get(key); hit {
		return cached.commit, cached.isTag
	}

	res, err := r.gh.PeelTagObject(ctx, owner, repo, sha)
	if err != nil {
		return "", false
	}
	if res.Typename == "" {
		// OID not found — don't cache, may appear after fetch/permission grant.
		return "", false
	}
	if res.Typename != "Tag" {
		r.tagObjectCache.Put(key, tagPeel{})
		return "", false
	}
	if res.CommitOID == "" {
		// Tag that doesn't peel to a commit — ambiguous, don't cache.
		return "", false
	}
	r.tagObjectCache.Put(key, tagPeel{commit: res.CommitOID, isTag: true})
	return res.CommitOID, true
}

// ListBranches returns all branches for owner/repo.
func (r *Resolver) ListBranches(ctx context.Context, owner, repo string) ([]ghapi.BranchHead, error) {
	return r.gh.ListBranches(ctx, owner, repo)
}

// ListTagsForRepo returns all tags for owner/repo.
func (r *Resolver) ListTagsForRepo(ctx context.Context, owner, repo string) ([]ghapi.TagEntry, error) {
	return r.gh.ListTags(ctx, owner, repo)
}

// GetDefaultBranch returns the default branch name for owner/repo.
func (r *Resolver) GetDefaultBranch(ctx context.Context, owner, repo string) string {
	return r.gh.GetDefaultBranch(ctx, owner, repo)
}

// GetBranchHead looks up a single branch by name.
func (r *Resolver) GetBranchHead(ctx context.Context, owner, repo, name string) (ghapi.BranchHead, bool) {
	return r.gh.GetBranchHead(ctx, owner, repo, name)
}

// ListProtectedBranches returns branches with protection rules enabled.
func (r *Resolver) ListProtectedBranches(ctx context.Context, owner, repo string) []ghapi.BranchHead {
	return r.gh.ListProtectedBranches(ctx, owner, repo)
}

// MatchingHeadRefs returns branches whose name starts with prefix.
func (r *Resolver) MatchingHeadRefs(ctx context.Context, owner, repo, prefix string) []ghapi.BranchHead {
	return r.gh.MatchingHeadRefs(ctx, owner, repo, prefix)
}
