# Verify actions.lock

Fails the job when `.github/workflows/actions.lock` is out of sync with the workflows, or a locked pin no longer verifies upstream. It downloads a checked `gh-actions-lock` release binary and runs `gh-actions-lock --verify`.

```yaml
steps:
  - uses: actions/checkout@v7.0.1
    with:
      persist-credentials: false
  - uses: github/gh-actions-lock/actions/verify@verify-vX.Y.Z
```

Use the [latest `verify-v*` tag](https://github.com/github/gh-actions-lock/tags). Every CLI release `vX.Y.Z` publishes `verify-vX.Y.Z`, which runs that CLI version by default, so a Dependabot bump of this action is also a CLI bump. Check out the repository before this step.

It's a Node action (`node24`) that uses only Node built-ins, so it runs on the runner's own Node on Linux, macOS and Windows, x64 and arm64. Self-hosted runners need [runner v2.327.1](https://github.com/actions/runner/releases/tag/v2.327.1) or later for `node24` actions. Downloads honor `HTTPS_PROXY`, `HTTP_PROXY` and `NO_PROXY`.

## Inputs

| Input | Default | Description |
|---|---|---|
| `token` | `${{ github.token }}` | Token the CLI uses to read action repositories. |
| `version` | the CLI release the action was stamped with; each `verify-vX.Y.Z` tag uses `vX.Y.Z` | `gh-actions-lock` release to run, such as `v0.5.1`. |
| `args` | `--verify` | Arguments passed to `gh-actions-lock`, split on whitespace and newlines. For example, `--verify --fail-on-warning`. |

With the default `version`, the binary is checked against checksums recorded in the action. With another `version`, it's checked against the release's build attestation on github.com using `token`, which needs the GitHub CLI (`gh`) on `PATH`. GitHub-hosted runners have it; self-hosted runners may not. On GHE.com, that check uses `GH_ACTIONS_LOCK_GITHUB_COM_TOKEN` instead, so your tenant token never leaves the tenant.

## Private and internal actions

The default `GITHUB_TOKEN` can read only the repository the workflow runs in. When a workflow uses a private or internal action from another repository, the runner can still run it if that repository's [Actions access setting](https://docs.github.com/repositories/managing-your-repositorys-settings-and-features/enabling-features-for-your-repository/managing-github-actions-settings-for-a-repository#allowing-access-to-components-in-a-private-repository) allows it, but `--verify` can't read it with `GITHUB_TOKEN` and fails.

To verify those actions, pass a GitHub App installation token with read access to the repositories that hold them:

```yaml
- uses: actions/create-github-app-token@vX
  id: app-token
  with:
    app-id: ${{ vars.LOCKFILE_APP_ID }}
    private-key: ${{ secrets.LOCKFILE_APP_PRIVATE_KEY }}
    owner: ${{ github.repository_owner }}
- uses: github/gh-actions-lock/actions/verify@verify-vX.Y.Z
  with:
    token: ${{ steps.app-token.outputs.token }}
```

The app needs **Contents: read** on this repository and on every repository whose actions your workflows use.

## GitHub Enterprise Cloud with data residency

Set `GH_ACTIONS_LOCK_GITHUB_COM_TOKEN` on the step or job for actions read from github.com; see the [main README](../../README.md#github-enterprise-cloud-with-data-residency).
