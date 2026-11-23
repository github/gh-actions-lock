# Releasing `gh-actions-lock`

## Cutting a release

From a clean, current `main`, preview the next release:

```sh
RELEASE_DRY_RUN=1 script/release patch
```

Then cut it:

```sh
script/release patch
```

To preview a release candidate:

```sh
RELEASE_DRY_RUN=1 script/release patch --rc
```

Then cut it:

```sh
script/release patch --rc
```

The first candidate is `vX.Y.Z-rc.1`; repeating the same bump increments
`rc.N`. Run the bump without `--rc` to publish the stable `vX.Y.Z`.

Release tags are immutable. If publishing fails, fix the publisher and run
the release script again for the next RC.

Rerunning a step is safe. If the push was held, rerun the same command: it
pushes the local tag when that tag already points at `HEAD`, and stops if it
points anywhere else. After a successful push, a rerun reuses the stamp pull
request below if it still matches a fresh stamp, then stops until it's merged.
Rerunning the tag workflow skips tagging when `verify-vX.Y.Z` is already the
same signed stamp on the release commit.

Use `patch` for compatible fixes, `minor` for compatible additions, and `major`
for breaking changes.

The script validates the repository, checks the current branch and `origin/main`,
then pushes and verifies an annotated `vX.Y.Z` or `vX.Y.Z-rc.N` tag.

The tag workflow verifies the tag, then publishes the binaries, attestations,
and GitHub release.

It then stamps the [verify action](actions/verify/README.md):
`script/verify-action-release.mjs` checks the binaries' attestations, writes
the tag and the binaries' checksums to `actions/verify/checksums.json`, commits
that on top of the release commit and tags it `verify-vX.Y.Z`. Users pin
`verify-vX.Y.Z`; the `vX.Y.Z` commit still holds the previous release's stamp.
Don't edit `checksums.json` by hand.

`main` carries a stamp too, so this repository's `verify-lockfile` workflow runs
the action the way other repositories do. That stamp can only land after the
release, because the binaries embed the commit they're built from. After a
stable tag, `script/release` waits for the tag workflow, stamps a
`verify-stamp-vX.Y.Z` branch off `main` with `--stamp-only`, and opens its pull
request with your `gh` credentials so its checks run. Release candidates don't
stamp `main`. The next release, including a release candidate, waits until that
pull request is merged.

## Dependabot compatibility

Before releasing, decide whether
[Dependabot's CLI integration](https://github.com/dependabot/dependabot-core/blob/main/github_actions/lib/dependabot/github_actions/lockfile/cli_engine.rb)
must move with the release. Update `dependabot-core` when the release changes:

- CLI flags used by Dependabot.
- Findings JSON or exit codes.
- Relocking behavior.
- The lockfile schema.

Dependabot
[pins the CLI version and binary checksums](https://github.com/dependabot/dependabot-core/blob/main/github_actions/Dockerfile)
and currently
[accepts lockfile schemas `v0.0.2` and `v0.0.3`](https://github.com/dependabot/dependabot-core/blob/main/github_actions/lib/dependabot/github_actions/constants.rb).
This CLI writes `v0.0.3`. Add schema support there before this CLI emits a new
version. Unrelated releases do not need a Dependabot bump.

The findings JSON fields can stay the same while behavior changes, which still
needs this decision.
