package main

import (
	"context"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strings"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/cli/go-gh/v2/pkg/auth"
	"github.com/github/gh-actions-lock/cmd/gh-actions-lock/format"
	"github.com/github/gh-actions-lock/internal/filesource"
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
	src := filesource.Remote{Reader: client, Owner: spec.Owner, Repo: spec.Repo, SHA: sha}
	target := fmt.Sprintf("%s/%s@%s", spec.Owner, spec.Repo, ref)

	paths, err := remoteWorkflowPaths(ctx, src, opts.workflowPaths, target)
	if err != nil {
		return err
	}

	// A missing lockfile is an empty one, so every action ref is then
	// reported as not pinned.
	contents, err := src.ReadFile(ctx, parserlock.Path)
	if err != nil && !filesource.IsNotExist(err) {
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
	if f, ok := renamedTargetFinding(spec, meta.FullName); ok {
		report.RepoFindings = append(report.RepoFindings, f)
	}
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
// the explicit ones (which must exist) or every workflow in the directory.
func remoteWorkflowPaths(ctx context.Context, src filesource.Source, explicit []string, target string) ([]string, error) {
	const dir = ".github/workflows"
	if len(explicit) > 0 {
		paths := make([]string, 0, len(explicit))
		for _, p := range explicit {
			p = path.Clean(filepath.ToSlash(p))
			if _, err := src.ReadFile(ctx, p); err != nil {
				return nil, fmt.Errorf("reading workflow %s in %s: %w", p, target, err)
			}
			paths = append(paths, p)
		}
		return paths, nil
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
	return paths, nil
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
