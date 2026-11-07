package ghapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// This file is the one place a tag or branch name is looked up by exact
// name. Every caller (parity, reverse lookup, shadow checks) goes through
// it, so each gets the same rules:
//
//   - a name the Actions backend doesn't accept is never looked up;
//   - a name is looked up as refs/tags/<name>, then refs/heads/<name>, and
//     only a ref that exists by exactly that name counts (never a revision
//     expression, never a prefix match);
//   - a failed lookup is an error, never "not found";
//   - listings read every page;
//   - requests go through restGet, which handles the Dependabot proxy and
//     the anonymous fallback for public repositories behind SSO.
//
// GraphQL parity batches the same lookup as ref(qualifiedName:) in one
// query (see buildPinCheckQuery); refNames gives both paths the same names.

// refNames lists the full names name is looked up as, in order: a tag, then
// a branch. It is nil for a name the Actions backend doesn't accept; a refs/
// name is prefixed too, so it never resolves.
func refNames(name string) []string {
	if !AcceptedRefName(name) {
		return nil
	}
	return []string{"refs/tags/" + name, "refs/heads/" + name}
}

// refs lists the full names p's Head is looked up as.
func (p PinCheck) refs() []string { return refNames(p.Head) }

// RefKind is which namespace a ref name resolved in.
type RefKind int

const (
	// NoRef means the name is neither a tag nor a branch.
	NoRef RefKind = iota
	TagRef
	BranchRef
)

// RefTarget is what a ref name resolves to. Existence and the peeled
// commit are separate facts: a tag can exist yet not peel to a commit (it
// targets a tree or blob), and tag precedence keys off existence alone.
type RefTarget struct {
	Kind RefKind
	// Commit is the commit the ref peels to; empty when it doesn't peel
	// to one.
	Commit string
}

// LookupRefName looks name up the way the Actions backend resolver looks up
// a lock entry ref: refs/tags/<name>, then refs/heads/<name>, by exact name.
// A name the backend doesn't accept is never looked up.
func (c *Client) LookupRefName(ctx context.Context, owner, repo, name string) (RefTarget, error) {
	c, err := c.ForRepo(ctx, owner, repo)
	if err != nil {
		return RefTarget{}, err
	}
	for i, qualified := range refNames(name) {
		commit, exists, err := c.exactRef(ctx, owner, repo, qualified)
		if err != nil {
			return RefTarget{}, err
		}
		// An existing tag wins even if it doesn't peel to a commit: the
		// backend never falls through to a same-named branch.
		if exists {
			return RefTarget{Kind: []RefKind{TagRef, BranchRef}[i], Commit: commit}, nil
		}
	}
	return RefTarget{}, nil
}

// TagExists reports whether owner/repo has a tag named exactly name.
func (c *Client) TagExists(ctx context.Context, owner, repo, name string) (bool, error) {
	c, err := c.ForRepo(ctx, owner, repo)
	if err != nil {
		return false, err
	}
	ref, found, err := c.gitRef(ctx, owner, repo, "refs/tags/"+name)
	return found && ref.Object.SHA != "", err
}

// exactRef reports whether the exact ref qualified (refs/tags/x or
// refs/heads/x) exists and, separately, the commit it peels to ("" when it
// doesn't peel to a commit).
func (c *Client) exactRef(ctx context.Context, owner, repo, qualified string) (commit string, exists bool, err error) {
	ref, found, err := c.gitRef(ctx, owner, repo, qualified)
	if err != nil || !found || ref.Object.SHA == "" {
		return "", false, err
	}
	switch ref.Object.Type {
	case "commit":
		return ref.Object.SHA, true, nil
	case "tag":
		// An annotated tag: the ref exists, and a ref is resolved before
		// any other reading of the name, so commits/{ref} peels this one.
		sha, peeled, err := c.restCommit(ctx, owner, repo, qualified)
		if err != nil {
			return "", true, err
		}
		if !peeled {
			return "", true, nil
		}
		return sha, true, nil
	}
	return "", true, nil
}

type gitRefBody struct {
	Ref    string `json:"ref"`
	Object struct {
		SHA  string `json:"sha"`
		Type string `json:"type"`
	} `json:"object"`
}

type gitRefResult struct {
	ref   gitRefBody
	found bool
}

// gitRef reads git/ref/<qualified without refs/>. found is false unless a
// ref exists by exactly that name: git/ref answers a name that only prefixes
// other refs with an array. Answers are cached for the invocation and
// concurrent lookups of one ref are coalesced; errors are not cached.
func (c *Client) gitRef(ctx context.Context, owner, repo, qualified string) (gitRefBody, bool, error) {
	key := ForNWOName(owner, repo, qualified)
	if r, ok := c.gitRefCache.Get(key); ok {
		return r.ref, r.found, nil
	}
	v, err, _ := c.gitRefSF.Do(key.String(), func() (any, error) {
		if r, ok := c.gitRefCache.Get(key); ok {
			return r, nil
		}
		ref, found, err := c.fetchGitRef(ctx, owner, repo, qualified)
		if err != nil {
			return nil, err
		}
		r := gitRefResult{ref: ref, found: found}
		c.gitRefCache.Put(key, r)
		return r, nil
	})
	if err != nil {
		return gitRefBody{}, false, err
	}
	r := v.(gitRefResult)
	return r.ref, r.found, nil
}

func (c *Client) fetchGitRef(ctx context.Context, owner, repo, qualified string) (gitRefBody, bool, error) {
	var raw json.RawMessage
	path := fmt.Sprintf("repos/%s/%s/git/ref/%s", url.PathEscape(owner), url.PathEscape(repo), escapeBranchPath(strings.TrimPrefix(qualified, "refs/")))
	found, err := c.restGet(ctx, owner, path, &raw)
	if err != nil || !found {
		return gitRefBody{}, false, err
	}
	var ref gitRefBody
	if json.Unmarshal(raw, &ref) != nil || ref.Ref != qualified {
		return gitRefBody{}, false, nil
	}
	return ref, true, nil
}

// TagsAtCommit returns the names of every tag in owner/repo whose commit is
// sha, reading every page of the tag listing (one request per 100 tags).
// Use it only where a later page matters: ListTags reads the first page.
func (c *Client) TagsAtCommit(ctx context.Context, owner, repo, sha string) ([]string, error) {
	c, err := c.ForRepo(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	var names []string
	for page := 1; ; page++ {
		path := fmt.Sprintf("repos/%s/%s/tags?per_page=100&page=%d", url.PathEscape(owner), url.PathEscape(repo), page)
		var resp []struct {
			Name   string `json:"name"`
			Commit struct {
				SHA string `json:"sha"`
			} `json:"commit"`
		}
		found, err := c.restGet(ctx, owner, path, &resp)
		if err != nil {
			return nil, fmt.Errorf("listing tags for %s/%s: %w", owner, repo, err)
		}
		if !found {
			return names, nil
		}
		for _, t := range resp {
			if strings.EqualFold(t.Commit.SHA, sha) {
				names = append(names, t.Name)
			}
		}
		if len(resp) < 100 {
			return names, nil
		}
	}
}

func (c *Client) restCommit(ctx context.Context, owner, repo, ref string) (string, bool, error) {
	var commit struct {
		SHA string `json:"sha"`
	}
	path := fmt.Sprintf("repos/%s/%s/commits/%s", url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(ref))
	found, err := c.restGet(ctx, owner, path, &commit)
	if err != nil || !found {
		return "", false, err
	}
	return commit.SHA, true, nil
}

// restGet reports found=false for a 404 or 422. It reads anonymously under
// the Dependabot proxy, and falls back to an anonymous read for a public
// repository behind an organization's SSO.
func (c *Client) restGet(ctx context.Context, owner, path string, dest any) (bool, error) {
	var err error
	if c.restOnly {
		err = c.anonGet(ctx, path, dest)
	} else {
		err = c.rest.DoWithContext(ctx, http.MethodGet, path, nil, dest)
		if err != nil && c.orgPolicyFallback(ctx, owner, err) {
			err = c.anonGet(ctx, path, dest)
		}
	}
	if code, _ := StatusCode(err); code == http.StatusNotFound || code == http.StatusUnprocessableEntity {
		return false, nil
	}
	if err != nil {
		return false, c.ssoErr(owner, err)
	}
	return true, nil
}
