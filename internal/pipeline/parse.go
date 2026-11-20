package pipeline

import (
	"context"
	"errors"
	"strings"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/ghapi"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/pinpool"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/github/gh-actions-lock/internal/resolve"
	"github.com/github/gh-actions-lock/internal/workflowfile"
)

// Diagnose scans workflows and produces findings for each.
//
// It is a backward-compatible wrapper around ParseAll, resolver pre-warming,
// and DiagnoseParsed. Newer callers can drive those phases directly to control
// UI progress.
func Diagnose(ctx context.Context, paths []string, r *resolve.Resolver, store *lockfile.State, pool *pinpool.Pool) *checks.Report {
	parsed := ParseAll(paths, store)
	keepActionFileNames(r, parsed)
	if r != nil {
		refs, _ := CollectResolvable(parsed)
		if len(refs) > 0 {
			_, _, _ = r.ResolveAllRecursive(ctx, refs)
		}
	}
	return DiagnoseParsed(ctx, parsed, r, store, pool)
}

// keepActionFileNames resolves every ref an in-repo `$/…` action file holds
// under the name it is written with. Runs never write action files.
func keepActionFileNames(r *resolve.Resolver, parsed []checks.ParsedWorkflow) {
	if r == nil {
		return
	}
	for _, pw := range parsed {
		r.KeepWrittenNames(pw.SelfActionRefs)
	}
}

// ParseAll loads and parses every workflow path, returning a slice in input
// order. onScan, if non-nil, fires with 1-based progress before each workflow
// is parsed so the UI can render [i/N] without leaking resolver detail.
func ParseAll(paths []string, store *lockfile.State) []checks.ParsedWorkflow {
	total := len(paths)
	out := make([]checks.ParsedWorkflow, 0, total)
	for _, path := range paths {
		pw := checks.ParsedWorkflow{Path: path}
		if store != nil {
			wfKey := workflowfile.KeyFromPath(path)
			deps, depsErr := store.Get(wfKey)
			if depsErr != nil {
				pw.DepsErr = depsErr
			} else {
				pw.ExistingDeps = deps
			}
		}
		wf, err := workflowfile.Load(path)
		if err != nil {
			pw.LoadErr = err
			out = append(out, pw)
			continue
		}
		scan := wf.ExtractActionRefs()
		selfScan := workflowfile.ScanSelfRepositoryActions(path, scan.SelfRepositoryActionRefs)
		// A reference that can't be parsed is never skipped: the workflow
		// would lock without it.
		if bad := mergeStrings(scan.MalformedRefs, selfScan.MalformedRefs); len(bad) > 0 {
			errs := make([]error, len(bad))
			for i, msg := range bad {
				errs[i] = errors.New(msg)
			}
			pw.LoadErr = errors.Join(errs...)
			out = append(out, pw)
			continue
		}

		pw.SHASpellings = lowercaseSHAs(scan.Refs, selfScan.Refs)
		pw.Refs = mergeActionRefs(scan.Refs, selfScan.Refs)
		pw.RewriteRefs = rewritableRefs(pw.Refs, selfScan.Refs)
		pw.WorkflowRefs = scan.Refs
		pw.SelfActionRefs = selfScan.Refs
		pw.SelfActionFiles = selfScan.RefFiles
		pw.LocalPaths = scan.LocalPaths
		pw.NestedLocalUses = selfScan.LocalUses
		pw.SelfRepositoryRefs = mergeStrings(scan.SelfRepositoryRefs, selfScan.SelfRepositoryRefs)
		pw.SelfRepositoryRefErrs = mergeStrings(scan.SelfRepositoryRefErrs, selfScan.SelfRepositoryRefErrs)
		pw.SelfRepositoryResolutionErrs = selfScan.Errors
		pw.Images = append(wf.ContainerImages(), selfScan.Images...)
		out = append(out, pw)
	}
	return out
}

// lowercaseSHAs spells each full-SHA ref in groups canonically, in place,
// so workflow refs match lockfile keys whatever case the source uses. It
// returns each changed `uses:` as the source spells it, keyed to its
// canonical form. The source itself is left as written.
func lowercaseSHAs(groups ...[]parserlock.ActionRef) map[string]string {
	var spellings map[string]string
	for _, group := range groups {
		for i, ref := range group {
			canon := lockfile.CanonicalRef(ref.Ref)
			if canon == ref.Ref {
				continue
			}
			if spellings == nil {
				spellings = map[string]string{}
			}
			spellings[ref.FullName()+"@"+ref.Ref] = ref.FullName() + "@" + canon
			group[i].Ref = canon
		}
	}
	return spellings
}

// rewritableRefs drops from refs every ref an in-repo `$/…` action file
// holds, matched by repository and ref. Runs never write action files, and
// the runner looks up each of those refs as written, so it keeps that
// spelling everywhere it's written and locks under it. A full SHA is
// matched by SHA alone: a renamed repository's old and new names resolve to
// one commit, and a `$/…` action is also used from other repositories,
// which run it without this lockfile, so its SHA is its only pin (#264).
func rewritableRefs(refs, selfRefs []parserlock.ActionRef) []parserlock.ActionRef {
	if len(selfRefs) == 0 {
		return refs
	}
	held := make(map[string]bool, len(selfRefs))
	for _, ref := range selfRefs {
		if parserlock.IsFullSha(ref.Ref) {
			held[strings.ToLower(ref.Ref)] = true
		} else {
			held[strings.ToLower(ref.NWO())+"@"+ref.Ref] = true
		}
	}
	// Non-nil even when empty: a nil RewriteRefs means every ref.
	out := make([]parserlock.ActionRef, 0, len(refs))
	for _, ref := range refs {
		if held[strings.ToLower(ref.Ref)] || held[strings.ToLower(ref.NWO())+"@"+ref.Ref] {
			continue
		}
		out = append(out, ref)
	}
	return out
}

func mergeActionRefs(groups ...[]parserlock.ActionRef) []parserlock.ActionRef {
	seen := make(map[string]bool)
	var refs []parserlock.ActionRef
	for _, group := range groups {
		for _, ref := range group {
			key := ref.FullName() + "@" + ref.Ref
			if seen[key] {
				continue
			}
			seen[key] = true
			refs = append(refs, ref)
		}
	}
	return refs
}

func mergeStrings(groups ...[]string) []string {
	seen := make(map[string]bool)
	var values []string
	for _, group := range groups {
		for _, value := range group {
			if seen[value] {
				continue
			}
			seen[value] = true
			values = append(values, value)
		}
	}
	return values
}

// CollectResolvable returns the deduplicated union of refs and existing deps
// across all parsed workflows. Use the returned slices to pre-warm the
// resolver caches once before per-workflow diagnostics.
func CollectResolvable(parsed []checks.ParsedWorkflow) ([]parserlock.ActionRef, []dep.Dependency) {
	return collectResolvable(parsed, nil)
}

// CollectUnrecordedResolvable is like CollectResolvable but excludes refs
// whose NWO@Ref key appears in recordedKeys. Deps whose key is in
// recordedKeys are also excluded. Use this when per-dep lockfile trust
// has already seeded the resolver cache for recorded deps, so only
// genuinely new refs need network resolution.
func CollectUnrecordedResolvable(parsed []checks.ParsedWorkflow, recordedKeys map[string]bool) ([]parserlock.ActionRef, []dep.Dependency) {
	return collectResolvable(parsed, recordedKeys)
}

func collectResolvable(parsed []checks.ParsedWorkflow, excludeKeys map[string]bool) ([]parserlock.ActionRef, []dep.Dependency) {
	seenRef := make(map[ghapi.ActionRef]bool)
	var refs []parserlock.ActionRef
	for _, pw := range parsed {
		for _, ref := range pw.Refs {
			nwoRef := strings.ToLower(ref.Owner+"/"+ref.Repo) + "@" + ref.Ref
			if excludeKeys[nwoRef] {
				continue
			}
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
			if excludeKeys[key] || seenDep[key] {
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
