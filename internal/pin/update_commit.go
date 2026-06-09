package pin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/workflowfile"
)

// CommitUpdate applies a plan to disk, ordering writes so the lockfile can
// never advance ahead of the workflow YAML. It stages every rewritten
// workflow's new bytes to a sibling temp file, saves the lockfile (itself an
// atomic temp+rename), then promotes the staged files with rename. No workflow
// file is mutated until the lockfile is durably written, so a failure while
// staging or saving leaves the working tree exactly as it was.
//
// The remaining non-atomic window is phase 3: if a promotion rename fails after
// the lockfile has saved, some workflows may be left unpromoted (lockfile ahead
// of YAML). A same-directory rename only fails under catastrophic conditions,
// so this window is far smaller than the old write-in-place ordering, but it is
// not zero — true N-file atomicity would need a journal.
//
// Each promotion preserves the destination's existing mode and follows a
// symlinked workflow to its target, so a 0600 or symlinked workflow keeps its
// identity instead of being replaced by a fresh 0644 regular file.
//
// It returns the sorted set of workflow file paths whose YAML was actually
// rewritten (using the caller's original paths, not symlink-resolved ones). A
// lockfile-only edit (same ref, freshly-moved SHA) changes no uses: line and is
// intentionally omitted, so workflows[] names only files that changed on disk.
func CommitUpdate(ctx context.Context, store *lockfile.State, plan *UpdatePlan) ([]string, error) {
	if len(plan.edits) == 0 {
		return nil, nil
	}

	type staged struct {
		reported string // original edit path, surfaced in workflows[]
		dst      string // promotion target (symlink-resolved)
		tmp      string
	}
	var stagedWrites []staged
	cleanup := func() {
		for _, s := range stagedWrites {
			_ = os.Remove(s.tmp)
		}
	}

	// Phase 1: render and stage every workflow rewrite. A planned rewrite that
	// matches no uses: line is a workflow/lockfile desync — abort before any
	// disk mutation.
	for _, e := range plan.edits {
		if len(e.rewrites) == 0 {
			continue // lockfile-only change (same ref, moved SHA): no YAML edit
		}
		wf, err := workflowfile.Load(e.path)
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("loading %s: %w", e.path, err)
		}
		content, changed, err := wf.RewriteActionRefs(e.rewrites)
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("rewriting %s: %w", e.path, err)
		}
		if changed == 0 {
			cleanup()
			return nil, fmt.Errorf("rewriting %s: no uses: line matched %v (workflow/lockfile desync)", e.path, e.rewrites)
		}

		// Promote onto the real file (following a symlink) and preserve its
		// mode so we don't widen perms or clobber a symlink.
		dst := e.path
		if info, err := os.Lstat(e.path); err == nil && info.Mode()&os.ModeSymlink != 0 {
			if resolved, err := filepath.EvalSymlinks(e.path); err == nil {
				dst = resolved
			}
		}
		mode := os.FileMode(0o644)
		if info, err := os.Stat(dst); err == nil {
			mode = info.Mode().Perm()
		}
		tmp := dst + ".gh-actions-pin.tmp"
		if err := os.WriteFile(tmp, content, mode); err != nil {
			cleanup()
			return nil, fmt.Errorf("staging %s: %w", e.path, err)
		}
		stagedWrites = append(stagedWrites, staged{reported: e.path, dst: dst, tmp: tmp})
	}

	// Phase 2: update and save the lockfile. store.Save is atomic (temp+rename),
	// so a failure here leaves the lockfile and every workflow file untouched;
	// the staged temps are discarded.
	for _, e := range plan.edits {
		if err := store.Set(ctx, e.wfKey, e.closure, e.parentMap, e.directKeys); err != nil {
			cleanup()
			return nil, fmt.Errorf("updating lockfile for %s: %w", e.path, err)
		}
	}
	if err := store.Save(); err != nil {
		cleanup()
		return nil, fmt.Errorf("saving lockfile: %w", err)
	}

	// Phase 3: promote staged YAML. The lockfile is already durable; each
	// same-directory rename is the smallest possible mutation window.
	saved := make([]string, 0, len(stagedWrites))
	for _, s := range stagedWrites {
		if err := os.Rename(s.tmp, s.dst); err != nil {
			return nil, fmt.Errorf("promoting %s: %w", s.reported, err)
		}
		saved = append(saved, s.reported)
	}
	sort.Strings(saved)
	return saved, nil
}
