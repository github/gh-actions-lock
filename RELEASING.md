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
points anywhere else.

Use `patch` for compatible fixes, `minor` for compatible additions, and `major`
for breaking changes.

The script validates the repository, checks the current branch and `origin/main`,
then pushes and verifies an annotated `vX.Y.Z` or `vX.Y.Z-rc.N` tag.

The tag workflow verifies the tag, then publishes the binaries, attestations,
and GitHub release.

## Verify action

Each stable release also gets a [verify action](verify/README.md) tag,
`verify/vX.Y.Z`, after the release:

1. After the tag workflow finishes, `script/release` runs
   `node script/verify-action-release.mjs vX.Y.Z` with your `gh` credentials. It
   checks the release binaries' attestations, adds their checksums to
   `verify/checksums.json` as the new default, and opens that change
   from `verify-stamp-vX.Y.Z` as a pull request to `main`, in a commit GitHub
   signs.
2. Merge that pull request.
3. Run `node script/verify-action-release.mjs --tag vX.Y.Z`. It tags `main`
   `verify/vX.Y.Z` once `main`'s `checksums.json` defaults to `vX.Y.Z`.

The stamp can't land with its own release, because the binaries embed the
commit they're built from, so users pin `verify/vX.Y.Z`, not `vX.Y.Z`.
`checksums.json` keeps every stamped release; stamping never changes an
existing entry. Don't edit it by hand. Both steps are safe to rerun: the first
reuses its branch and pull request when they still match a fresh stamp, and the
second accepts an existing tag on a commit stamped for `vX.Y.Z`. Release
candidates get no verify tag. The next release, including a release candidate,
waits until `main` is stamped and tagged.

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
