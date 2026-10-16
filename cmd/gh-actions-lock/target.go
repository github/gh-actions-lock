package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/github/gh-actions-lock/internal/repospec"
)

// pathExists reports whether name exists on disk. Swapped in tests.
var pathExists = func(name string) bool {
	_, err := os.Stat(name)
	return err == nil
}

// resolveTarget decides whether the invocation names a remote repository and
// splits args into the repository and workflow paths. Repository detection
// only runs under --verify, so the remote form cannot leak into other modes.
func (opts *checkOptions) resolveTarget(args []string) error {
	if !opts.verify {
		if len(args) > 0 {
			opts.workflowPaths = args
		}
		return nil
	}

	var spec *repospec.Spec
	paths := args

	switch {
	case opts.repo != "":
		s, err := repospec.Parse(opts.repo)
		if err != nil {
			return fmt.Errorf("invalid --repo: %w", err)
		}
		spec = &s
		if len(args) > 0 && repospec.LooksLikeRepo(args[0]) {
			p, err := repospec.Parse(args[0])
			if err != nil {
				return err
			}
			if !sameRepository(s, p) {
				return fmt.Errorf("conflicting repositories: --repo %s and argument %s", opts.repo, args[0])
			}
			spec.Ref = p.Ref
			paths = args[1:]
		}
	case len(args) > 0 && repospec.LooksLikeRepo(args[0]) && !pathExists(args[0]):
		s, err := repospec.Parse(args[0])
		if err != nil {
			return err
		}
		spec = &s
		paths = args[1:]
	}

	if spec == nil {
		if opts.ref != "" {
			return fmt.Errorf("--ref requires a remote repository (--repo or an OWNER/REPO argument)")
		}
		if len(args) > 0 {
			opts.workflowPaths = args
		}
		return nil
	}

	if opts.ref != "" {
		if spec.Ref != "" && spec.Ref != opts.ref {
			return fmt.Errorf("conflicting refs: %q from the repository argument and --ref %q", spec.Ref, opts.ref)
		}
		spec.Ref = opts.ref
	}
	if spec.Host != "" && opts.hostname != "" && !strings.EqualFold(spec.Host, opts.hostname) {
		return fmt.Errorf("repository host %s conflicts with --hostname %s", spec.Host, opts.hostname)
	}

	opts.remote = spec
	opts.workflowPaths = paths
	return nil
}

// sameRepository compares host (when both name one), owner and repo.
func sameRepository(a, b repospec.Spec) bool {
	if a.Host != "" && b.Host != "" && !strings.EqualFold(a.Host, b.Host) {
		return false
	}
	return strings.EqualFold(a.Owner, b.Owner) && strings.EqualFold(a.Repo, b.Repo)
}
