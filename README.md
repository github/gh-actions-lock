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

## CI Integration

### Lockfile lint

A composite action checks that `actions.lock` is in sync with your workflow files. Add it to your CI:

```yaml
jobs:
  lockfile-lint:
    runs-on: ubuntu-latest
    permissions:
      contents: read
    steps:
      - uses: actions/checkout@v4
      - uses: github/gh-actions-lock/actions/lint@main
```

The action installs `gh-actions-lock` and runs a read-only check. It produces GitHub annotations, grouped log output, and a job summary with any findings.

#### Inputs

| Input | Description | Default |
|-------|-------------|---------|
| `extra-flags` | Extra flags passed to `gh actions-lock` (e.g. `--allow-runners foo`) | `""` |
| `ignore-categories` | Comma-separated finding categories to treat as warnings instead of errors (e.g. `local-action`) | `""` |

### Dependabot relock

When Dependabot bumps an action version, the lockfile goes stale. A reusable workflow automatically relocks on Dependabot PRs.

Add a caller workflow to your repo:

```yaml
name: Relock on Dependabot

on:
  pull_request:

permissions:
  contents: write

jobs:
  relock:
    if: github.actor == 'dependabot[bot]'
    uses: github/gh-actions-lock/.github/workflows/relock.yml@main
    with:
      head-ref: ${{ github.head_ref }}
```

The reusable workflow checks out the Dependabot branch, runs `gh actions-lock` with `--no-onboard`, and pushes the updated lockfile.

## Limitations

There are currently eligibility limitations for workflows that can be onboarded to lockfiles:
- Workflows in the lockfile must use a hosted runner. In the meantime, we will skip onboarding self-hosted runners. Discerning a self-hosted runner from a Larger runner requires the `manage_runners:org` which users are unlikely to have on their CLI in the typical case, if workflows are skipped during onboarding, the cli will output the command to run to allow onboarding those workflows. The requirement for hosted runners is enforced at runtime. This is a short-term gap.
- Workflows in the lockfile cannot use local-path actions, these will be skipped for onboarding. This is also a short-term gap.

