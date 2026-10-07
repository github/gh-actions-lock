# gh-actions-lock

Lock your workflow dependencies.

> [!WARNING]
> **Technical Preview.** gh-actions-lock is pre-1.0 and under active development. The
> lockfile format, command flags, and behavior may change without notice between
> releases. Use it, file issues, and expect rough edges.

## Background

gh-actions-lock is part of GitHub's Workflow Dependency Pinning effort. It gives repositories a lockfile that pins every workflow dependency to a verified commit, so what runs on the runner is exactly what you locked. Development is ongoing and behavior may still change.

Contributions are welcome. See [CONTRIBUTING.md](./CONTRIBUTING.md) to get started.

## Requirements

Supported targets are `github.com` and GitHub Enterprise Cloud with data
residency (`*.ghe.com`), subject to the [availability note below](#github-enterprise-cloud-with-data-residency).
GitHub Enterprise Server (GHES) is not supported.

Requires the [`gh` CLI](https://cli.github.com/). Install it first, then install the extension:

```bash
gh extension install github/gh-actions-lock
```

## Usage

Scan every workflow under `.github/workflows/` directory, pin each resolvable action to a SHA, and update the lockfile:

```bash
gh actions-lock
```

After the initial run to onboard workflows, you will need to run `gh actions-lock` when:
- A new workflow is created that has `uses` dependencies.
- An existing workflow adds or removes `uses` dependencies.

A full-directory run (`gh actions-lock` with no path arguments) also prunes lockfile entries for workflows that have been deleted from `.github/workflows/`, dropping any dependencies left orphaned by the removal. Scoped runs that name specific workflows never prune out-of-scope entries.

Pins to branches or partial versions (e.g. `main`, `v4`) are trusted from the
lockfile and not re-resolved on a normal run. To bump them to the current
upstream commit, run:

```bash
gh actions-lock --relock
```

`--relock` re-resolves refs that have legitimately moved and rewrites the
lockfile to the new SHA. Suspicious pins whose recorded commit is no longer
reachable upstream are left as errors — use `--accept-moved` to re-resolve
those as well.

### GitHub Enterprise Cloud with data residency

> [!IMPORTANT]
> Hostname-aware tenant/public resolution is not yet released. It is being
> developed in [#137](https://github.com/github/gh-actions-lock/pull/137);
> neither v0.1.6 nor v0.1.7-rc.1 includes it. Installing or upgrading the
> published extension does not install this draft implementation.

With a build that includes this support, authenticate `gh` to your tenant, then
run the extension from your tenant repository checkout:

```bash
gh auth login --hostname octocorp.ghe.com
# From the repository checkout:
gh actions-lock
```

With no conflicting environment overrides, the CLI infers the host from the
repository remote and uses the credentials stored by `gh` for that host. You do
not need to export a token or pass `--hostname` on every run. The account must
have read access to the tenant repositories used by your workflows.

#### Host and credential overrides

Host selection and credential selection are separate. Host selection uses the
first available source:

1. `--hostname`.
2. `GH_HOST`.
3. The current repository from `gh`: `GH_REPO` if set, otherwise a remote on a
   host known to `gh`. Among eligible remotes, `upstream` takes precedence over
   `github`, then `origin`.
4. `github.com` if the current repository cannot be determined.

A host-qualified `GH_REPO` such as `octocorp.ghe.com/OWNER/REPO` overrides remote
discovery. An unqualified `OWNER/REPO` uses `gh`'s default host: the sole
configured host if there is one, otherwise `github.com` (unless `GH_HOST` is set).
Authenticate to the tenant before relying on remote discovery. For an
unambiguous host override:

```bash
gh actions-lock --hostname octocorp.ghe.com --no-interactive
```

For `github.com` and `*.ghe.com`, the first **nonempty** credential source wins:
`GH_TOKEN`, then `GITHUB_TOKEN`, then stored credentials for that host.

Stored credentials come from `gh` configuration or its secure credential store.
`GH_ENTERPRISE_TOKEN` does **not** select credentials for `*.ghe.com`.
Conversely, a dotcom `GH_TOKEN` can override valid stored tenant credentials
and cause a tenant `401`. `--hostname` does not override token environment
variables. For tenant requests, a rejected token is not retried using stored
credentials or anonymous access.

#### Diagnose authentication without exposing tokens

Check which overrides are set without printing their values:

```bash
for name in GH_HOST GH_REPO GH_TOKEN GITHUB_TOKEN; do
  if printenv "$name" >/dev/null; then
    printf '%s is set\n' "$name"
  fi
done
```

If the overrides are unintended, test stored tenant credentials with a
command-scoped clean environment. These commands do not change your shell's
environment or print token values:

```bash
env -u GH_TOKEN -u GITHUB_TOKEN \
  gh auth status --hostname octocorp.ghe.com
env -u GH_TOKEN -u GITHUB_TOKEN \
  gh api --hostname octocorp.ghe.com user --silent
```

If needed, sign in without the conflicting token overrides:

```bash
env -u GH_TOKEN -u GITHUB_TOKEN \
  gh auth login --hostname octocorp.ghe.com
```

Then, from the tenant checkout, bypass unintended host, repository, and token
overrides for a read-only remote check:

```bash
env -u GH_HOST -u GH_REPO -u GH_TOKEN -u GITHUB_TOKEN \
  gh actions-lock --hostname octocorp.ghe.com --rescan --no-fix
```

Keep intentional overrides, especially in automation; supply a token valid for
the selected host instead. Do not share token values or use
`gh auth status --show-token` in diagnostic output. A `403` can also mean missing repository
access or an organization policy restriction; changing hosts or retrying
anonymously is not a remedy.

#### Resolution and lockfile behavior

New dependencies resolve on the tenant first. Only a repository-level `404`
permits fallback to a **public** repository on `github.com`. A tenant repository
shadows its dotcom namesake even when the requested ref is missing. Authorization
errors, rate limits, and network failures do not trigger fallback.

When generating from a `*.ghe.com` tenant, the v0.0.3 lockfile records every
dependency's hostname, including `github.com` for public dependencies, alongside
its commit and host-specific repository IDs. Dotcom-root generation and refresh
also write v0.0.3 but omit `hostname` for dotcom dependencies; omission means
`github.com`. Existing pins retain their host identity, including
during `--rescan` and `--relock`. Legacy v0.0.1/v0.0.2 pins and v0.0.3 pins without
a hostname mean `github.com`. Legacy repository IDs are checked against dotcom
before migration on a tenant; tenant pins generated by older CLI versions must
be regenerated rather than relabeled. Conflicting host assignments for the same
repository are rejected.

Public fallback uses unauthenticated dotcom requests, subject to GitHub's
anonymous API rate limit. Tenant tokens and headers are never forwarded to
dotcom.

If any dependency cannot be resolved, generation exits nonzero without writing
an incomplete lockfile. `--json` reports `valid: false`. `--verify-local` checks
only recorded coverage; it does not prove successful remote resolution.

### Self repository actions (`$/…`)

`uses: $/…` references an action or reusable workflow in the **same repository** as
the defining file, resolved at the **running commit**. Because it always resolves to
that repository's running SHA it is **inherently pinned** — no lockfile entry is
required, and it is valid anywhere a relative `./…` reference is:

```yaml
steps:
  - uses: $/actions/my-action          # same-repo action, inherently pinned
jobs:
  call:
    uses: $/.github/workflows/reusable.yml  # same-repo reusable workflow
```

A trailing `@ref` (e.g. `$/actions/my-action@v1`) is rejected — the ref is always
the running commit.

Same-repo `./…` composite action references are automatically converted to `$/…`
on fix runs. This rewrites `./…` steps both in your workflows and in your in-repo
composite action definitions (`action.yml`). Only `./…` paths that resolve to an
in-repo action file are rewritten. To leave `./…` refs untouched, opt out with
`--no-migrate-local-actions`:

```bash
gh actions-lock --no-migrate-local-actions
```

## How it works

A repo gets a lockfile (located at [`.github/workflows/actions.lock`](https://github.com/github/gh-actions-lock/blob/main/.github/workflows/actions.lock)) and workflows are onboarded to the lockfile on a per-workflow basis. 

Workflows that are onboarded to the lockfile enforce that all dependencies are present in the lockfile and guarantees that the locked commit for an Action is what's executed on the runner. Lockfiles are also verified for forgeries. The sha must exist in the refs it's stated to exist in. Repository identity is recorded and redirects and mismatches are blocked at runtime. 

Finally, locked actions must have a branch that the commit being locked exists within. This is to make impostor commit style attacks harder.

## Limitations

There are currently eligibility limitations for workflows that can be onboarded to lockfiles:
- Workflows in the lockfile cannot use local-path actions, these will be skipped for onboarding. This is also a short-term gap.

## License

This project is licensed under the terms of the MIT open source license. See [LICENSE](./LICENSE) for the full terms.

## Maintainers

gh-actions-lock is maintained by @github/actions-dispatch-reviewers. See [CODEOWNERS](./CODEOWNERS).

## Support

Support is best-effort and community-based. Please file bugs and feature requests as [GitHub issues](https://github.com/github/gh-actions-lock/issues). See [SUPPORT.md](./SUPPORT.md) for details.
