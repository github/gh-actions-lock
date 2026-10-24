package main

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/MakeNowJust/heredoc"
	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/refresh"
	"github.com/github/gh-actions-lock/internal/ui"
	"github.com/spf13/cobra"
)

// newUpdateCmd returns `actions-lock update`, which re-resolves selected
// lock entries to their refs' current commits. Without action selectors it
// is the old --relock.
func newUpdateCmd(newResolver resolverFunc) *cobra.Command {
	opts := &checkOptions{}
	cmd := &cobra.Command{
		Use:   "update [<owner/repo[@ref]> | <workflow-path>]...",
		Short: "Re-resolve locked actions to their refs' current commits",
		Long: heredoc.Doc(`
Re-resolve locked actions to the commits their refs point at now, and
rewrite the lockfile.

A plain gh actions-lock keeps every recorded entry as it is. Use update
to advance a floating ref (v4, main) that moved, or to lock an exact tag
whose commit changed (tag-commit-mismatch). To bump an action to another
version, edit its uses: line and run gh actions-lock instead.

Each argument is either a workflow file or an action. Workflow files
limit the run to those workflows, as with gh actions-lock. Actions
(owner/repo for every ref the lockfile records, or owner/repo@ref for one)
limit what moves: only those entries and the actions they use are
re-resolved. With no actions, every entry in the selected workflows is
re-resolved.

An action that only a composite action uses is read from the composite's
action.yml, so update the composite to move it.

Lock entries are shared: one owner/repo@ref entry serves every workflow
that uses it, so updating it moves it for all of them. The summary names
any such workflow the run didn't scan.

A pin whose commit is no longer in its ref's history (unreachable-pin)
stays an error. Check why it moved, then run gh actions-lock --accept-moved.
`),
		Example: heredoc.Doc(`
# Advance every floating ref in every workflow
$ gh actions-lock update

# Re-resolve one action, wherever it is used
$ gh actions-lock update actions/checkout@v4

# Re-resolve every ref of one action in one workflow
$ gh actions-lock update actions/setup-go .github/workflows/ci.yml
`),
		PreRunE: func(cmd *cobra.Command, args []string) error {
			if opts.verify || opts.verifyLocal {
				return errors.New("update writes the lockfile; it cannot be combined with --verify or --verify-local")
			}
			if opts.acceptMoved {
				return errors.New("--accept-moved is a separate step; run gh actions-lock --accept-moved")
			}
			paths, sels, err := splitUpdateArgs(args)
			if err != nil {
				return err
			}
			opts.workflowPaths = paths
			opts.refresh = sels
			opts.relock = len(sels) == 0
			return opts.validateOutputFlags()
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCheck(cmd, opts, newResolver)
		},
	}
	bindCheckFlags(cmd, opts)
	for _, name := range []string{"relock", "accept-moved", "verify", "verify-local", "allow-runners", "allow-all-runners"} {
		_ = cmd.Flags().MarkHidden(name)
	}
	return cmd
}

// splitUpdateArgs sorts update's arguments into workflow files, which exist
// on disk, and owner/repo[@ref] action selectors.
func splitUpdateArgs(args []string) (paths []string, sels []refresh.Selector, err error) {
	for _, a := range args {
		if fi, statErr := os.Stat(a); statErr == nil && !fi.IsDir() {
			paths = append(paths, a)
			continue
		}
		sel, err := refresh.ParseSelector(a)
		if err != nil {
			return nil, nil, err
		}
		sels = append(sels, sel)
	}
	return paths, sels, nil
}

// sharedEntries snapshots, for a run scoped to some workflows, the commit
// of each entry that a workflow outside the scope also records, so the
// summary can name the workflows a moved entry moved for too.
type sharedEntries struct {
	commit    map[string]string
	unscanned map[string][]string
}

func snapshotSharedEntries(store *lockfile.State, scope []string) sharedEntries {
	s := sharedEntries{commit: map[string]string{}, unscanned: map[string][]string{}}
	all := func(parserlock.Pin, parserlock.Action) bool { return true }
	for _, wk := range store.WorkflowKeys() {
		if slices.Contains(scope, wk) {
			continue
		}
		for _, lp := range store.Closure(wk, all) {
			k := lockfile.EntryKey(lp.Pin)
			if !slices.Contains(s.unscanned[k], wk) {
				s.unscanned[k] = append(s.unscanned[k], wk)
			}
			s.commit[k] = lp.Action.Commit
		}
	}
	return s
}

// renderSharedMoves names the workflows outside the run's scope whose lock
// entries the run moved.
func renderSharedMoves(console *ui.UI, store *lockfile.State, s sharedEntries) {
	var lines []string
	for _, k := range slices.Sorted(maps.Keys(s.unscanned)) {
		a, ok := store.Entry(k)
		if !ok || strings.EqualFold(a.Commit, s.commit[k]) {
			continue
		}
		_, sha, _ := strings.Cut(a.Commit, "-")
		sha = cmp.Or(sha, a.Commit)
		lines = append(lines, fmt.Sprintf("  %s (%s) also moved for %s", k, sha[:min(7, len(sha))], strings.Join(s.unscanned[k], ", ")))
	}
	if len(lines) == 0 {
		return
	}
	console.TermBlank()
	console.TermCaution("Moved %d shared %s that workflows outside this run also use",
		len(lines), ui.Pluralize(len(lines), "entry", "entries"))
	for _, l := range lines {
		console.TermDetail("%s", l)
	}
}
