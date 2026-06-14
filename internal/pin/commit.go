package pin

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/github/gh-actions-pin/internal/dep"
	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/pipeline/checks"
	"github.com/github/gh-actions-pin/internal/workflowfile"
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

	// Phase 2: Update lockfile entries for each pinned workflow.
	// Only write workflows that have at least one genuinely new pin.
	pinnedByWorkflow := groupPinnedByWorkflow(rec)
	hasNewPin := workflowsWithNewPins(rec)
	if len(pinnedByWorkflow) > 0 {
		progress("Updating lockfile")
	}
	for wfPath, deps := range pinnedByWorkflow {
		if !hasNewPin[wfPath] {
			continue // all entries verified — no write needed
		}
		wfKey := workflowfile.KeyFromPath(wfPath)
		parentMap := buildParentMap(rec, wfPath)
		directKeys := buildDirectKeys(rec, wfPath)
		deps = retainUnresolvablePins(rec, store, wfPath, deps, directKeys)
		if err := store.Set(ctx, wfKey, deps, parentMap, directKeys); err != nil {
			return fmt.Errorf("updating lockfile for %s: %w", wfPath, err)
		}
	}

	// Phase 3: Persist lockfile to disk.
	if err := store.Save(); err != nil {
		return fmt.Errorf("saving lockfile: %w", err)
	}

	return nil
}

func rewriteWorkflow(wp WorkflowPlan) error {
	if len(wp.Rewrites) == 0 {
		return nil
	}
	wf, err := workflowfile.Load(wp.Path)
	if err != nil {
		return err
	}
	content, _, err := wf.RewriteActionRefs(wp.Rewrites)
	if err != nil {
		return fmt.Errorf("applying rewrites: %w", err)
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
// entry that cannot be resolved this run (impostor-flagged or transiently
// unresolvable, e.g. 403/SSO). Without this a co-located re-pin silently
// drops the existing pin.
func retainUnresolvablePins(rec *Record, store *lockfile.State, wfPath string, deps []dep.Dependency, directKeys map[string]bool) []dep.Dependency {
	retain := make(map[string]bool)
	for _, e := range rec.Entries {
		shouldRetain := (e.Resolution == Investigate && e.Issue == string(checks.ImpostorCommit)) ||
			e.Resolution == Unresolved
		if !shouldRetain {
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

// workflowsWithNewPins returns the set of workflow paths that contain at
// least one entry with Resolution == Pinned (i.e. genuinely new or changed).
func workflowsWithNewPins(rec *Record) map[string]bool {
	m := make(map[string]bool)
	for _, e := range rec.Entries {
		if e.Resolution != Pinned {
			continue
		}
		for _, wf := range e.Workflows {
			m[wf] = true
		}
	}
	return m
}
