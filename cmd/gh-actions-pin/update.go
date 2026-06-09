package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/MakeNowJust/heredoc"
	"github.com/cli/go-gh/v2/pkg/prompter"
	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-pin/cmd/gh-actions-pin/format"
	"github.com/github/gh-actions-pin/internal/discover"
	"github.com/github/gh-actions-pin/internal/pin"
	"github.com/github/gh-actions-pin/internal/pinpool"
	"github.com/github/gh-actions-pin/internal/pipeline"
	"github.com/github/gh-actions-pin/internal/pipeline/checks"
	"github.com/github/gh-actions-pin/internal/ui"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

type updateOptions struct {
	action        string
	target        string
	workflowPaths []string
	jsonFields    string
	hostname      string
}

// versionPrompter selects one option from a list. Satisfied by
// *prompter.Prompter; an interface so tests inject a fake without a TTY.
type versionPrompter interface {
	Select(prompt, defaultValue string, options []string) (int, error)
}

// promptFactory returns an interactive prompter and whether the session can
// prompt (stdin and stderr are both terminals). Injected for tests; the prod
// default binds to the real terminal and renders to stderr so stdout stays
// reserved for JSON.
type promptFactory func() (versionPrompter, bool)

func defaultPromptFactory() (versionPrompter, bool) {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stderr.Fd())) {
		return nil, false
	}
	// Render the picker to stderr (not stdout) so `update --json | jq` stays clean.
	return prompter.New(os.Stdin, os.Stderr, os.Stderr), true
}

func newUpdateCmd(newResolver resolverFunc, newPrompt promptFactory) *cobra.Command {
	opts := &updateOptions{}
	if newPrompt == nil {
		newPrompt = defaultPromptFactory
	}

	cmd := &cobra.Command{
		Use:   "update --action <owner>/<repo>@<current-ref> [--target <ref>] [<workflow-path>...]",
		Args:  cobra.ArbitraryArgs,
		Short: "Relock a single action to a new ref",
		Long: heredoc.Doc(`
Relock one action dependency across the workflows that already use
it, updating both the workflow uses: line and the lockfile pin (the
immutable SHA the new ref resolves to).

--action names the dependency and its CURRENT pinned ref
(owner/repo@ref); the ref anchors version selection and is matched
against the lockfile. --target is the destination ref to move to. The
human-readable ref you pass is preserved verbatim in the workflow
(v4 → v6, never narrowed to v6.1.2); untargeted dependencies are
left untouched.

When --target is omitted and the session is interactive, update
lists the newer versions available for the action and lets you pick
one. In a non-interactive session (--no-interactive, or stdin/stderr
not a terminal) --target is required.

update always writes: a successful relock mutates the workflow files
and the lockfile in place. Onboarding is refused — a workflow that
uses the action but has no lockfile entry is skipped with an
onboarding-required finding (run 'gh actions-pin check' to onboard
it first).

All three result arrays (updated, workflows, findings) are always
present in --json output; the selector is accepted for symmetry with
check but does not gate any field.

Exit status:
  0  relock succeeded (or was a no-op) with no blocking findings.
  1  blocking findings remain (e.g. onboarding-required). With
     --json, stdout still carries well-formed JSON; without it,
     findings are printed to stderr.
  2  the tool itself failed (bad flags, IO error, resolve or auth
     failure, or --target missing in a non-interactive session).
`),
		Example: heredoc.Doc(`
# Bump actions/checkout from v4 to v6
$ gh actions-pin update --action actions/checkout@v4 --target v6

# Pick a newer version interactively (no --target)
$ gh actions-pin update --action actions/checkout@v4

# Emit JSON for tooling (relock still applied)
$ gh actions-pin update --action actions/checkout@v4 --target v6 --json=updated
`),
		PreRunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				opts.workflowPaths = args
			}
			if opts.action == "" {
				return fmt.Errorf("--action is required (expected <owner>/<repo>@<current-ref>)")
			}
			return format.ValidateUpdateJSONFields(opts.jsonFields)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUpdate(cmd, opts, newResolver, newPrompt)
		},
	}

	cmd.Flags().StringVar(&opts.action, "action", "", "Action and its current ref as `<owner>/<repo>@<ref>` (required)")
	cmd.Flags().StringVar(&opts.target, "target", "", "Destination `ref` to relock to (omit to pick interactively)")
	cmd.Flags().StringVar(&opts.jsonFields, "json", "", "Output JSON (all of updated,findings,workflows always emitted; `fields` accepted for symmetry with check but does not filter)")
	cmd.Flags().Lookup("json").NoOptDefVal = "updated"
	cmd.Flags().StringVar(&opts.hostname, "hostname", "", "GitHub hostname to query (defaults to GH_HOST, current repo host, or github.com)")

	return cmd
}

func runUpdate(cmd *cobra.Command, opts *updateOptions, newResolver resolverFunc, newPrompt promptFactory) error {
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
		return fmt.Errorf("invalid --action %q: expected <owner>/<repo>@<current-ref>", opts.action)
	}
	targetNWO := ar.NWO()
	currentRef := ar.Ref

	pool := pinpool.New(0, console)
	paths, r, store, err := newRun(opts.workflowPaths, opts.hostname, pool, newResolver)
	if err != nil {
		return err
	}
	r.SeedFromLockfile(store.AllDeps())

	if opts.jsonFields == "" {
		console.SetLog(io.Discard)
	}

	// Resolve the destination ref: explicit --target, or an interactive pick
	// anchored on the current ref. A non-interactive session with no --target
	// is a usage error (exit 2), never a hang or silent auto-pick.
	targetRef := opts.target
	if targetRef == "" {
		picked, ok, err := pickTargetRef(ctx, cmd, console, opts, newPrompt, targetNWO, currentRef, r.GHClient())
		if err != nil {
			console.StopProgress()
			return err
		}
		if !ok {
			// No newer version to offer; nothing to do. Emit an empty,
			// well-formed result so JSON consumers still parse cleanly.
			console.StopProgress()
			res := format.UpdateResult{Valid: true}
			return emitUpdateResult(out, console, opts, store.File().Version, res)
		}
		targetRef = picked
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

	saved, err := pin.CommitUpdate(ctx, store, plan)
	if err != nil {
		console.StopProgress()
		return err
	}
	console.StopProgress()

	res := buildUpdateResult(plan, saved)
	if opts.jsonFields == "" {
		for _, w := range plan.Warnings {
			console.TermDetail("warning: %s", w)
		}
	}
	return emitUpdateResult(out, console, opts, store.File().Version, res)
}

// pickTargetRef drives interactive version selection. It returns (ref, true)
// when the user picks a version, (\"\", false) when there is nothing newer to
// offer, and an error (exit 2) when the session can't prompt or discovery
// fails.
func pickTargetRef(ctx context.Context, cmd *cobra.Command, console *ui.UI, opts *updateOptions, newPrompt promptFactory, nwo, currentRef string, lister discover.TagLister) (string, bool, error) {
	noInteractive, _ := cmd.Flags().GetBool("no-interactive")
	var p versionPrompter
	if !noInteractive {
		p, _ = newPrompt()
	}
	if p == nil {
		return "", false, fmt.Errorf("--target is required: pass the destination ref, or run in an interactive terminal to pick one")
	}

	cands, err := discover.Candidates(ctx, nwo, currentRef, lister)
	if err != nil {
		return "", false, err
	}
	console.StopProgress()
	if len(cands) == 0 {
		console.TermNeutral("No newer versions available for %s (current %s).", nwo, currentRef)
		return "", false, nil
	}

	options := make([]string, len(cands))
	for i, c := range cands {
		if c.SHA != "" {
			options[i] = fmt.Sprintf("%s (%s)", c.Ref, shortSHA(c.SHA))
		} else {
			options[i] = c.Ref
		}
	}

	// Seed the default selection with the ref a dependabot PR would land
	// (core's precision-preserving pick) so hitting Enter matches eventual-PR
	// behavior; the full list is still offered for a deliberate manual choice.
	def := options[0]
	if pick, ok := discover.CorePick(currentRef, cands); ok {
		for i, c := range cands {
			if c.Ref == pick.Ref {
				def = options[i]
				break
			}
		}
	}
	idx, err := p.Select(fmt.Sprintf("Select a version for %s (current %s)", nwo, currentRef), def, options)
	if err != nil {
		return "", false, fmt.Errorf("selecting a version: %w", err)
	}
	return cands[idx].Ref, true, nil
}

// emitUpdateResult writes the result as JSON (stdout) or a human summary
// (stderr), then maps blocking findings to the errSilent (exit 1) sentinel.
func emitUpdateResult(out io.Writer, console *ui.UI, opts *updateOptions, lockfileVersion string, res format.UpdateResult) error {
	if opts.jsonFields != "" {
		if err := format.WriteUpdateJSON(out, res, cliVersion(), lockfileVersion); err != nil {
			return err
		}
	} else {
		format.PresentUpdateSummary(console, res)
	}
	if !res.Valid {
		return errSilent
	}
	return nil
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
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
