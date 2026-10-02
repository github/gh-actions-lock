---
name: actions-lock
description: Keep GitHub Actions dependencies locked whenever workflows or local actions are created, modified, upgraded, or removed.
---

<!--
Example skill. Install as .github/skills/actions-lock/SKILL.md.
See docs/repository-developer-experience.md for the rationale.
-->

When changing files under `.github/workflows/`, or changing an `action.yml` or
`action.yaml` used by those workflows:

1. Generate `.github/workflows/actions.lock` only with the CLI; do not create or
   edit it manually. Manual changes bypass dependency resolution and can leave
   refs, commits, or repository IDs inconsistent. Verification or workflow
   startup can fail, and a later CLI run may overwrite the edits.
2. Ensure the `github/gh-actions-lock` CLI extension is installed. This is safe
   to run when it is already present:

   ```bash
   gh extension install github/gh-actions-lock
   ```

3. After editing workflows or local actions, update the lockfile:

   ```bash
   gh actions-lock --no-interactive
   ```

4. Expect the command to edit source files, not just the lockfile. It narrows
   `uses:` refs to a full semver tag and rewrites same-repo `./…` action
   references to `$/…`. Both are intended; do not revert them.
5. Include every generated workflow, local action, and
   `.github/workflows/actions.lock` change in the resulting change set.
6. Verify the result without modifying files:

   ```bash
   gh actions-lock --verify
   ```

Do not consider the task complete unless verification succeeds. If locking
fails, report the exact finding instead of leaving a stale or incomplete
lockfile. Use the finding's detail and remediation to identify the next step;
do not delete lockfile entries or accept moved pins just to make a check pass.

For `local path actions are not yet supported`, check that each `./…` path
resolves from the repository root to an `action.yml` or `action.yaml`. Fix an
incorrect path and re-run the CLI. If the action is generated, checked out from
another repository, or otherwise unavailable for inspection, defer onboarding
that workflow and report the limitation. Other workflows can still be locked.
If the affected workflow is already onboarded, report the blocking finding
rather than removing its lockfile entry.

Other failures include missing or changed refs, invalid `$/…` paths, unreachable
pins, misleading SHA-shaped refs, and API or authentication failures. See
[Troubleshooting](https://github.com/github/gh-actions-lock/blob/main/docs/repository-developer-experience.md#troubleshooting)
for common findings and recovery steps.

Handling of job-level reusable workflow calls
(`jobs.<id>.uses: owner/repo/.github/workflows/x.yml@ref`) is still under
review. If this repository calls remote reusable workflows, review `git diff` on
the lockfile and report anything that looks wrong. Do not hand-edit the lockfile
to correct it.

Background on how locking is automated in this repository:
`docs/repository-developer-experience.md`.
