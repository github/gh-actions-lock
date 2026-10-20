package main

// Verification modes for gh actions-lock:
//
//   --verify       Frozen check: the default checks, including runner parity
//                  for every locked pin, without writing. Requires auth.
//                  With -R or an OWNER/REPO argument it reads the repository
//                  through the API instead of the working tree.
//
//   --verify-local Offline static coverage check. Every action ref must have
//                  a lockfile entry. No network, no auth, no writes — ideal for
//                  pre-commit. Same-repo `./…` refs are reported, not migrated.

import (
	"context"
	"io"

	"github.com/github/gh-actions-lock/internal/ui"
)

// verifier is one self-contained verification mode. It reports a failing
// verification as errSilent after rendering the findings itself.
type verifier interface {
	Verify(ctx context.Context, out io.Writer, console *ui.UI) error
}

// selectVerifier returns the verifier for the standalone modes, or nil when
// the invocation is a regular check run that continues in runCheck.
func selectVerifier(opts *checkOptions, newResolver resolverFunc) verifier {
	switch {
	case opts.remote != nil:
		return remoteVerifier{opts: opts, newResolver: newResolver}
	case opts.verifyLocal:
		return localVerifier{opts: opts}
	}
	return nil
}

// applyVerifyFlags expands --verify into its constituent flags. Called at the
// top of runCheck before any work begins.
func applyVerifyFlags(opts *checkOptions) {
	if opts.verify {
		opts.noFix = true
	}
}
