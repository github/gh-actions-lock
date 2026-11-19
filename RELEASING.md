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

Use `patch` for compatible fixes, `minor` for compatible additions, and `major`
for breaking changes.

The script validates the repository, checks the current branch and `origin/main`,
then pushes and verifies an annotated `vX.Y.Z` or `vX.Y.Z-rc.N` tag.

The tag workflow verifies the tag, then publishes the binaries, attestations,
and GitHub release.

It then publishes the [verify action](actions/verify/README.md) for the
release. `script/verify-action-release.mjs` checks the release binaries'
attestations, sets the action's default `version` to the new tag and records the
release binaries' checksums in `actions/verify/checksums.txt`. The workflow
commits that on top of the release commit and tags it `verify-vX.Y.Z`. Don't
edit the default or the checksums by hand.

`main` also carries a stamp, so this repository's own `verify-lockfile`
workflow runs the action the way other repositories do. The stamp for a release
can only land after it: the binaries embed the commit they're built from. So
after each release, run
`node script/verify-action-release.mjs --stamp-only vX.Y.Z` (it checks the
attestations with `gh`) and merge the result. `script/release` refuses to cut
the next release, including a release candidate, until `main` is stamped for
the latest stable release. Stamp `main` with stable releases only.

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
