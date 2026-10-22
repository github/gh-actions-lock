# Troubleshooting `gh actions-lock`

Find the cause of an error or warning from `gh actions-lock`, and fix it without losing the dependencies you already reviewed.

To learn more about lockfiles, see [Workflow dependency lockfiles](https://docs.github.com/actions/concepts/security/workflow-dependency-lockfiles) and [Locking workflow dependencies](https://docs.github.com/actions/how-tos/secure-your-work/lock-workflow-dependencies).

## Before you start

Most problems are fixed by the same few steps:

1. Make sure you have the latest version of the extension.

   ```shell
   gh extension upgrade gh-actions-lock
   ```

1. From the root of your repository, check the lockfile against your workflows and against GitHub. This runs the same checks as a fix run, and doesn't change any files.

   ```shell
   gh actions-lock --verify
   ```

1. Find the category of each problem, such as `not-pinned` or `unreachable-pin`, and use the matching section below. Each problem in the output links to its section. See [Reading the output](#reading-the-output).

Don't delete `.github/workflows/actions.lock` to make an error go away. See [Why you shouldn't delete the lockfile](#why-you-shouldnt-delete-the-lockfile).

For the full set of options in your installed version, run `gh actions-lock --help`.

## Reading the output

Each problem that `gh actions-lock` reports has a category, such as `unreachable-pin`. Each category has a section in this document, and the section's anchor is the category name.

In the terminal, most problems start with a short headline, such as **Pinned commit not in upstream history**, and end with a `see:` link to their section. The sections below show each headline. In `--json` output, each finding has a `category` field and a `doc_url` field with the same link.

### Severity

The `severity` field in `--json` output is one of the following values.

| Severity | Meaning |
| --- | --- |
| `error` | Blocks the run. Nothing is written. |
| `warning` | Doesn't block the run. |
| `info` | A suggestion. |

A finding with `"retryable": true` is an `error` because GitHub didn't answer the check. See [`reachability-unknown`](#reachability-unknown).

### Exit codes

| Exit code | Meaning |
| --- | --- |
| `0` | No errors remain. Warnings and notices don't change the exit code. |
| `1` | One or more findings block the run. With `--json`, the output on `stdout` is complete. |
| `2` | The tool failed, for example because of an invalid option, a lockfile it can't read, a lockfile version newer than the extension, or a rejected token. Read the message on `stderr`. |

When a run that updates the lockfile succeeds, `--json` reports the state the run left: `valid` is `true`, and errors that the run fixed aren't listed. When a run exits with code `1`, `--json` reports the problems it found.

### Categories that need no action

These categories don't have a section, because they report no problem: `valid`, `run-only` (the workflow has only `run:` steps), and `self-repository-action` (the workflow uses a `$/` reference, which needs no lockfile entry).

## Errors

These findings block the run. While any of them remains, `gh actions-lock` doesn't change any workflow or lockfile entry, and `--verify` exits with code `1`.

### `not-pinned`

**Headline:** Not pinned

A workflow has `uses:` dependencies with no matching entry in the lockfile. Run `gh actions-lock` and commit the workflow and the lockfile together.

The same fix applies when the message says `workflow has no lockfile entry`, which means the workflow isn't in the lockfile at all.

The extension also reports `not-pinned` for a lockfile entry that's keyed by a full-length commit SHA and records that same SHA as its `ref`. Run `gh actions-lock` to record the tag or branch for that commit.

### `ref-missing`

**Headline:** Ref does not exist

A `uses:` reference doesn't exist in the action's repository, so there's nothing to lock. The reference can be a tag, a branch, or a commit. Regenerating the lockfile doesn't fix this. Change the `uses:` line, then run `gh actions-lock`.

* **For a tag or branch,** point `uses:` at a tag or branch that exists.
* **For a short hexadecimal reference,** such as `7b403c9`, use the full 40-character commit SHA if you meant a commit. Otherwise, point `uses:` at a tag or branch that exists.
* **For a full-length commit SHA,** the commit doesn't exist or isn't reachable in the repository. Pin a commit that exists, or point `uses:` at a tag or branch.

### `invalid-workflow`

**Headline:** Invalid workflow

A workflow file can't be loaded, so none of its actions can be checked. The message gives the reason after `failed to load workflow:` or `could not parse workflow:`. Fix the workflow file, then run `gh actions-lock` again.

### `ref-changed`

**Headline:** Ref changed

A `uses:` reference in a workflow was changed, for example from `v6.2.0` to `v6`, but the lockfile still records the old reference. If you meant to change the reference, run `gh actions-lock` to record it. Otherwise, revert the `uses:` line.

Don't edit the lockfile to match the workflow. The extension rejects lockfile entries that don't match their references.

### `stale`

**Headline:** Unused lockfile entry

The lockfile records a dependency for a workflow that no longer uses it. Run `gh actions-lock` to remove the entry.

### `unreachable-pin`

**Headline:** Pinned commit not in upstream history

The locked commit is no longer in the history of the reference. This happens when:

* The upstream maintainer rewrote history, for example by force-pushing or squashing.
* The tag was moved to an unrelated commit.
* The lockfile entry was edited.

The extension can't tell these apart, so it fails closed. **Don't accept the new commit automatically.**

1. Compare the lockfile with the version on your default branch. If it was edited by hand, restore it. See [Restoring the lockfile](#restoring-the-lockfile).
1. Compare the upstream release notes and history with the commit in your lockfile.
1. If you're satisfied that the new commit is legitimate, accept it and review the diff.

   ```shell
   gh actions-lock --accept-moved
   ```

   `--accept-moved` tells the extension that you've reviewed the upstream history and trust the new commit. Don't use it in automation.

`--accept-moved` applies only to dependencies that your workflow's own `uses:` lines reference, including direct full-length commit SHA pins. If the pin is inside a composite action, the message names that action. Upgrade the composite action, replace it with a version whose pin can be verified, or stop using it.

If the extension suggests a pin, it's the newest release on a branch of the action's repository. If the locked commit isn't on any branch, ask the action's maintainer to tag releases from a branch. See [Using tags for release management](https://docs.github.com/actions/how-tos/create-and-publish-actions/manage-custom-actions#using-tags-for-release-management).

Related headlines:

* **Ref no longer exists:** The tag or branch that the lockfile records was deleted. Point `uses:` at a tag or branch that exists, then run `gh actions-lock`.
* **Not a tag or branch:** The reference isn't a tag or branch name. Point `uses:` at a tag or branch, then run `gh actions-lock`.
* **Ref name refused:** GitHub Actions doesn't accept the reference name. See [Unsupported reference syntax](#unsupported-reference-syntax).

### `tag-commit-mismatch`

**Headline:** Locked commit isn't the tag's commit

A lockfile entry for an exact release tag, such as `v4.2.1`, locks an older commit in the tag's history, not the commit the tag points at. Either the publisher moved the tag, or the lockfile was edited.

1. Check whether the lockfile was edited by hand, and restore it if it was. See [Restoring the lockfile](#restoring-the-lockfile).
1. Look at what changed upstream between the two commits.
1. To lock the tag's current commit, run the following command. `--relock` also fixes an entry that a composite action pins.

   ```shell
   gh actions-lock --relock
   ```

   To use a different release instead, change the `uses:` line and run `gh actions-lock`.

### `misleading-sha`

**Headline:** Misleading SHA

A reference looks like a commit SHA but resolves to a different commit. This usually means a tag or branch was named to look like a SHA. Investigate the action's repository before you use it, then point `uses:` at a release tag you trust and run `gh actions-lock`.

### `repo-replaced`

**Headline:** Repository replaced (identity changed)

The action's name now belongs to a **different** repository from the one you locked. The original repository might have been deleted and its name taken by someone else. Re-locking under the same name won't fix this.

Don't trust the new repository. Find the action's current, verified location, point `uses:` at it, and run `gh actions-lock`. If a composite action uses it, stop using that composite action or upgrade it to a version that doesn't.

### `repo-unavailable`

**Headline:** Repository missing or inaccessible

The extension can't find the action's repository. Either the repository was deleted, or your token can't read it. For a private or internal repository, check that you have access and that your token is authorized for single sign-on if your organization requires it. Then run the command again.

### `incomplete-lockfile`

**Headline:** Lockfile doesn't match locked manifest

A locked action's `action.yml` at its locked commit doesn't match its lockfile entry. Either it uses an action the entry doesn't record, or the action file is missing at that commit. A plain run of `gh actions-lock` doesn't repair this.

* If the entry is missing a dependency, restore the lockfile from version control, or rebuild the entry with the following command. To restore the lockfile, see [Restoring the lockfile](#restoring-the-lockfile).

  ```shell
  gh actions-lock --relock
  ```

* If the action file is missing, point `uses:` at a version of the action that exists at that commit, then run `gh actions-lock --relock`.

### `local-action`

The workflow uses an action from a local path, such as `uses: ./.github/actions/build`.

A `./` path resolves against the runner's workspace, which checkouts, downloads, and scripts can change. The extension never follows or locks a `./` reference. The workflow's other dependencies are still locked and checked.

**When it's an error:** The headline is **local action**. The local action's `action.yml` in your checkout uses other actions. At run time, each of those actions needs a lockfile entry, and the lockfile can't record those entries for a `./` action. Every mode reports this error, including `--verify` and `--verify-local`. The message says:

> PATH runs whatever is in the workspace, so the lockfile can't cover it or the actions it uses; they'll fail when the lockfile is enforced.

Fix it in one of these ways:

* **Lock the action as `$/`.** If the action is in your repository, rewrite `./` references to `$/`, in your workflows and in your repository's action files:

  ```shell
  gh actions-lock --migrate-local-actions
  ```

  A `$/` reference always runs at the workflow's own commit, and the extension locks the actions it uses. Review the diff before you commit. See [Local actions](../README.md#local-actions) and [Self repository actions](../README.md#self-repository-actions-) in the README.

* **Keep `./`.** Report the local action as a warning instead of an error:

  ```shell
  gh actions-lock --ignore-local-actions
  ```

  The actions it uses aren't covered by the lockfile. `--no-migrate-local-actions` is an alias.

If the `./` reference is inside a composite action from another repository, the message names that action and only `--ignore-local-actions` applies.

**When it's a warning:** The headline is **Cannot follow N workspace-local actions**, followed by each workflow and its `./` paths. This is reported when the action file isn't in your checkout, when it has only `run:` steps, or when you pass `--ignore-local-actions`. No action is needed. The local action isn't covered by the lockfile.

### `invalid-self-repository-ref`

**Headline:** invalid self repository ref

A same-repository reference, such as `uses: $/.github/actions/build`, is malformed or points to something that isn't an action. Same-repository references can't have an `@ref` suffix, because they always run at the workflow's own commit. Correct the path and run `gh actions-lock`.

### `reachability-unknown`

**Headline:** Couldn't verify N pins: REASON

GitHub didn't answer whether a locked commit is still valid, or the extension couldn't read a locked action's `action.yml`. This usually means a rate limit or a network error. The finding has `"retryable": true` in `--json` output.

Run the command again at the time the message gives, or once the GitHub API is reachable. For a rate limit, you can also use a token with a higher rate limit. Don't treat the dependency as verified until the check passes. In CI, the extension retries for about two minutes before it reports this.

### `ancestry-unknown`

**Headline:** Couldn't verify N pins: REASON

GitHub didn't answer whether the locked commit is in the reference's history. Fix it the same way as [`reachability-unknown`](#reachability-unknown).

### Unsupported reference syntax

The extension refuses references that GitHub Actions doesn't accept as a tag or branch name, including:

* Git revision syntax, such as `main~1` or `v2^`.
* References shaped like `git describe` output, such as `v1.2.3-4-gabc1234`.
* Full `refs/` names.

Change the `uses:` line to a tag or branch that exists in the action's repository, and run `gh actions-lock` again. If the lockfile already records such a reference, the run exits with code `2` and names the entry. Restore the lockfile. See [Restoring the lockfile](#restoring-the-lockfile).

## Warnings and notices

These findings don't block the run, but you should read them.

### `ref-moved`

A tag or branch that your workflow uses now points to a different commit than the one in the lockfile.

For a branch or a partial version, such as `main` or `v4`, a normal run keeps the locked commit. This warning appears only in `--json` output and doesn't change the exit code. To advance the reference, check what changed upstream between the two commits, then run:

```shell
gh actions-lock --relock
```

**Headline:** Tag now points past locked commit

The extension shows this warning when a `uses:` line pins a full-length commit SHA, and the release tag recorded for it has moved to a later commit. The pinned commit still runs. To follow the tag, change the `uses:` line to the tag, such as `octo-org/deploy-action@v3.1.0`, and run `gh actions-lock`. If a composite action pins it, upgrade that composite action.

An exact release tag that moved for a dependency your workflow references by tag is reported as [`tag-commit-mismatch`](#tag-commit-mismatch) instead.

### `repo-renamed`

**Headline:** WORKFLOW: OLD-NAME was renamed or transferred to NEW-NAME

The action's repository was renamed or transferred, but it's still the same repository, and GitHub Actions follows the redirect. Run `gh actions-lock` to update the `uses:` name. The run reports **Rewrote OLD-NAME → NEW-NAME** and doesn't change the locked commit. If a composite action uses it, upgrade the composite action to a version that uses the new name.

### `sha-as-ref`

**Headline:** N actions are pinned to a bare SHA without a tag ref

A `uses:` line pins a full-length commit SHA instead of a tag. The lockfile still pins the commit, but the workflow doesn't show which release it is. To clear the warning, change the `uses:` line to the release tag for that commit, then run `gh actions-lock`.

### `container-image`

**Headline:** container image NAME is not pinned by digest

The lockfile pins the code of each action, not the container images that a workflow or action runs. An image referenced by a tag, such as `docker://alpine:3`, can change without any change to the lockfile. This applies to `docker://` steps, job `container:` and `services:` images, and a Docker action's `runs.image`.

* For an image in your workflow, reference it by digest, such as `docker://alpine@sha256:DIGEST`.
* For an image that an action uses, ask the action's maintainer to pin it by digest.

Related headlines:

* **container image `NAME` is chosen at runtime, so the lockfile can't check it:** The image comes from an expression. Use a literal image pinned by digest, or pin each value of the matrix or input by digest.
* **ACTION builds its container from NAME; the lockfile doesn't cover its base image:** The action builds from a `Dockerfile`. Ask the action's maintainer to pin its base image by digest.

### `fresh-tag`

A dependency was locked to a recently released version tag. This is a notice. Review the release before you merge.

If your Dependabot configuration sets a `cooldown` for `github-actions`, the notice covers tags released within that cooldown. Otherwise, it covers tags released in the last three days, unless you set a cooldown in `~/.config/gh-actions-lock/config.yml`. Tags without a GitHub release aren't checked. See [Dependabot `cooldown`](https://docs.github.com/code-security/reference/supply-chain-security/dependabot-options-reference#cooldown-).

`gh actions-lock` doesn't hold back a release because of the cooldown setting. It only uses the setting for this notice.

### `cooldown-config-ignored`

Your Dependabot cooldown settings include options that the extension doesn't use, such as `semver-major-days` or `include`, or the Dependabot configuration file can't be parsed. The rest of the run is unaffected.

### `version-ref`

**Headline:** N actions pinned without a full semver tag

A workflow uses a partial version, such as `v4` or `v3.1`. A full version tag, such as `v4.2.1`, names one commit. Consider changing the `uses:` line to a full version tag. When you first lock a workflow, the extension narrows partial versions to full version tags; to keep partial versions, pass `--no-narrow`.

### `stale-workflow`

**Headline:** lockfile records deleted workflow PATH — run `gh actions-lock` to prune it

The lockfile records a workflow file that no longer exists. To remove the entry, run `gh actions-lock` with no workflow paths.

### `onboarding-required`

**Headline:** N onboarding-required entries skipped — re-run without --no-onboard to add them

You ran the extension with `--no-onboard`, and it found a workflow or action that isn't in the lockfile yet. The extension skips it instead of adding it. This is a notice and doesn't change the exit code. To onboard it, run `gh actions-lock` without `--no-onboard`.

## Lockfile is unreadable

The extension can't parse `.github/workflows/actions.lock`, or an entry in it doesn't match its key. For example, the message might say that a `ref` doesn't match the reference in the entry's key. The run exits with code `2` and doesn't change any files.

This usually means the lockfile was changed by a hand edit, a search-and-replace across the repository, or an incorrectly resolved merge conflict.

1. Restore the lockfile. See [Restoring the lockfile](#restoring-the-lockfile).
1. Make your change in the workflow files only.
1. Run `gh actions-lock`, then review and commit the diff.

If the lockfile on your default branch is also unreadable, move it aside and run `gh actions-lock` to lock your workflows again. Every reference resolves again, so review each entry before you commit.

## Lockfile version is newer than the extension

The lockfile's `version` field records the file format. If the lockfile was generated by a newer version of the extension than the one you have, the run exits with code `2`. Upgrade the extension.

```shell
gh extension upgrade gh-actions-lock
```

Don't change the `version` field by hand.

## Restoring the lockfile

To undo changes to the lockfile, restore the last version from your default branch, then regenerate it from your workflows.

```shell
git fetch origin
git checkout origin/DEFAULT-BRANCH -- .github/workflows/actions.lock
gh actions-lock
```

Replace `DEFAULT-BRANCH` with the name of your default branch, such as `main`. Review the diff before you commit. Only changes that follow from your workflow changes should remain.

## Why you shouldn't delete the lockfile

Locked references are sticky. `gh actions-lock` only resolves references that the lockfile doesn't already record. If you delete the lockfile, every reference is resolved again to whatever it points to today:

* A tag that moved, maliciously or not, is accepted without a warning.
* The record of the dependencies you previously reviewed is lost.

To fix a problem, restore the lockfile from version control instead, and let the extension report what changed.

## Getting help

See [SUPPORT.md](../SUPPORT.md). When you report a bug, attach the run log that a failed run prints the path to.
