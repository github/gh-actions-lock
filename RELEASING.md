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
Add schema support there before this CLI emits a new version. Unrelated releases
do not need a Dependabot bump.

## Recorded-ref validation upgrade

The release candidate containing #174 is behavior-breaking and needs a major
release classification and Dependabot coordination before publication. It does
not introduce a lockfile schema or select a release version.

The combined candidate also blocks mutable external job-level reusable calls
with `not-pinned` findings and warns about full-SHA callees whose dependencies
are not inspected. Configured cooldown now gates newly adopted commits in
write modes, including relock and accept-moved; refusals use the new
`release-cooldown` error category. The optional `--cooldown-days` flag overrides
the selected policy's default; explicit zero disables all cooldowns for that
run. Cover these changed findings, exit statuses, no-write failures, and policy
overrides in the Dependabot compatibility review. No JSON field or lockfile
schema change is required.

Previously accepted locks can now fail to load (exit 2): every entry's `ref`
must be non-empty, not a SHA, not `refs/`-qualified, and must match its key's
ref unless the key uses a full SHA. Restore trusted lock metadata from version
control or regenerate it from reviewed workflow refs; do not replace invalid
metadata with guessed ref names.

Online validation rejects pins whose recorded ref is missing or no longer
contains the commit. Exact version tags must still point at that commit.
Restoring the trusted recorded ref or explicitly reviewing a different ref is
required for a deleted ref; another ref containing the commit is not a fallback.
`--relock --accept-moved` can deliberately select current commits from existing
refs, but cannot bypass a missing or ambiguous recorded ref.

Generation, relocking, and verification check tags before branches. If both
names exist at different commits, the CLI emits a blocking `unreachable-pin`
finding without writing. Recovery requires an unambiguous ref name, not a
repin hint. The existing category and additive-only JSON contract are preserved.
Dependabot consumes pre-fix findings but post-fix exit status: validate both,
including exit 2 parse failures, proxy lookup failures, collision remediation,
and complete transitive-graph rebuilding, before updating its binary checksums.

Every online mode except explicit relocking checks transitive edges against
manifests at locked commits. Offline verification cannot detect edges erased from both the graph
and metadata. Explicit relocking rebuilds the graph even when no ref moved.
Container-image warnings remain terminal-only and non-blocking.

Local `./…` actions no longer block locking or verification. They are never
followed, locked, or automatically migrated to `$/…`; their nested dependencies
remain outside lockfile coverage. The existing `local-action` finding is now
informational and visible in JSON. Validate these severity and exit-code changes
with Dependabot, including workflows that mix local actions with missing or
rejected remote pins. `--no-migrate-local-actions` remains a compatibility no-op.

Newly emitted lockfiles use the canonical documentation URL in their header.
Expect a one-time comment-only change when an existing lockfile is rewritten;
the header update does not change its schema.
