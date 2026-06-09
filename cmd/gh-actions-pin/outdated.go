package main

import (
	"context"
	"io"

	"github.com/MakeNowJust/heredoc"
	"github.com/github/gh-actions-pin/cmd/gh-actions-pin/format"
	"github.com/github/gh-actions-pin/internal/discover"
	"github.com/github/gh-actions-pin/internal/pinpool"
	"github.com/github/gh-actions-pin/internal/ui"
	"github.com/spf13/cobra"
)

type outdatedOptions struct {
	jsonFields string
	hostname   string
}

func newOutdatedCmd(newResolver resolverFunc) *cobra.Command {
	opts := &outdatedOptions{}

	cmd := &cobra.Command{
		Use:   "outdated",
		Args:  cobra.NoArgs,
		Short: "Report newer refs available for pinned actions",
		Long: heredoc.Doc(`
			Read-only version discovery. For each action recorded in the
			lockfile, report the newest ref it could upgrade to, preserving the
			precision of the current ref: a major float (v5) is offered the
			highest major-form tag (v6), a full pin (v5.1.0) the highest full
			tag (v6.1.2). Cross-major upgrades are offered; a major pin is not a
			ceiling. Prereleases are skipped unless the current ref is itself a
			prerelease.

			outdated never writes. To apply an offer, run
			'gh actions-pin update --action <nwo>@<available_ref> --write'.

			The picker mirrors the dependabot-core github_actions update checker,
			so an offered ref matches the one an eventual Dependabot PR would
			land. It is a superset of what Dependabot opens a PR for: discovery
			reports availability, not policy (cooldowns and ignore rules gate the
			PR, not the offer).

			Exit status:
			  0  everything is up to date.
			  1  one or more updates are available (available_updates is
			     non-empty). With --json, stdout still carries well-formed JSON.
			  2  the tool itself failed (IO error, tag-list or auth failure).
		`),
		Example: heredoc.Doc(`
			# List available updates as JSON
			$ gh actions-pin outdated --json=available_updates

			# Human-readable summary
			$ gh actions-pin outdated
		`),
		PreRunE: func(cmd *cobra.Command, args []string) error {
			return format.ValidateOutdatedJSONFields(opts.jsonFields)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runOutdated(cmd, opts, newResolver)
		},
	}

	cmd.Flags().StringVar(&opts.jsonFields, "json", "", "Output JSON (available_updates always emitted; `fields` accepted for symmetry with check but does not filter)")
	cmd.Flags().Lookup("json").NoOptDefVal = "available_updates"
	cmd.Flags().StringVar(&opts.hostname, "hostname", "", "GitHub hostname to query (defaults to GH_HOST, current repo host, or github.com)")

	return cmd
}

func runOutdated(cmd *cobra.Command, opts *outdatedOptions, newResolver resolverFunc) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()
	console := ui.NewWithWriter(errOut)
	defer console.StopProgress()

	pool := pinpool.New(0, console)
	_, r, store, err := newRun(nil, opts.hostname, pool, newResolver)
	if err != nil {
		return err
	}
	r.SeedFromLockfile(store.AllDeps())

	if opts.jsonFields == "" {
		console.SetLog(io.Discard)
	}

	offers, err := discover.Discover(ctx, store.AllDeps(), r.GHClient())
	if err != nil {
		console.StopProgress()
		return err
	}
	console.StopProgress()

	updates := make([]format.AvailableUpdate, 0, len(offers))
	for _, o := range offers {
		updates = append(updates, format.AvailableUpdate{
			NWO:          o.NWO,
			CurrentRef:   o.CurrentRef,
			CurrentSHA:   o.CurrentSHA,
			AvailableRef: o.AvailableRef,
			AvailableSHA: o.AvailableSHA,
			Precision:    o.Precision,
		})
	}

	if opts.jsonFields != "" {
		if err := format.WriteOutdatedJSON(out, updates, cliVersion(), store.File().Version); err != nil {
			return err
		}
	} else {
		format.PresentOutdatedSummary(console, updates)
	}

	// Exit 1 when updates are available (npm `outdated` convention): a clean,
	// scriptable signal, not a tool failure. errSilent maps to exit 1 without
	// emitting a stderr error, leaving stdout JSON intact.
	if len(updates) > 0 {
		return errSilent
	}
	return nil
}
