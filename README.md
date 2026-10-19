# gh-actions-lock

Lock your workflow dependencies.

> [!WARNING]
> **Public preview.** gh-actions-lock is pre-1.0 and under active development. The
> lockfile format, command flags, and behavior may change without notice between
> releases. Use it, file issues, and expect rough edges.

## Background

gh-actions-lock is part of GitHub's Workflow Dependency Pinning effort. It gives repositories a lockfile that pins every workflow dependency to a verified commit, so what runs on the runner is exactly what you locked. Development is ongoing and behavior may still change.

Contributions are welcome. See [CONTRIBUTING.md](./CONTRIBUTING.md) to get started.

## Requirements

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

Locked pins are sticky: a normal run only re-resolves refs the lockfile does
not record yet. Every locked pin, including transitive ones, is re-checked the
way the runner checks it at job start: the repository was not renamed or
transferred, the pinned commit still exists, and full-version tags (e.g.
`v4.2.1`) still point at it. A failed check blocks the run.

Writes are all or nothing: if a run finds any error it cannot fix itself, it
changes no workflow and no lockfile entry, not even for workflows that are
otherwise fine.

To check without writing anything, for example in CI, run:

```bash
gh actions-lock --verify
```

Every mode checks each locked commit against the lockfile entry's `ref`, which
is looked up only as a tag, then a branch, the way the runner does. An exact
tag (`v4.2.1`) must still point at the commit. Any other pin, including a branch
(`main`), a partial version (`v4`) or a full SHA (`uses: owner/repo@<sha>`),
must have its commit in that ref's history, so a fork commit can't pass as
upstream. A `ref` the runner would reject makes the lockfile invalid (exit
2): it must be a short tag or branch name, never a SHA or a `refs/` name, and
it must equal the `uses:` ref unless that is a full SHA. A `uses:` ref that is
neither a tag nor a branch, such as `pull/1/head`, is never locked. Generation
and relocking use the same tag-first lookup. If a tag and branch share a name
but point at different commits, the run fails without writing. Choose a distinct,
unambiguous tag or branch name; relocking the same ambiguous name cannot fix it.
Every online non-refresh run also reads action manifests at
the locked commits to check that the recorded transitive graph covers their
dependencies, without advancing to current upstream commits.

`--verify-local` runs offline and never writes. It checks direct refs and all
recorded transitive edges for missing dependency entries, but cannot discover an
edge erased from the lockfile or establish that recorded commits are
trustworthy; use `--verify` in CI for that.

Pins to branches or partial versions (e.g. `main`, `v4`) are not advanced when
upstream moves. To regenerate the lockfile from the current upstream commits,
run:

```bash
gh actions-lock --relock
```

`--relock` re-resolves the complete graph, even when no recorded ref has moved,
and restores missing entries and edges. A normal sticky write rejects dangling
recorded references: restore missing entries from version control to keep the old
commits, or explicitly relock to select current upstream commits.
Refreshes reject conflicting commits for a shared dependency rather than
combining manifests from different commits into one lockfile entry.
Suspicious pins whose recorded commit is no longer
reachable upstream are left as errors — use `--accept-moved` to re-resolve
those as well.

A deleted recorded ref still blocks, including with `--relock --accept-moved`.
Restore the trusted ref or deliberately select and review another ref. A branch
force-push that removes the locked commit from its history also blocks a normal
run; it is not enough that another ref still contains that commit.

### Container image warnings

Action commits do not pin the contents of mutable container image tags. The CLI
warns in terminal output about direct `uses: docker://...` images and images in
remote action manifests it reads: Docker actions' `runs.image` and composite
actions' `uses: docker://...`, including transitive actions and cached manifests.
Warnings name the action responsible and do not change JSON findings or exit
codes. Digest-qualified images do not warn.

`--verify` inspects the recorded commits' manifests. A sticky run that does not
need those manifests, or offline `--verify-local`, cannot discover images hidden
behind remote actions. Dockerfiles and their base images are not analyzed.

### GitHub Enterprise Cloud with data residency

Authenticate `gh` to your tenant, then
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
on fix runs. The migration rewrites `./…` steps both in your workflows and in
your in-repo composite action definitions (`action.yml`). Only `./…` paths that
resolve to an in-repo action file are rewritten. `--verify` and `--verify-local`
never migrate; they report `./…` refs as `local-action` findings instead. To
leave `./…` refs untouched on fix runs, opt out with
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
