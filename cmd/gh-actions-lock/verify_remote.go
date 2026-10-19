package main

import (
	"context"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strings"

	"github.com/cli/go-gh/v2/pkg/auth"
	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/cmd/gh-actions-lock/format"
	"github.com/github/gh-actions-lock/internal/filesource"
	"github.com/github/gh-actions-lock/internal/ghapi"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/pinpool"
	"github.com/github/gh-actions-lock/internal/pipeline"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/github/gh-actions-lock/internal/repospec"
	"github.com/github/gh-actions-lock/internal/resolve"
	"github.com/github/gh-actions-lock/internal/ui"
	"github.com/github/gh-actions-lock/internal/workflowfile"
)

// remoteVerifier verifies a repository through the API at a single commit.
// It never touches the working tree or the cwd: no clone, no lockfile
// write, no run log.
type remoteVerifier struct {
	opts        *checkOptions
	newResolver resolverFunc
}

// Verify implements verifier.
func (v remoteVerifier) Verify(ctx context.Context, out io.Writer, console *ui.UI) error {
	ctx = ghapi.WithSnapshotRedirects(ctx)
	opts, spec := v.opts, v.opts.remote
	hostname := remoteHostname(spec, opts.hostname)

	pool := pinpool.New(0, console)
	newResolver := v.newResolver
	if newResolver == nil {
		newResolver = func(hostname string, pool *pinpool.Pool) (*resolve.Resolver, error) {
			return resolve.New(hostname, pool)
		}
	}
	r, err := newResolver(hostname, pool)
	if err != nil {
		return err
	}
	client := r.GHClient()
	if client == nil {
		return fmt.Errorf("no API client available for %s", hostname)
	}
	// Without a pin, a 404 on a tenant host falls back to a public github.com
	// repository of the same name and verifies the wrong repository.
	if err := client.PinHost(spec.Owner, spec.Repo, hostname); err != nil {
		return err
	}

	meta, err := client.RepoMetadata(ctx, spec.Owner, spec.Repo)
	if err != nil {
		return err
	}
	ref := spec.Ref
	if ref == "" {
		ref = meta.DefaultBranch
		if ref == "" {
			return fmt.Errorf("could not determine the default branch of %s/%s", spec.Owner, spec.Repo)
		}
	}
	sha, err := client.CommitSHA(ctx, spec.Owner, spec.Repo, ref)
	if err != nil {
		return err
	}
	if !parserlock.IsFullSha(sha) {
		return fmt.Errorf("resolving %s/%s@%s: API did not return a full commit SHA", spec.Owner, spec.Repo, ref)
	}
	src := filesource.Remote{Reader: client, Owner: spec.Owner, Repo: spec.Repo, SHA: sha}
	target := fmt.Sprintf("%s/%s@%s", spec.Owner, spec.Repo, ref)

	paths, err := remoteWorkflowPaths(ctx, src, opts.workflowPaths, target)
	if err != nil {
		return err
	}

	// A missing lockfile is an empty one, but an untracked workflow can still
	// pass the per-workflow checks (e.g. one with only local actions), so the
	// absence is also reported as a blocking repository finding.
	contents, err := src.ReadFile(ctx, parserlock.Path)
	lockfileMissing := filesource.IsNotExist(err)
	if err != nil && !lockfileMissing {
		return fmt.Errorf("reading %s: %w", parserlock.Path, err)
	}
	store, err := lockfile.ParseState(contents, r)
	if err != nil {
		return fmt.Errorf("opening lockfile: %w", err)
	}
	if err := store.SetHostname(r.Hostname()); err != nil {
		return err
	}
	if err := r.SeedHosts(store.AllDeps()); err != nil {
		return err
	}
	scope := make([]string, len(paths))
	for i, p := range paths {
		scope[i] = workflowfile.KeyFromPath(p)
	}
	if err := store.VerifyHosts(ctx, scope); err != nil {
		return err
	}
	r.SeedBranchHints(store.AllDeps())
	r.SeedFromLockfile(store.AllDeps())

	if opts.jsonFields == "" {
		console.SetLog(io.Discard)
		console.TermDetail("Verifying %s (%s)", target, shortSHA(sha))
	}

	result, err := pipeline.Run(ctx, pipeline.RunOptions{
		WorkflowPaths: paths,
		Source:        src,
		Resolver:      r,
		Store:         store,
		Pool:          pool,
		Verify:        true,
	})
	console.StopProgress()
	if err != nil {
		return err
	}

	report, valid := result.Report, result.Valid
	if lockfileMissing {
		report.RepoFindings = append(report.RepoFindings, missingLockfileFinding(target))
	}
	if f, ok := renamedTargetFinding(spec, meta.FullName); ok {
		report.RepoFindings = append(report.RepoFindings, f)
	}
	valid = report.IsValid()
	if opts.jsonFields != "" {
		if err := format.WriteJSON(out, report, nil, valid, opts.jsonFields, cliVersion(), store.File().Version, r.Hostname()); err != nil {
			return err
		}
	} else {
		format.PresentResults(console, report, valid, false)
	}
	if !valid {
		if opts.jsonFields == "" {
			format.PresentReadOnlyFailures(console, report)
		}
		return errSilent
	}
	if opts.jsonFields == "" {
		n := len(paths)
		console.TermSuccess("All %d %s valid", n, ui.Pluralize(n, "workflow", "workflows"))
	}
	return nil
}

// missingLockfileFinding is the blocking repository-level result for a
// target with no lockfile.
func missingLockfileFinding(target string) checks.Finding {
	return checks.Finding{
		Category:    checks.NotPinned,
		Severity:    checks.SeverityError,
		Confidence:  checks.ConfidenceHigh,
		Detail:      fmt.Sprintf("%s has no %s", target, parserlock.Path),
		Remediation: "Run `gh actions-lock` in the repository to create the lockfile.",
		DocURL:      pipeline.DocURLFor(checks.NotPinned),
	}
}

// renamedTargetFinding reports a target that GitHub redirected to a new
// name. The target's own identity is not locked anywhere, so this says only
// where the repository lives now, and never blocks. Case differences are
// not renames.
func renamedTargetFinding(spec *repospec.Spec, canonical string) (checks.Finding, bool) {
	requested := spec.Owner + "/" + spec.Repo
	if canonical == "" || strings.EqualFold(canonical, requested) {
		return checks.Finding{}, false
	}
	return checks.Finding{
		Category:    checks.RepoRenamed,
		Severity:    checks.SeverityWarning,
		Confidence:  checks.ConfidenceHigh,
		Detail:      fmt.Sprintf("%s was renamed or transferred and is now %s", requested, canonical),
		Remediation: fmt.Sprintf("Refer to %s directly.", canonical),
	}, true
}

// remoteHostname ignores the cwd repository: a remote target must not pick
// up whatever repository the command happens to run inside.
func remoteHostname(spec *repospec.Spec, override string) string {
	switch {
	case spec.Host != "":
		return strings.ToLower(spec.Host)
	case override != "":
		return strings.ToLower(override)
	}
	host, _ := auth.DefaultHost()
	return strings.ToLower(host)
}

// remoteWorkflowPaths returns the repo-relative workflow paths to verify:
// the explicit ones (which must exist) plus the same-repository reusable
// workflows they call, or every workflow in the directory.
func remoteWorkflowPaths(ctx context.Context, src filesource.Source, explicit []string, target string) ([]string, error) {
	const dir = ".github/workflows"
	if len(explicit) > 0 {
		return withReusableWorkflows(ctx, src, explicit, target)
	}
	names, err := src.ListFiles(ctx, dir)
	if err != nil && !filesource.IsNotExist(err) {
		return nil, fmt.Errorf("listing %s in %s: %w", dir, target, err)
	}
	var paths []string
	for _, n := range names {
		if ext := path.Ext(n); ext == ".yml" || ext == ".yaml" {
			paths = append(paths, path.Join(dir, n))
		}
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no workflow files found in %s of %s", dir, target)
	}
	return withReusableWorkflows(ctx, src, paths, target)
}

// withReusableWorkflows expands explicit workflow paths through job-level
// same-repository reusable workflow calls, so a selected caller cannot pass
// while the workflow it calls is missing or unlocked. Each workflow is
// visited once, which also terminates call cycles. A workflow that does not
// parse is kept and reported by the pipeline.
func withReusableWorkflows(ctx context.Context, src filesource.Source, explicit []string, target string) ([]string, error) {
	type item struct{ path, caller string }
	queue := make([]item, 0, len(explicit))
	for _, p := range explicit {
		queue = append(queue, item{path: path.Clean(filepath.ToSlash(p))})
	}
	seen := make(map[string]bool)
	var paths []string
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		if seen[it.path] {
			continue
		}
		seen[it.path] = true
		content, err := src.ReadFile(ctx, it.path)
		if err != nil {
			if it.caller != "" {
				return nil, fmt.Errorf("reading reusable workflow %s called by %s in %s: %w", it.path, it.caller, target, err)
			}
			return nil, fmt.Errorf("reading workflow %s in %s: %w", it.path, target, err)
		}
		paths = append(paths, it.path)
		wf, err := workflowfile.Parse(it.path, content)
		if err != nil {
			continue
		}
		for _, callee := range wf.ReusableWorkflowPaths() {
			queue = append(queue, item{path: callee, caller: it.path})
		}
	}
	return paths, nil
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
