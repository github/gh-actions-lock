// Package cachekey defines comparable struct types used as map keys for the
// resolver- and doctor-side in-memory caches (branch lists, tag lists,
// compare results, reachability, action resolution, etc.).
//
// # Why not strings?
//
// Cache keys built from string concatenation (`owner + "/" + repo + "|" +
// strings.ToLower(sha)`) drift across callsites: each cache picks its own
// delimiter, each callsite remembers (or forgets) its own normalization,
// and a delimiter that happens to appear in user-supplied input can collide
// distinct tuples into one slot. Two callers writing the "same" key by
// different concatenations also miss each other's entries. Typed keys move
// every one of those decisions into one constructor per shape: callers hand
// over the components and get back a value that compares equal iff the
// tuple is the same.
//
// # Normalization
//
// All constructors apply consistent normalization:
//
//   - Owner and repo are lowercased. GitHub NWOs are case-insensitive, so
//     "Actions/Checkout" and "actions/checkout" name the same repository
//     and must share a cache slot.
//   - Hex commit SHAs are lowercased. The git wire protocol treats hex as
//     case-insensitive and our caches must too.
//   - Refs (tags, branches) preserve case. Git refs ARE case-sensitive.
//
// All struct fields are unexported so the constructors are the only way to
// build a key. This rules out callers building a value with un-normalized
// fields and silently reintroducing the collision class the package exists
// to prevent.
//
// # String forms
//
// Each key type has a String() method intended for diagnostics and for the
// few APIs that require a string key (e.g. golang.org/x/sync/singleflight).
// The string forms are stable across calls within a process but are NOT a
// parseable wire format: they pick delimiters that are convenient for
// humans and unambiguous within a single key shape, not invertible.
// Callers must not parse a String() result back into components and must
// not use it as a durable identifier outside the process.
//
// # When NOT to use this package
//
// Some "owner/repo@ref"-shaped strings in this codebase are part of a wire
// format (the lockfile's NWO@Ref dep key, the workflow YAML `uses:` line,
// resolver.ParentMap's child/parent keys). Those are contract strings, not
// cache keys, and must continue to use the canonical wire form
// (lockfile.Pin.IndexKey / lockfile.Dependency.Key). cachekey types are
// strictly for in-memory cache and dedup-set identity.
package cachekey

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

// String returns "owner/repo" (lowercased).
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

// String returns "owner/repo@ref". Diagnostics-only; do not parse.
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

// String returns "owner/repo|sha". Diagnostics-only; do not parse.
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

// String returns "owner/repo|name". Diagnostics-only; do not parse.
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

// String returns "owner/repo|base|head". Diagnostics-only; do not parse.
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

// String returns "owner/repo|sha|ref". Diagnostics-only; do not parse.
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

// String returns "owner/repo@ref" or "owner/repo/path@ref" when a sub-action
// path is set. Diagnostics-only; do not parse.
func (k ActionRef) String() string {
	if k.path == "" {
		return k.repo.String() + "@" + k.ref
	}
	return k.repo.String() + "/" + k.path + "@" + k.ref
}
