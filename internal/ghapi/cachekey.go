// Cache key types: comparable structs for in-memory caches.
//
// Constructors centralize normalization: owner, repo, and hex SHAs are
// lowercased; refs preserve case because git refs are case-sensitive.
// Struct fields stay unexported so callers cannot build unnormalized keys.
//
// String methods are diagnostics-only. Do not parse them or use them as
// durable identifiers.

package ghapi

import "strings"

// Repo identifies a GitHub repository by owner and name. Both components
// are lowercased on construction so case variants share a single cache
// slot.
type Repo struct {
	owner string
	repo  string
}

// ForRepo builds a Repo key, lowercasing owner and repo.
func ForRepo(owner, repo string) Repo {
	return Repo{owner: strings.ToLower(owner), repo: strings.ToLower(repo)}
}

// String returns the normalized "owner/repo" key, used for cache-key
// diagnostics and for singleflight coalescing.
func (r Repo) String() string {
	return r.owner + "/" + r.repo
}

// NWORef pairs a repo with a git ref (tag, branch, or SHA-as-written).
// Refs preserve their original case because git refs are case-sensitive.
type NWORef struct {
	repo Repo
	ref  string
}

// ForNWORef builds an NWORef key.
func ForNWORef(owner, repo, ref string) NWORef {
	return NWORef{repo: ForRepo(owner, repo), ref: ref}
}

// String is diagnostics-only; do not parse.
func (k NWORef) String() string {
	return k.repo.String() + "@" + k.ref
}

// NWOSha pairs a repo with a lowercased commit SHA.
type NWOSha struct {
	repo Repo
	sha  string
}

// ForNWOSha builds an NWOSha key, lowercasing the SHA.
func ForNWOSha(owner, repo, sha string) NWOSha {
	return NWOSha{repo: ForRepo(owner, repo), sha: strings.ToLower(sha)}
}

// String is diagnostics-only; do not parse.
func (k NWOSha) String() string {
	return k.repo.String() + "|" + k.sha
}

// NWOName pairs a repo with an arbitrary name (e.g. a branch name). The
// name preserves case; callers that want case-insensitive lookup must
// normalize before passing in.
type NWOName struct {
	repo Repo
	name string
}

// ForNWOName builds an NWOName key.
func ForNWOName(owner, repo, name string) NWOName {
	return NWOName{repo: ForRepo(owner, repo), name: name}
}

// String is diagnostics-only; do not parse.
func (k NWOName) String() string {
	return k.repo.String() + "|" + k.name
}

// Compare identifies a Compare API result between two SHAs in a repo,
// e.g. "is base an ancestor of head?". Both SHAs are lowercased.
type Compare struct {
	repo Repo
	base string
	head string
}

// ForCompare builds a Compare key, lowercasing both SHAs.
func ForCompare(owner, repo, base, head string) Compare {
	return Compare{
		repo: ForRepo(owner, repo),
		base: strings.ToLower(base),
		head: strings.ToLower(head),
	}
}

// String is diagnostics-only; do not parse.
func (k Compare) String() string {
	return k.repo.String() + "|" + k.base + "|" + k.head
}

// Reach is (NWO, sha, ref) — the reachability cache distinguishes the same
// SHA on different refs because the verdict depends on which ref's history
// we're walking. The SHA is lowercased; the ref preserves case.
type Reach struct {
	repo Repo
	sha  string
	ref  string
}

// ForReach builds a Reach key, lowercasing the SHA.
func ForReach(owner, repo, sha, ref string) Reach {
	return Reach{
		repo: ForRepo(owner, repo),
		sha:  strings.ToLower(sha),
		ref:  ref,
	}
}

// String is diagnostics-only; do not parse.
func (k Reach) String() string {
	return k.repo.String() + "|" + k.sha + "|" + k.ref
}

// ActionRef is the path-aware key used by the resolver's per-ref cache and
// the BFS dedup set in ResolveAllRecursive. Sub-action paths must be
// distinct identities for graph traversal — actions/cache/save@v4 visits
// a different action.yml than actions/cache@v4 — even though they collapse
// to the same repo+ref tarball at runner-download granularity. Use NWORef
// (not ActionRef) when path is irrelevant.
type ActionRef struct {
	repo Repo
	path string
	ref  string
}

// ForActionRef builds an ActionRef key.
func ForActionRef(owner, repo, path, ref string) ActionRef {
	return ActionRef{repo: ForRepo(owner, repo), path: path, ref: ref}
}

// String is diagnostics-only; do not parse.
func (k ActionRef) String() string {
	if k.path == "" {
		return k.repo.String() + "@" + k.ref
	}
	return k.repo.String() + "/" + k.path + "@" + k.ref
}
