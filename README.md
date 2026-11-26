# gh-actions-lock

A [GitHub CLI](https://cli.github.com/) extension that pins every direct and transitive action your workflows use to a verified commit, recorded in `.github/workflows/actions.lock`.

## Install

Requires an authenticated `gh`. github.com and GitHub Enterprise Cloud with data residency (`*.ghe.com`) are supported; GitHub Enterprise Server is not.

```bash
gh extension install github/gh-actions-lock
```

To teach your AI coding agent the CLI:

```bash
gh skill install github/gh-actions-lock
```

On GHE.com, `gh` looks the shorthand up on your tenant, so install from the full URL instead:

```bash
gh extension install https://github.com/github/gh-actions-lock
gh skill install https://github.com/github/gh-actions-lock
```

To upgrade to the latest release:

```bash
gh extension upgrade actions-lock
```

## Quickstart

From the repository checkout:

```bash
gh actions-lock            # onboard or fix: pin every resolvable action and write actions.lock
git add .github/workflows/ # check into source
```

Rerun `gh actions-lock` after adding a workflow or changing any `uses:`.

Make sure you don't check in a corrupt lockfile. Add a workflow that runs the [verify action](verify/README.md) on every pull request, then run `gh actions-lock` once so that workflow is locked too:

```yaml
name: verify-lockfile
on:
  pull_request:
  push:
    branches: [main]
permissions:
  contents: read
jobs:
  verify:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7.0.1
        with:
          persist-credentials: false
      - uses: github/gh-actions-lock@verify/vX.Y.Z
```

Replace `verify/vX.Y.Z` with the [latest `verify/v*` tag](https://github.com/github/gh-actions-lock/tags). Dependabot's `github-actions` updates bump it with each CLI release.

The job's `GITHUB_TOKEN` can only read the repository the workflow runs in, so `--verify` fails on private or internal actions from other repositories even when the runner is allowed to use them. Pass a GitHub App installation token with read access to those repositories as the action's `token` input; see [Private and internal actions](verify/README.md#private-and-internal-actions).

Without such a token, or on GHE.com without a github.com token, set `args: --verify-local` for an offline check. It catches a lockfile that's out of sync with the workflows, but not a tag that moved upstream or a locked commit that no longer exists; see [Offline check](verify/README.md#offline-check).

### GitHub Enterprise Cloud with data residency

Run `gh auth login --hostname TENANT.ghe.com`, then run `gh actions-lock` from the tenant repository checkout. The host comes from the repository remote; you don't need `--hostname` or an exported token.

Actions that aren't on the tenant, such as `actions/checkout`, are read from public github.com repositories. Those lookups never use the tenant token, `GH_TOKEN` or `GITHUB_TOKEN`. They use, in order:

1. `GH_ACTIONS_LOCK_GITHUB_COM_TOKEN`, a github.com token used only for these lookups.
2. Your `gh auth login --hostname github.com` login.
3. Otherwise they're anonymous and share GitHub's limit of 60 requests an hour per IP address, which a shared proxy or runner pool can exhaust quickly.

In GitHub Actions on GHE.com, the job's `GITHUB_TOKEN` is issued by your tenant, so github.com doesn't accept it. Create a [fine-grained personal access token](https://docs.github.com/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens#creating-a-fine-grained-personal-access-token) on github.com with public repository access only and no permissions, save it as a secret on the tenant, and pass it in:

```yaml
- uses: github/gh-actions-lock@verify/vX.Y.Z
  env:
    GH_ACTIONS_LOCK_GITHUB_COM_TOKEN: ${{ secrets.ACTIONS_LOCK_GITHUB_COM_TOKEN }}
```

Or, running the CLI directly:

```yaml
- run: gh actions-lock --verify
  env:
    GH_TOKEN: ${{ github.token }}
    GH_ACTIONS_LOCK_GITHUB_COM_TOKEN: ${{ secrets.ACTIONS_LOCK_GITHUB_COM_TOKEN }}
```

### Persisting anonymous public lookups

Set `GH_ACTIONS_LOCK_CACHE_DIR` to cache successful anonymous github.com API responses across runs on a `*.ghe.com` tenant. Entries are keyed by API URL and representation, so changing one dependency reuses the other dependencies' cached resources. Successful responses are saved immediately, even if other lookups hit the rate limit and the command fails. Errors, authenticated requests, and tenant responses are never cached.

`GH_ACTIONS_LOCK_CACHE_TTL` sets the maximum age of an entry (default `24h`). Cached repository identity and ref reachability can be stale for that long; GitHub still validates the lockfile at execution time. Unset the cache directory for fresh online checks. Cache read or write failures fall back to online requests.

In CI, restore this directory before verification and save it even when verification fails, using a new cache key each run so partial progress accumulates. Restore only caches from trusted workflows; cache contents are verification inputs, not independently authenticated evidence. Do not share a writable cache across trust boundaries. A cold run can still be rate-limited and fail until enough responses have been collected.

## Modes

| Command | Writes | Network | Use for |
|---|---|---|---|
| `gh actions-lock` | yes | yes | Onboarding and fixing the lockfile |
| `gh actions-lock --relock` | yes | yes | Advancing moved refs (`main`, `v4`) to current upstream commits |
| `gh actions-lock --verify` | no | yes | CI: the same checks as a fix run |
| `gh actions-lock --verify-local` | no | no | Pre-commit, or CI without a token: every `uses:` has a lockfile entry (coverage only, not provenance) |
| `gh actions-lock --no-fix` | no | yes | Read-only report |

Other flags:

- `<workflow-path>...` scopes a run to specific workflows. Only a full-directory run prunes entries for deleted workflows. An action file (`action.yml` or `action.yaml`) isn't accepted: composite actions can't carry their own lockfile yet, so lock the workflows that call it.
- `--json [fields]` prints machine-readable output to stdout.
- `--accept-moved` accepts pins whose locked commit is no longer upstream. Check why it moved first; never use it in automation.
- `--no-narrow` keeps partial versions (`v4`) as written instead of narrowing to a full tag.
- `--fail-on-warning` makes `--verify`, `--verify-local` and `--no-fix` exit `1` when any warning is reported.

`gh actions-lock --help` is authoritative.

## Limitations

- Reusable workflows called at `jobs.<id>.uses` from another repository are not locked ([#179](https://github.com/github/gh-actions-lock/issues/179)).
- Local `./…` actions are never followed or locked, so one that uses remote actions is a blocking error until you pass `--migrate-local-actions` (rewrites same-repo `./…` in workflows to `$/…`) or `--ignore-local-actions`. Action files are never edited, so change a `./…` inside a same-repo action to `$/…` yourself.
- A full commit SHA in a same-repo `$/…` action is never rewritten to a tag, since other repositories that use that action don't have your lockfile. It's locked under the SHA in every workflow that reaches it ([#264](https://github.com/github/gh-actions-lock/issues/264)).
- Container images are not locked, and any image without an `@sha256:` digest, including one chosen by a `${{ }}` expression, is a warning ([#236](https://github.com/github/gh-actions-lock/issues/236)).
- `--verify-local` is an offline coverage check that doesn't verify provenance; `--verify` checks provenance before merge, and the runner checks it again at job start.
- A lockfile change that moves a floating ref (`v4`, `main`) back to an older commit in its history isn't detected, so review lockfile diffs or pin exact tags.

## Learn more

- [Workflow dependency lockfiles](https://docs.github.com/en/actions/concepts/security/workflow-dependency-lockfiles)
- [Lock workflow dependencies](https://docs.github.com/en/actions/how-tos/secure-your-work/lock-workflow-dependencies)
- [Enforce workflow lockfiles](https://docs.github.com/en/actions/how-tos/administer/enforce-workflow-lockfiles)
- [Troubleshooting](docs/troubleshooting.md)

## Contributing

See [CONTRIBUTING.md](./CONTRIBUTING.md)

## Support

See [SUPPORT.md](./SUPPORT.md).

## License
Licensed under [MIT](./LICENSE).
