package pin

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/workflowfile"
	"golang.org/x/sync/errgroup"
)

// CommitOptions configures the Commit pass.
type CommitOptions struct {
	// OnProgress is called at each phase boundary. Nil means no progress.
	OnProgress func(phase string)
	// SkipNewWorkflowEntries forces the Commit phase to skip workflows with no existing lockfile entry.
	SkipNewWorkflowEntries bool
}

// Commit writes a planned Record to disk: rewrites workflow files and
// updates the lockfile store. All-or-nothing semantics — if any write
// fails, previously written files are not rolled back (best-effort),
// but the error is returned immediately.
func Commit(ctx context.Context, rec *Record, store *lockfile.State, copts *CommitOptions) error {
	progress := func(string) {}
	if copts != nil && copts.OnProgress != nil {
		progress = copts.OnProgress
	}
	rec.Workflows = committedWorkflows(rec, store, copts)
	if err := validateRequiredRewrites(rec.Workflows); err != nil {
		return err
	}

	// Phase 1: Update lockfile entries in memory. It runs before any file is
	// rewritten, so an entry the lockfile refuses leaves every file as it was.
	if len(rec.Workflows) > 0 {
		progress("Updating lockfile")
	}
	if err := updateLockfile(ctx, rec, rec.Workflows, store); err != nil {
		return err
	}

	// Phase 2: Rewrite workflow files (uses: line changes).
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

	// Phase 3: Persist lockfile to disk.
	if err := store.Save(); err != nil {
		return fmt.Errorf("saving lockfile: %w", err)
	}

	return nil
}

// committedWorkflows are the planned workflows Commit writes.
func committedWorkflows(rec *Record, store *lockfile.State, copts *CommitOptions) []WorkflowPlan {
	if copts == nil || !copts.SkipNewWorkflowEntries {
		return rec.Workflows
	}
	var workflows []WorkflowPlan
	for _, wp := range rec.Workflows {
		if store.HasWorkflow(workflowfile.KeyFromPath(wp.Path)) {
			workflows = append(workflows, wp)
		}
	}
	return workflows
}

// updateLockfile applies rec's entry repairs for the workflows it writes,
// then sets each of those workflows' entries in store.
func updateLockfile(ctx context.Context, rec *Record, workflows []WorkflowPlan, store *lockfile.State) error {
	written := make(map[string]bool, len(workflows))
	for _, wp := range workflows {
		written[wp.Path] = true
	}
	for _, r := range rec.EntryRepairs {
		if !slices.ContainsFunc(r.Workflows, func(p string) bool { return written[p] }) {
			continue
		}
		if err := store.SetEntryRef(r.Key, r.Ref); err != nil {
			return fmt.Errorf("updating lockfile: %w", err)
		}
	}
	writtenKeys := make(map[string]bool, len(workflows))
	for _, wp := range workflows {
		writtenKeys[workflowfile.KeyFromPath(wp.Path)] = true
	}
	var unwritten []string
	for _, wf := range store.WorkflowKeys() {
		if !writtenKeys[wf] {
			unwritten = append(unwritten, wf)
		}
	}
	store.KeepEdges(unwritten)
	pinnedByWorkflow := groupPinnedByWorkflow(rec)
	for _, wp := range workflows {
		wfPath := wp.Path
		wfKey := workflowfile.KeyFromPath(wfPath)
		deps := pinnedByWorkflow[wfPath]
		parentMap := buildParentMap(rec, wfPath)
		directKeys := buildDirectKeys(rec, wfPath)
		deps = retainUnresolvablePins(rec, store, wfPath, deps, directKeys)
		if err := store.Set(ctx, wfKey, deps, parentMap, directKeys); err != nil {
			return fmt.Errorf("updating lockfile for %s: %w", wfPath, err)
		}
	}
	// A written workflow's closure can reach entries it shares with
	// workflows this run doesn't write, so check each one whole.
	f := store.File()
	all := func(parserlock.Pin, parserlock.Action) bool { return true }
	for _, wp := range workflows {
		if faults := lockfile.GraphFaults(f, workflowfile.KeyFromPath(wp.Path), all); len(faults) > 0 {
			return fmt.Errorf("refusing to write %s: its lock graph is incomplete: %s; run `gh actions-lock --relock` to rebuild it", wp.Path, faults[0])
		}
		if cycle := lockfile.UsesCycle(f, workflowfile.KeyFromPath(wp.Path)); cycle != nil {
			return fmt.Errorf("refusing to write %s: %s; the lockfile can't record actions that use each other, so pin one of them to a different commit", wp.Path, describeCycle(cycle))
		}
	}
	return nil
}

// describeCycle spells a cycle as "a uses b, which uses c, which uses a".
func describeCycle(cycle []string) string {
	var b strings.Builder
	b.WriteString(cycle[0])
	for i, key := range cycle[1:] {
		if i == 0 {
			b.WriteString(" uses ")
		} else {
			b.WriteString(", which uses ")
		}
		b.WriteString(key)
	}
	return b.String()
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

	if bytes.Equal(content, wf.Content) {
		return nil
	}
	return os.WriteFile(wp.Path, content, 0o644)
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
				Hostname:    e.Hostname,
				NWO:         e.NWO,
				Ref:         e.Ref,
				SHA:         e.SHA,
				OriginalRef: e.AutoFixedRef,
				RenamedFrom: e.RenamedFrom,
				Branch:      e.OnBranch,
				Tag:         e.Tag,
				FromLock:    e.FromLock,
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

func validateRequiredRewrites(plans []WorkflowPlan) error {
	for _, wp := range plans {
		if len(wp.RequiredRewrites) == 0 {
			continue
		}
		wf, err := workflowfile.Load(wp.Path)
		if err != nil {
			return fmt.Errorf("validating required rewrites in %s: %w", wp.Path, err)
		}
		found, err := wf.ValidateRequiredActionRefRewrites(wp.RequiredRewrites)
		if err != nil {
			return fmt.Errorf("validating required rewrites in %s: %w", wp.Path, err)
		}
		for oldUse := range wp.RequiredRewrites {
			if found[oldUse] == 0 {
				return fmt.Errorf("required action rewrite for %s was not found in writable workflow sources", oldUse)
			}
		}
	}
	return nil
}
