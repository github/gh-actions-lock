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
transferred, the pinned commit still exists, and it is still in the history of
the tag or branch the lockfile recorded. A failed check blocks the run.

A locked composite's dependencies are re-checked too. Each locked action's
`action.yml` (or `action.yaml`) is read at its locked commit. A run fails if
the file is missing, or if the composite uses an action its lockfile entry
doesn't record. A manifest that can't be read fails the run as a retryable
error (`"retryable": true` in `--json`). `--relock` skips this check unless the run is read-only,
because it re-reads every manifest anyway.

Writes are all or nothing: if a run finds any error it cannot fix itself, it
changes no workflow and no lockfile entry, not even for workflows that are
otherwise fine.

To check without writing anything, for example in CI, run:

```bash
gh actions-lock --verify
```

For a ready-to-copy CI job that runs this on pull requests, see
[`.github/workflows/verify-lockfile.yml`](.github/workflows/verify-lockfile.yml).

If the GitHub API is unreachable or rate-limited, `--verify` retries for up to
about 2 minutes in CI (a few seconds elsewhere), then fails (exit 1) with the pins it couldn't verify. Their
`--json` findings carry `"retryable": true`; re-run the check once the API is
reachable. An untrusted TLS certificate or a rejected token stops the run
instead (exit 2).

Every mode checks each locked commit against the lockfile entry's `ref`, which
is looked up only as an existing tag, then branch, of exactly that name, the
way the Actions backend resolver does. Every pin, whether an exact tag
(`v4.2.1`), a partial version (`v4`), a branch (`main`) or a full SHA (`uses:
owner/repo@<sha>`), must have its commit in that ref's history, so a fork
commit can't pass as upstream. An exact tag means exactly one commit: if the
entry locks an older commit in the tag's history, for example because a
publisher re-pointed the tag, every mode fails with `tag-commit-mismatch`
until `gh actions-lock --relock` locks the tag's commit, including for an
entry a composite action pins. Floating refs (`v4`,
`main`) accept any commit in the ref's history, so a lockfile change that moves
one to an older commit isn't detected. Review lockfile diffs or pin exact tags.
A full-SHA `uses:` whose recorded tag moved ahead only warns under `--verify`
with "Tag now points past locked commit", since the commit is in the workflow.
A lockfile entry whose `ref` the Actions backend resolver
would reject fails the run (exit 2), naming the first such entry in the file and its line:
it must be a short tag or branch name the backend accepts, never a SHA, a
`refs/` name, revision syntax such as `main~1`, or a `git describe` name, and
it must equal the `uses:` ref unless that is a full SHA. A `uses:` ref that is
not such a tag or branch, such as `pull/1/head` or `main~1`, is never locked.

One exception loads instead: an entry keyed by a full SHA whose `ref` is that
same SHA. The Actions backend still rejects it, so `--verify`, `--no-fix` and
`--verify-local` report it as a `not-pinned` error (exit 1) and write nothing,
wherever it sits: in a scanned workflow, in a workflow the run didn't scan or
that was deleted, under a composite, or used by nothing. A plain run or
`--relock` records the tag at that commit, else a branch containing it, keeps
the full-SHA `uses:`, and drops the entry if nothing uses it; a commit no tag
or branch contains still fails the run and nothing is written. A run given
workflow paths leaves another workflow's such entry as it is, warns about it
(exit 0), and doesn't report every workflow valid.
`--verify-local` runs
offline, never writes, and only checks that every ref has a lockfile entry and
that every entry a workflow reaches through `uses:` exists, not that the
entries are trustworthy; use `--verify` in CI for that. A missing entry fails
every mode, and a plain run writes nothing; run `gh actions-lock --relock` to
rebuild the graph.

Pins to branches or partial versions (e.g. `main`, `v4`) are not advanced when
upstream moves. To regenerate the lockfile from the current upstream commits,
run:

```bash
gh actions-lock --relock
```

`--relock` re-resolves every ref, rewrites the lockfile to the current SHAs,
and rebuilds each composite action's `uses:` from its manifest, even when no
ref moved, so children it gained are added and ones it dropped go. Suspicious
pins whose recorded commit is no longer reachable upstream are left as errors —
use `--accept-moved` to re-resolve those as well. A plain run keeps a locked
composite's recorded `uses:` as they are.

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

### Local actions

`uses: ./…` names a path in the runner workspace, which checkouts and scripts
can change, so `gh actions-lock` cannot follow it, and never locks or rewrites
it. That includes `./…` steps inside remote composite actions. A
workspace-local action's own `uses:` dependencies are **outside lockfile coverage**.

When the action's `action.yml` is in the checkout and uses remote actions, the
runner needs a lockfile pin for each one and fails the job without it. Every
mode, including `--verify` and `--verify-local`, reports this as a blocking
`local-action` error. The message doesn't list those actions, since the
workspace may hold something else at run time. To fix it, either:

- pass `--migrate-local-actions` on a fix run to rewrite same-repo `./…`
  references to `$/…`, in workflows and in-repo `action.yml` files, which the
  CLI then follows and locks; or
- pass `--ignore-local-actions` to keep `./…` and report it as a warning.
  `--no-migrate-local-actions` is an alias.

A `./…` inside a remote composite can't be rewritten, so only
`--ignore-local-actions` applies there.

A `./…` action the CLI can't read from the checkout, or one with only `run:`
steps, is a non-blocking `local-action` warning. Both link to the
[troubleshooting docs](docs/troubleshooting.md#local-action).
The workflow's other dependencies are locked and checked as usual.

## How it works

A repo gets a lockfile (located at [`.github/workflows/actions.lock`](https://github.com/github/gh-actions-lock/blob/main/.github/workflows/actions.lock)) and workflows are onboarded to the lockfile on a per-workflow basis. 

Workflows that are onboarded to the lockfile enforce that all dependencies are present in the lockfile and guarantees that the locked commit for an Action is what's executed on the runner. Lockfiles are also verified for forgeries. The sha must exist in the refs it's stated to exist in. Repository identity is recorded and redirects and mismatches are blocked at runtime. 

Finally, locked actions must have a branch that the commit being locked exists within. This is to make impostor commit style attacks harder.

## Limitations

- Local `./…` actions and their dependencies are outside lockfile coverage; see
  [Local actions](#local-actions).

The lockfile pins action commits, not the container images that workflows and Docker actions pull or build. Any image not pinned by `@sha256:` digest (a `docker://` step, a job `container:` or `services:` image, or a Docker action's `runs.image`) is reported as a non-blocking `container-image` warning. This includes images chosen by a `${{ }}` expression and Docker actions built from a Dockerfile, whose base image the lockfile doesn't cover.

## License

This project is licensed under the terms of the MIT open source license. See [LICENSE](./LICENSE) for the full terms.

## Maintainers

gh-actions-lock is maintained by @github/actions-dispatch-reviewers. See [CODEOWNERS](./CODEOWNERS).

## Support

For help with a specific error or warning, see [Troubleshooting](docs/troubleshooting.md).

Support is best-effort and community-based. Please file bugs and feature requests as [GitHub issues](https://github.com/github/gh-actions-lock/issues). See [SUPPORT.md](./SUPPORT.md) for details.
