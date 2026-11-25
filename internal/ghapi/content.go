package ghapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
)

// RepositoryContents addresses one repository on the selected host, without
// the cross-host fallback used for action dependencies.
type RepositoryContents struct {
	client   *Client
	owner    string
	endpoint string
}

// Contents selects a repository on the client's home host.
func (c *Client) Contents(owner, repo string) RepositoryContents {
	if c.local != nil {
		c = c.local
	}
	return RepositoryContents{c, owner, "repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)}
}

// DefaultBranch returns the branch GitHub uses for repository URLs without a ref.
func (r RepositoryContents) DefaultBranch(ctx context.Context) (string, error) {
	var repo struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := r.get(ctx, r.endpoint, &repo); err != nil {
		return "", err
	}
	if !ValidRefName(repo.DefaultBranch) {
		return "", fmt.Errorf("repository did not return a default branch")
	}
	return repo.DefaultBranch, nil
}

// BranchHead only accepts exact branch names, even if GitHub returns prefix matches.
func (r RepositoryContents) BranchHead(ctx context.Context, branch string) (string, error) {
	var raw json.RawMessage
	if err := r.get(ctx, r.endpoint+"/git/ref/heads/"+escapeBranchPath(branch), &raw); err != nil {
		return "", err
	}
	var ref gitRefBody
	if json.Unmarshal(raw, &ref) != nil || ref.Ref != "refs/heads/"+branch || ref.Object.Type != "commit" || !fullSHAShaped(ref.Object.SHA) {
		return "", fmt.Errorf("branch %q: %w", branch, fs.ErrNotExist)
	}
	return ref.Object.SHA, nil
}

// CommitSHA resolves a read-only selector (for example a tag) to a snapshot.
func (r RepositoryContents) CommitSHA(ctx context.Context, ref string) (string, error) {
	var commit struct {
		SHA string `json:"sha"`
	}
	if err := r.get(ctx, r.endpoint+"/commits/"+url.PathEscape(ref), &commit); err != nil {
		return "", err
	}
	if !fullSHAShaped(commit.SHA) {
		return "", fmt.Errorf("GitHub did not return a commit SHA for %q", ref)
	}
	return commit.SHA, nil
}

// ContentEntry is an item in a directory returned by the Contents API.
type ContentEntry struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// List reads a directory at a fixed commit.
func (r RepositoryContents) List(ctx context.Context, commit, dir string) ([]ContentEntry, error) {
	var entries []ContentEntry
	if err := r.get(ctx, r.contentsPath(commit, dir), &entries); err != nil {
		return nil, err
	}
	// The Contents API stops at 1,000 entries. Never prune from a
	// potentially incomplete listing.
	if len(entries) >= 1000 {
		return nil, fmt.Errorf("%s has reached the Contents API's 1,000-entry limit; select a workflow file instead", dir)
	}
	return entries, nil
}

// ReadFile reads a repository file at a fixed commit.
func (r RepositoryContents) ReadFile(ctx context.Context, commit, name string) ([]byte, error) {
	var file struct {
		Type      string  `json:"type"`
		Path      string  `json:"path"`
		Encoding  string  `json:"encoding"`
		Content   *string `json:"content"`
		Submodule string  `json:"submodule_git_url"`
	}
	if err := r.get(ctx, r.contentsPath(commit, name), &file); err != nil {
		return nil, err
	}
	if file.Type != "file" || file.Path != name || file.Encoding != "base64" || file.Content == nil || file.Submodule != "" {
		return nil, fmt.Errorf("%s is not a regular file with base64 content in the Contents API response", name)
	}
	data, err := base64.StdEncoding.DecodeString(*file.Content)
	if err != nil {
		return nil, fmt.Errorf("decoding %s: %w", name, err)
	}
	return data, nil
}

func (r RepositoryContents) contentsPath(commit, name string) string {
	return r.endpoint + "/contents/" + escapeBranchPath(name) + "?ref=" + url.QueryEscape(commit)
}

func (r RepositoryContents) get(ctx context.Context, endpoint string, out any) error {
	err := r.client.rest.DoWithContext(ctx, http.MethodGet, endpoint, nil, out)
	if code, _ := StatusCode(err); code == http.StatusNotFound {
		return fmt.Errorf("%w: %w", fs.ErrNotExist, err)
	}
	return r.client.ssoErr(r.owner, err)
}

// FileChanges holds the additions, replacements, and deletions for one commit.
type FileChanges struct {
	Additions []FileAddition `json:"additions,omitempty"`
	Deletions []FileDeletion `json:"deletions,omitempty"`
}

// FileAddition contains a repository path and its full contents.
type FileAddition struct {
	Path     string `json:"path"`
	Contents []byte `json:"contents"` // JSON encodes bytes as base64.
}

// FileDeletion identifies a repository path to remove.
type FileDeletion struct {
	Path string `json:"path"`
}

// CommitChanges writes all files in one commit, only if the branch still
// points at the exact snapshot the caller inspected.
func (r RepositoryContents) CommitChanges(ctx context.Context, nwo, branch, expected, message string, changes FileChanges) (string, error) {
	const mutation = `mutation($input: CreateCommitOnBranchInput!) {
  createCommitOnBranch(input: $input) { commit { url } }
}`
	var result struct {
		CreateCommitOnBranch struct {
			Commit struct{ URL string }
		}
	}
	vars := map[string]any{"input": map[string]any{
		"branch":          map[string]string{"repositoryNameWithOwner": nwo, "branchName": branch},
		"expectedHeadOid": expected,
		"message":         map[string]string{"headline": message},
		"fileChanges":     changes,
	}}
	err := r.client.graphql.DoWithContext(withoutRetry(ctx), mutation, vars, &result)
	if err != nil {
		return "", fmt.Errorf("committing to %s (expected %s): %w", branch, expected, r.client.ssoErr(r.owner, err))
	}
	if result.CreateCommitOnBranch.Commit.URL == "" {
		return "", fmt.Errorf("GitHub did not return a commit URL for %s", branch)
	}
	return result.CreateCommitOnBranch.Commit.URL, nil
}

// CreateBranch creates a new branch at a snapshot without updating existing refs.
func (r RepositoryContents) CreateBranch(ctx context.Context, branch, sha string) error {
	var result json.RawMessage
	return r.post(ctx, "/git/refs", map[string]string{"ref": "refs/heads/" + branch, "sha": sha}, &result)
}

// OpenPullRequest opens a pull request between two branches in the repository.
func (r RepositoryContents) OpenPullRequest(ctx context.Context, base, head, title string) (string, error) {
	var pr struct {
		URL string `json:"html_url"`
	}
	err := r.post(ctx, "/pulls", map[string]string{"base": base, "head": head, "title": title}, &pr)
	if err != nil {
		return "", err
	}
	if pr.URL == "" {
		return "", fmt.Errorf("GitHub did not return a pull request URL")
	}
	return pr.URL, nil
}

func (r RepositoryContents) post(ctx context.Context, suffix string, payload map[string]string, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	err = r.client.rest.DoWithContext(withoutRetry(ctx), http.MethodPost, r.endpoint+suffix, bytes.NewReader(body), out)
	return r.client.ssoErr(r.owner, err)
}

type noRetryKey struct{}

// A lost write response isn't proof that GitHub didn't apply the mutation.
func withoutRetry(ctx context.Context) context.Context {
	return context.WithValue(ctx, noRetryKey{}, true)
}
