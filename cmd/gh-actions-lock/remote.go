package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/ghapi"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/pin"
	"github.com/github/gh-actions-lock/internal/resolve"
)

const workflowsPath = ".github/workflows"

var repoNamePart = regexp.MustCompile(`^[a-zA-Z0-9_-][a-zA-Z0-9_.-]*$`)

type remoteTarget struct {
	host, owner, repo string
	ref, workflow     string
}

func (t remoteTarget) nwo() string { return t.owner + "/" + t.repo }

func (opts *checkOptions) selectRemote() error {
	target := opts.repo
	if target == "" && len(opts.workflowPaths) > 0 {
		arg := opts.workflowPaths[0]
		if strings.Contains(arg, "://") {
			target = arg
		} else if _, err := os.Stat(arg); errors.Is(err, fs.ErrNotExist) {
			owner, repo, ok := strings.Cut(arg, "/")
			if ok && !workflowName(repo) && repoNamePart.MatchString(owner) && repoNamePart.MatchString(repo) {
				target = arg
			}
		}
	}
	if target == "" {
		if opts.ref != "" || opts.remoteCommit || opts.remotePR {
			return fmt.Errorf("--ref, --commit and --pr require a remote repository target")
		}
		return nil
	}
	if (opts.repo != "" && len(opts.workflowPaths) != 0) || (opts.repo == "" && len(opts.workflowPaths) != 1) {
		return fmt.Errorf("pass a single remote repository, workflow directory, or workflow URL, without local paths")
	}
	if opts.verifyLocal {
		return fmt.Errorf("--verify-local needs a checkout; use --verify for a remote repository")
	}
	if opts.remoteCommit && opts.remotePR {
		return fmt.Errorf("--commit and --pr are mutually exclusive")
	}
	if (opts.remoteCommit || opts.remotePR) && (opts.verify || opts.noFix) {
		return fmt.Errorf("--commit and --pr cannot be combined with --verify or --no-fix")
	}
	if opts.migrateLocalActions {
		return fmt.Errorf("--migrate-local-actions needs a checkout to scan all in-repository action files")
	}
	if ghapi.DependabotProxy() {
		return fmt.Errorf("remote repository targets are not supported under the Dependabot proxy")
	}
	t, err := parseRemoteTarget(target, opts.hostname, opts.ref)
	if err != nil {
		return err
	}
	if (opts.remoteCommit || opts.remotePR) && t.detached() {
		return fmt.Errorf("remote writes require a branch, not %q", t.ref)
	}
	opts.remote = &t
	opts.noFix = !opts.remoteCommit && !opts.remotePR
	return nil
}

func parseRemoteTarget(value, hostname, ref string) (remoteTarget, error) {
	var t remoteTarget
	if strings.Contains(value, "://") {
		u, err := url.Parse(value)
		if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.Host != u.Hostname() {
			return t, fmt.Errorf("expected an HTTPS GitHub repository or workflow URL")
		}
		t.host = strings.ToLower(u.Hostname())
		if hostname != "" && !strings.EqualFold(hostname, t.host) {
			return t, fmt.Errorf("URL host %s does not match --hostname %s", t.host, hostname)
		}
		parts := strings.SplitN(strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), "/"), "/", 3)
		if len(parts) < 2 {
			return t, fmt.Errorf("repository URL must include an owner and repository")
		}
		t.owner, t.repo = parts[0], strings.TrimSuffix(parts[1], ".git")
		if len(parts) == 3 {
			kind, tail, _ := strings.Cut(parts[2], "/")
			switch kind {
			case "tree":
				t.ref = strings.TrimSuffix(tail, "/"+workflowsPath)
				if t.ref == "" {
					return t, fmt.Errorf("expected a branch or ref in the tree URL")
				}
			case "blob":
				marker := "/" + workflowsPath + "/"
				at := strings.LastIndex(tail, marker)
				if at <= 0 || !workflowName(tail[at+len(marker):]) {
					return t, fmt.Errorf("expected a workflow file under %s in the blob URL", workflowsPath)
				}
				t.ref = tail[:at]
				t.workflow = tail[at+1:]
			default:
				if parts[2] != workflowsPath {
					return t, fmt.Errorf("expected a repository, workflow directory, or workflow file URL")
				}
			}
		}
	} else {
		t.owner, t.repo, _ = strings.Cut(value, "/")
		t.host = strings.ToLower(hostname)
		if t.host == "" {
			t.host = strings.ToLower(os.Getenv("GH_HOST"))
		}
		if t.host == "" {
			t.host = "github.com"
		}
	}
	if t.host != "github.com" && !ghapi.IsProxima(t.host) {
		return t, fmt.Errorf("unsupported GitHub host %q", t.host)
	}
	if !repoNamePart.MatchString(t.owner) || !repoNamePart.MatchString(t.repo) {
		return t, fmt.Errorf("expected a repository in OWNER/REPO format")
	}
	if ref != "" {
		if t.ref != "" && t.ref != ref {
			return t, fmt.Errorf("URL ref %q does not match --ref %q", t.ref, ref)
		}
		t.ref = ref
	}
	short := strings.TrimPrefix(t.ref, "refs/heads/")
	if short == t.ref {
		short = strings.TrimPrefix(t.ref, "refs/tags/")
	}
	if t.ref != "" && !ghapi.ValidRefName(short) {
		return t, fmt.Errorf("invalid Git ref %q", t.ref)
	}
	return t, nil
}

func workflowName(name string) bool {
	if !fs.ValidPath(name) || strings.ContainsAny(name, "/\\") {
		return false
	}
	ext := path.Ext(name)
	return ext == ".yml" || ext == ".yaml"
}

func (t remoteTarget) detached() bool {
	return strings.HasPrefix(t.ref, "refs/tags/") || parserlock.IsFullSha(t.ref)
}

type remoteSnapshot struct {
	target      remoteTarget
	api         ghapi.RepositoryContents
	branch, sha string
	mu          sync.Mutex
	original    map[string]snapshotFile
	pending     map[string][]byte
}

type snapshotFile struct {
	data []byte
	err  error
}

func loadRemote(ctx context.Context, target remoteTarget, r *resolve.Resolver, write bool) (*remoteSnapshot, []string, *lockfile.State, error) {
	s, err := openRemote(ctx, target, r.GHClient(), write)
	if err != nil {
		return nil, nil, nil, err
	}
	paths, err := s.workflows(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	data, err := s.read(ctx, parserlock.Path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil, fmt.Errorf("reading lockfile: %w", err)
	}
	store, err := lockfile.LoadContents(data, r)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("opening lockfile: %w", err)
	}
	if len(paths) == 0 && len(store.WorkflowKeys()) == 0 {
		return nil, nil, nil, noWorkflowsError("")
	}
	return s, paths, store, nil
}

func openRemote(ctx context.Context, target remoteTarget, gc *ghapi.Client, write bool) (*remoteSnapshot, error) {
	s := &remoteSnapshot{
		target: target, api: gc.Contents(target.owner, target.repo),
		original: map[string]snapshotFile{}, pending: map[string][]byte{},
	}
	s.branch = strings.TrimPrefix(target.ref, "refs/heads/")
	if target.ref == "" {
		branch, err := s.api.DefaultBranch(ctx)
		if err != nil {
			return nil, fmt.Errorf("reading default branch: %w", err)
		}
		s.branch = branch
	}
	if !target.detached() {
		sha, err := s.api.BranchHead(ctx, s.branch)
		if err == nil {
			s.sha = sha
			return s, nil
		}
		if write || !errors.Is(err, fs.ErrNotExist) || target.ref == "" || strings.HasPrefix(target.ref, "refs/heads/") {
			return nil, fmt.Errorf("reading branch %q: %w", s.branch, err)
		}
	}
	ref := target.ref
	if !strings.HasPrefix(ref, "refs/tags/") && !parserlock.IsFullSha(ref) {
		ref = "refs/tags/" + ref
	}
	sha, err := s.api.CommitSHA(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("reading ref %q: %w", target.ref, err)
	}
	s.sha = sha
	return s, nil
}

func (s *remoteSnapshot) workflows(ctx context.Context) ([]string, error) {
	if s.target.workflow != "" {
		if _, err := s.read(ctx, s.target.workflow); err != nil {
			return nil, fmt.Errorf("reading %s: %w", s.target.workflow, err)
		}
		return []string{s.target.workflow}, nil
	}
	entries, err := s.api.List(ctx, s.sha, workflowsPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing workflows: %w", err)
	}
	var paths []string
	for _, entry := range entries {
		if !workflowName(entry.Name) || entry.Type == "dir" {
			continue
		}
		if entry.Type != "file" {
			return nil, fmt.Errorf("%s/%s is not a regular workflow file", workflowsPath, entry.Name)
		}
		paths = append(paths, workflowsPath+"/"+entry.Name)
	}
	slices.Sort(paths)
	return paths, nil
}

func (s *remoteSnapshot) read(ctx context.Context, name string) ([]byte, error) {
	s.mu.Lock()
	cached, ok := s.original[name]
	s.mu.Unlock()
	if ok {
		return cached.data, cached.err
	}
	data, err := s.api.ReadFile(ctx, s.sha, name)
	s.mu.Lock()
	s.original[name] = snapshotFile{data, err}
	s.mu.Unlock()
	return data, err
}

func (s *remoteSnapshot) stage(name string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	orig, ok := s.original[name]
	if !ok || orig.err != nil && !errors.Is(orig.err, fs.ErrNotExist) {
		return fmt.Errorf("cannot write %s without reading its original contents", name)
	}
	if data == nil && orig.err != nil || data != nil && orig.err == nil && bytes.Equal(data, orig.data) {
		return nil
	}
	s.pending[name] = data
	return nil
}

func (s *remoteSnapshot) saveLockfile(store *lockfile.State) error {
	data, err := store.Contents()
	if err != nil {
		return err
	}
	return s.stage(parserlock.Path, data)
}

func (s *remoteSnapshot) commitOptions(ctx context.Context, opts *pin.CommitOptions) {
	opts.ReadFile = func(name string) ([]byte, error) { return s.read(ctx, name) }
	opts.WriteFile = s.stage
	opts.SaveLockfile = s.saveLockfile
}

func (s *remoteSnapshot) publish(ctx context.Context, openPR bool) (string, string, error) {
	if len(s.pending) == 0 {
		return "", "", nil
	}
	var names []string
	for name := range s.pending {
		names = append(names, name)
	}
	slices.Sort(names)
	var changes ghapi.FileChanges
	for _, name := range names {
		if data := s.pending[name]; data != nil {
			changes.Additions = append(changes.Additions, ghapi.FileAddition{Path: name, Contents: data})
		} else {
			changes.Deletions = append(changes.Deletions, ghapi.FileDeletion{Path: name})
		}
	}
	const message = "Lock GitHub Actions dependencies"
	head := s.branch
	if openPR {
		sha, err := s.api.BranchHead(ctx, s.branch)
		if err != nil {
			return "", "", fmt.Errorf("checking pull request base %q: %w", s.branch, err)
		}
		if sha != s.sha {
			return "", "", fmt.Errorf("branch %q moved since the scan; rerun before opening a pull request", s.branch)
		}
		head = "actions-lock/" + strings.ToLower(rand.Text())
		if err := s.api.CreateBranch(ctx, head, s.sha); err != nil {
			return "", "", fmt.Errorf("creating pull request branch %q: %w", head, err)
		}
	}
	commitURL, err := s.api.CommitChanges(ctx, s.target.nwo(), head, s.sha, message, changes)
	if err != nil && openPR {
		return "", "", fmt.Errorf("pull request branch %q was created, but the commit was not confirmed: %w", head, err)
	}
	if err != nil || !openPR {
		return commitURL, "", err
	}
	prURL, err := s.api.OpenPullRequest(ctx, s.branch, head, message)
	if err != nil {
		return commitURL, "", fmt.Errorf("opening pull request from %q into %q (changes committed at %s): %w", head, s.branch, commitURL, err)
	}
	return commitURL, prURL, nil
}
