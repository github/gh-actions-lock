package pipeline

import (
	"context"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-pin/internal/dep"
	"github.com/github/gh-actions-pin/internal/ghapi"
	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/pinpool"
	"github.com/github/gh-actions-pin/internal/pipeline/checks"
	"github.com/github/gh-actions-pin/internal/resolve"
	"github.com/github/gh-actions-pin/internal/workflowfile"
)

// Diagnose scans workflows and produces findings for each.
//
// It is a backward-compatible wrapper around ParseAll, resolver pre-warming,
// and DiagnoseParsed. Newer callers can drive those phases directly to control
// UI progress.
func Diagnose(ctx context.Context, paths []string, r *resolve.Resolver, store *lockfile.State, pool *pinpool.Pool, onWorkflow ...func(done, total int, path string)) *checks.Report {
	var onScan func(done, total int, path string)
	if len(onWorkflow) > 0 {
		onScan = onWorkflow[0]
	}
	parsed := ParseAll(paths, store, onScan)
	if r != nil {
		refs, deps := CollectResolvable(parsed)
		if len(refs) > 0 {
			_, _, _ = r.ResolveAllRecursive(ctx, refs)
		}
		if len(deps) > 0 {
			_ = r.CheckReachabilityAll(ctx, deps)
		}
	}
	return DiagnoseParsed(ctx, parsed, r, store, pool)
}

// ParseAll loads and parses every workflow path, returning a slice in input
// order. onScan, if non-nil, fires with 1-based progress before each workflow
// is parsed so the UI can render [i/N] without leaking resolver detail.
func ParseAll(paths []string, store *lockfile.State, onScan func(done, total int, path string)) []checks.ParsedWorkflow {
	total := len(paths)
	out := make([]checks.ParsedWorkflow, 0, total)
	for i, path := range paths {
		if onScan != nil {
			onScan(i+1, total, path)
		}
		pw := checks.ParsedWorkflow{Path: path}
		wf, err := workflowfile.Load(path)
		if err != nil {
			pw.LoadErr = err
			out = append(out, pw)
			continue
		}
		pw.Refs, _, pw.ParseWarnings = wf.ExtractActionRefs()
		if len(pw.Refs) > 0 {
			wfKey := workflowfile.KeyFromPath(path)
			deps, depsErr := store.Get(wfKey)
			if depsErr != nil {
				pw.DepsErr = depsErr
			} else {
				pw.ExistingDeps = deps
			}
		}
		out = append(out, pw)
	}
	return out
}

// CollectResolvable returns the deduplicated union of refs and existing deps
// across all parsed workflows. Use the returned slices to pre-warm the
// resolver caches once before per-workflow diagnostics.
func CollectResolvable(parsed []checks.ParsedWorkflow) ([]parserlock.ActionRef, []dep.Dependency) {
	seenRef := make(map[ghapi.ActionRef]bool)
	var refs []parserlock.ActionRef
	for _, pw := range parsed {
		for _, ref := range pw.Refs {
			key := ghapi.ForActionRef(ref.Owner, ref.Repo, ref.Path, ref.Ref)
			if seenRef[key] {
				continue
			}
			seenRef[key] = true
			refs = append(refs, ref)
		}
	}
	seenDep := make(map[string]bool)
	var deps []dep.Dependency
	for _, pw := range parsed {
		for _, dep := range pw.ExistingDeps {
			key := dep.Key()
			if seenDep[key] {
				continue
			}
			seenDep[key] = true
			deps = append(deps, dep)
		}
	}
	return refs, deps
}

// DiagnoseParsed runs the engine diagnostics for each pre-parsed workflow.
// Assumes the resolver caches have already been warmed (calls into the
// resolver will hit cache and stay silent). Returns a checks.Report aggregating per-
// workflow findings in input order.
