package main

import (
	"context"
	"fmt"
	"io"
	"sort"

	"github.com/MakeNowJust/heredoc"
	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-pin/cmd/gh-actions-pin/format"
	"github.com/github/gh-actions-pin/internal/pin"
	"github.com/github/gh-actions-pin/internal/pinpool"
	"github.com/github/gh-actions-pin/internal/pipeline"
	"github.com/github/gh-actions-pin/internal/pipeline/checks"
	"github.com/github/gh-actions-pin/internal/ui"
	"github.com/spf13/cobra"
)

type updateOptions struct {
	action        string
	workflowPaths []string
	jsonFields    string
	hostname      string
	write         bool
}

func newUpdateCmd(newResolver resolverFunc) *cobra.Command {
	opts := &updateOptions{}

	cmd := &cobra.Command{
		Use:   "update --action <owner>/<repo>@<ref> [<workflow-path>...]",
		Args:  cobra.ArbitraryArgs,
		Short: "Relock a single action to a new ref",
		Long: heredoc.Doc(`
			Relock one action dependency to a new ref across the workflows that
			already use it, updating both the workflow uses: line and the
			lockfile pin (the immutable SHA the new ref resolves to).

			Resolves --action's ref to its newest immutable SHA and rewrites
			every matching workflow that already has a lockfile entry. The
			human-readable ref you pass is preserved verbatim (v4 → v6, never
			narrowed to v6.1.2). Untargeted dependencies are left untouched.

			Onboarding is refused: a workflow that uses the action but has no
			existing lockfile entry is skipped with an onboarding-required
			finding. Run 'gh actions-pin check' to onboard it first.

			--write applies the changes to disk; without it the diff is computed
			but nothing is written. All three result arrays (updated, workflows,
			findings) are always present in --json output; the selector is
			accepted for symmetry with check but does not gate any field.

			Exit status:
			  0  relock succeeded (or was a no-op) with no blocking findings.
			  1  blocking findings remain (e.g. onboarding-required). With
			     --json, stdout still carries well-formed JSON; without it,
			     findings are printed to stderr.
			  2  the tool itself failed (bad --action, IO error, resolve or
			     auth failure).
		`),
		Example: heredoc.Doc(`
			# Bump actions/checkout to v6 and write the changes
			$ gh actions-pin update --action actions/checkout@v6 --write --json=updated

			# Dry-run: compute the diff without touching disk
			$ gh actions-pin update --action actions/checkout@v6 --json=updated
		`),
		PreRunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				opts.workflowPaths = args
			}
			if opts.action == "" {
				return fmt.Errorf("--action is required (expected <owner>/<repo>@<ref>)")
			}
			return format.ValidateUpdateJSONFields(opts.jsonFields)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUpdate(cmd, opts, newResolver)
		},
	}

	cmd.Flags().StringVar(&opts.action, "action", "", "Action to relock as `<owner>/<repo>@<ref>` (required)")
	cmd.Flags().StringVar(&opts.jsonFields, "json", "", "Output JSON (all of updated,findings,workflows always emitted; `fields` accepted for symmetry with check but does not filter)")
	cmd.Flags().Lookup("json").NoOptDefVal = "updated"
	cmd.Flags().StringVar(&opts.hostname, "hostname", "", "GitHub hostname to query (defaults to GH_HOST, current repo host, or github.com)")
	cmd.Flags().BoolVar(&opts.write, "write", false, "Apply changes to workflows and the lockfile (omit for a dry run)")

	return cmd
}

func runUpdate(cmd *cobra.Command, opts *updateOptions, newResolver resolverFunc) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()
	console := ui.NewWithWriter(errOut)
	defer console.StopProgress()

	ar := parserlock.ParseActionRef(opts.action)
	if ar == nil {
		return fmt.Errorf("invalid --action %q: expected <owner>/<repo>@<ref>", opts.action)
	}
	targetNWO := ar.NWO()
	targetRef := ar.Ref

	pool := pinpool.New(0, console)
	paths, r, store, err := newRun(opts.workflowPaths, opts.hostname, pool, newResolver)
	if err != nil {
		return err
	}
	r.SeedFromLockfile(store.AllDeps())

	if opts.jsonFields == "" {
		console.SetLog(io.Discard)
	}

	plan, err := pin.PlanUpdate(ctx, pin.UpdateOptions{
		Resolver:      r,
		Store:         store,
		TargetNWO:     targetNWO,
		TargetRef:     targetRef,
		WorkflowPaths: paths,
	})
	if err != nil {
		console.StopProgress()
		return err
	}

	var saved []string
	if opts.write {
		saved, err = pin.CommitUpdate(ctx, store, plan)
		if err != nil {
			console.StopProgress()
			return err
		}
	}
	console.StopProgress()

	res := buildUpdateResult(plan, saved)

	if opts.jsonFields != "" {
		if err := format.WriteUpdateJSON(out, res, cliVersion(), store.File().Version); err != nil {
			return err
		}
	} else {
		for _, w := range plan.Warnings {
			console.TermDetail("warning: %s", w)
		}
		format.PresentUpdateSummary(console, res, opts.write)
	}

	if !res.Valid {
		return errSilent
	}
	return nil
}

// buildUpdateResult assembles the JSON-facing result from the engine plan and
// the set of saved workflow paths, attaching doc URLs and computing validity
// (blocking = any error-severity, non-inconclusive finding).
func buildUpdateResult(plan *pin.UpdatePlan, saved []string) format.UpdateResult {
	res := format.UpdateResult{Valid: true}
	for _, ch := range plan.Changes {
		res.Updated = append(res.Updated, format.UpdatedAction{
			NWO:    ch.NWO,
			OldRef: ch.OldRef,
			NewRef: ch.NewRef,
			OldSHA: ch.OldSHA,
			NewSHA: ch.NewSHA,
		})
	}
	for _, p := range saved {
		res.Workflows = append(res.Workflows, format.UpdatedWorkflow{Path: p})
	}
	for _, f := range plan.Findings {
		if f.DocURL == "" {
			f.DocURL = pipeline.DocURLFor(f.Category)
		}
		res.Findings = append(res.Findings, format.FindingJSON(f))
		if f.Severity == checks.SeverityError && !f.Category.IsInconclusive() {
			res.Valid = false
		}
	}
	// findings[] is an always-on diagnostic array; give it an explicit total
	// order so the JSON is byte-deterministic regardless of plan traversal.
	sort.Slice(res.Findings, func(i, j int) bool {
		a, b := res.Findings[i], res.Findings[j]
		if a.Workflow != b.Workflow {
			return a.Workflow < b.Workflow
		}
		if a.Category != b.Category {
			return a.Category < b.Category
		}
		return a.Detail < b.Detail
	})
	return res
}
