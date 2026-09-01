package pin

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"

	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/workflowfile"
	"golang.org/x/sync/errgroup"
)

// CommitOptions configures the Commit pass.
type CommitOptions struct {
	// OnProgress is called at each phase boundary. Nil means no progress.
	OnProgress func(phase string)
}

// Commit writes a planned Record to disk: rewrites workflow files and
// updates the lockfile store. All-or-nothing semantics — if any write
// fails, previously written files are not rolled back (best-effort),
// but the error is returned immediately.
func Commit(ctx context.Context, rec *Record, store *lockfile.State, copts *CommitOptions) error {
	reconciled, err := reconcileSharedPins(rec, store)
	if err != nil {
		return err
	}
	rec = reconciled

	progress := func(string) {}
	if copts != nil && copts.OnProgress != nil {
		progress = copts.OnProgress
	}

	// Phase 1: Rewrite workflow files (uses: line changes).
	if len(rec.Workflows) > 0 {
		progress("Rewriting workflows")
	}

	// Rewrite workflow files in parallel. Uses errgroup rather than pinpool
	// because this is fast local I/O — no progress reporting or API rate
	// limiting needed. The pool is for network-bound work.
	g, _ := errgroup.WithContext(ctx)
	g.SetLimit(runtime.GOMAXPROCS(0))
	for _, wp := range rec.Workflows {
		g.Go(func() error {
			if err := rewriteWorkflow(wp); err != nil {
				return fmt.Errorf("rewriting %s: %w", wp.Path, err)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}

	// Action files can be shared by several workflows, so merge their
	// rewrites and write each file once, serially.
	if err := rewriteSelfActionFiles(rec.Workflows); err != nil {
		return err
	}

	// Phase 2: Update lockfile entries for each scanned workflow.
	pinnedByWorkflow := groupPinnedByWorkflow(rec)
	scopedWorkflows := make(map[string]bool, len(rec.Workflows))
	for _, wp := range rec.Workflows {
		scopedWorkflows[workflowfile.KeyFromPath(wp.Path)] = true
	}
	if len(rec.Workflows) > 0 {
		progress("Updating lockfile")
	}
	for _, wp := range rec.Workflows {
		wfPath := wp.Path
		wfKey := workflowfile.KeyFromPath(wfPath)
		deps := pinnedByWorkflow[wfPath]
		if len(deps) == 0 && !store.HasWorkflow(wfKey) {
			continue
		}
		parentMap := buildParentMap(rec, wfPath)
		directKeys := buildDirectKeys(rec, wfPath)
		deps = retainUnresolvablePins(rec, store, wfPath, deps, directKeys)
		if err := store.SetScoped(ctx, wfKey, deps, parentMap, directKeys, scopedWorkflows); err != nil {
			return fmt.Errorf("updating lockfile for %s: %w", wfPath, err)
		}
	}

	// Phase 3: Persist lockfile to disk.
	if err := store.Save(); err != nil {
		return fmt.Errorf("saving lockfile: %w", err)
	}

	return nil
}

// reconcileSharedPins retains recorded authority when scoped workflow plans
// disagree about the commit or closure of a global lockfile dependency.
func reconcileSharedPins(rec *Record, store *lockfile.State) (*Record, error) {
	shasByKey := make(map[string]map[string]bool)
	for _, e := range rec.Entries {
		if !isPinOrVerified(e.Resolution) {
			continue
		}
		key := strings.ToLower(e.NWO + "@" + e.Ref)
		if shasByKey[key] == nil {
			shasByKey[key] = make(map[string]bool)
		}
		shasByKey[key][strings.ToLower(e.SHA)] = true
	}

	conflicts := make(map[string]dep.Dependency)
	for key, shas := range shasByKey {
		if len(shas) < 2 {
			continue
		}
		for _, existing := range store.AllDeps() {
			if strings.EqualFold(existing.Key(), key) {
				conflicts[key] = existing
				break
			}
		}
		if _, ok := conflicts[key]; !ok {
			return nil, fmt.Errorf("conflicting planned commits for %s", key)
		}
	}
	if len(conflicts) == 0 {
		return rec, nil
	}

	reconciled := *rec
	reconciled.Entries = slices.Clone(rec.Entries)
	for i := range reconciled.Entries {
		e := &reconciled.Entries[i]
		key := strings.ToLower(e.NWO + "@" + e.Ref)
		if existing, ok := conflicts[key]; ok && isPinOrVerified(e.Resolution) {
			e.SHA = existing.SHA
			e.OnBranch = existing.Branch
			e.Tag = existing.Tag
		}
		e.RequiredBy = slices.DeleteFunc(slices.Clone(e.RequiredBy), func(parent string) bool {
			_, conflicted := conflicts[strings.ToLower(parent)]
			return conflicted
		})
	}
	return &reconciled, nil
}

func rewriteWorkflow(wp WorkflowPlan) error {
	wf, err := workflowfile.Load(wp.Path)
	if err != nil {
		return err
	}

	content := wf.Content
	if len(wp.Rewrites) > 0 {
		content, _, err = wf.RewriteActionRefs(wp.Rewrites)
		if err != nil {
			return fmt.Errorf("applying rewrites: %w", err)
		}
	}

	content = workflowfile.EnsureSentinel(content)
	if bytes.Equal(content, wf.Content) {
		return nil
	}
	return os.WriteFile(wp.Path, content, 0o644)
}

// rewriteSelfActionFiles applies each workflow's rewrites to the in-repo
// action files it reaches via `$/…`. No sentinel comment: these are action
// definitions, not managed workflows.
func rewriteSelfActionFiles(plans []WorkflowPlan) error {
	merged := make(map[string]map[string]string)
	for _, wp := range plans {
		if len(wp.Rewrites) == 0 {
			continue
		}
		for _, path := range wp.SelfActionFiles {
			if merged[path] == nil {
				merged[path] = make(map[string]string)
			}
			for oldUses, newUses := range wp.Rewrites {
				merged[path][oldUses] = newUses
			}
		}
	}
	for path, rewrites := range merged {
		wf, err := workflowfile.Load(path)
		if err != nil {
			return fmt.Errorf("loading %s: %w", path, err)
		}
		content, changed, err := wf.RewriteActionRefs(rewrites)
		if err != nil {
			return fmt.Errorf("rewriting %s: %w", path, err)
		}
		if changed == 0 || bytes.Equal(content, wf.Content) {
			continue
		}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", path, err)
		}
	}
	return nil
}

func isPinOrVerified(r Resolution) bool {
	return r == Pinned || r == Verified
}

func groupPinnedByWorkflow(rec *Record) map[string][]dep.Dependency {
	result := make(map[string][]dep.Dependency)
	for _, e := range rec.Entries {
		if !isPinOrVerified(e.Resolution) {
			continue
		}
		for _, wf := range e.Workflows {
			result[wf] = append(result[wf], dep.Dependency{
				NWO:    e.NWO,
				Ref:    e.Ref,
				SHA:    e.SHA,
				Branch: e.OnBranch,
				Tag:    e.Tag,
			})
		}
	}
	return result
}

// retainUnresolvablePins re-adds the workflow's existing on-disk pins for any
// entry that cannot be resolved this run (flagged for investigation or transiently
// unresolvable, e.g. 403/SSO). Without this a co-located re-pin silently
// drops the existing pin.
func retainUnresolvablePins(rec *Record, store *lockfile.State, wfPath string, deps []dep.Dependency, directKeys map[string]bool) []dep.Dependency {
	retain := make(map[string]bool)
	for _, e := range rec.Entries {
		if e.Resolution != Unresolved && e.Resolution != Investigate {
			continue
		}
		for _, wf := range e.Workflows {
			if wf == wfPath {
				retain[strings.ToLower(e.NWO+"@"+e.Ref)] = true
			}
		}
	}
	if len(retain) == 0 {
		return deps
	}
	existing, err := store.Get(workflowfile.KeyFromPath(wfPath))
	if err != nil {
		return deps
	}
	have := make(map[string]bool, len(deps))
	for _, d := range deps {
		have[strings.ToLower(d.NWO+"@"+d.Ref)] = true
	}
	for _, d := range existing {
		k := strings.ToLower(d.NWO + "@" + d.Ref)
		if retain[k] && !have[k] {
			deps = append(deps, d)
			directKeys[d.Key()] = true
			have[k] = true
		}
	}
	return deps
}

func buildParentMap(rec *Record, wfPath string) map[string][]string {
	pm := make(map[string][]string)
	for _, e := range rec.Entries {
		if !isPinOrVerified(e.Resolution) {
			continue
		}
		for _, wf := range e.Workflows {
			if wf != wfPath {
				continue
			}
			if len(e.RequiredBy) > 0 {
				key := e.NWO + "@" + e.Ref
				pm[key] = e.RequiredBy
			}
		}
	}
	return pm
}

func buildDirectKeys(rec *Record, wfPath string) map[string]bool {
	keys := make(map[string]bool)
	for _, e := range rec.Entries {
		if !isPinOrVerified(e.Resolution) || !e.Direct {
			continue
		}
		for _, wf := range e.Workflows {
			if wf == wfPath {
				keys[e.NWO+"@"+e.Ref] = true
			}
		}
	}
	return keys
}
