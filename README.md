# gh-actions-lock

Lock your workflow dependencies.

## Install

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

## How it works

A repo gets a lockfile (located at [`.github/workflows/actions.lock`](https://github.com/github/gh-actions-lock/blob/main/.github/workflows/actions.lock)) and workflows are onboarded to the lockfile on a per-workflow basis. 

Workflows that are onboarded to the lockfile enforce that all dependencies are present in the lockfile and guarantees that the locked commit for an Action is what's executed on the runner. Lockfiles are also verified for forgeries. The sha must exist in the refs it's stated to exist in. Repository identity is recorded and redirects and mismatches are blocked at runtime. 

Finally, locked actions must have a branch that the commit being locked exists within. This is to make impostor commit style attacks harder.

## Limitations

There are currently eligibility limitations for workflows that can be onboarded to lockfiles:
- Workflows in the lockfile cannot use local-path actions, these will be skipped for onboarding. This is also a short-term gap.

