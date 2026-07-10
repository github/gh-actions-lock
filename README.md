# gh-actions-lock

Lock your workflow dependencies.

> [!WARNING]
> **Public preview.** gh-actions-lock is pre-1.0 and under active development. The
> lockfile format, command flags, and behavior may change without notice between
> releases. Use it, file issues, and expect rough edges.

## Background

gh-actions-lock is part of GitHub's Workflow Dependency Pinning effort. It gives repositories a lockfile that pins every workflow dependency to a verified commit, so what runs on the runner is exactly what you locked. Development is ongoing and behavior may still change.

Contributions are welcome. See [CONTRIBUTING.md](./CONTRIBUTING.md) to get started.

## Requirements

Requires the [`gh` CLI](https://cli.github.com/). Install it first, then install the extension:

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

### Self-referencing actions (`$/…`)

`uses: $/…` references an action or reusable workflow in the **same repository** as
the defining file, resolved at the **running commit**. Because it always resolves to
the workflow's own SHA it is **inherently pinned** — no lockfile entry is required,
and it is valid anywhere a relative `./…` reference is:

```yaml
steps:
  - uses: $/actions/my-action          # same-repo action, inherently pinned
jobs:
  call:
    uses: $/.github/workflows/reusable.yml  # same-repo reusable workflow
```

A trailing `@ref` (e.g. `$/actions/my-action@v1`) is rejected — the ref is always
the running commit.

## How it works

A repo gets a lockfile (located at [`.github/workflows/actions.lock`](https://github.com/github/gh-actions-lock/blob/main/.github/workflows/actions.lock)) and workflows are onboarded to the lockfile on a per-workflow basis. 

Workflows that are onboarded to the lockfile enforce that all dependencies are present in the lockfile and guarantees that the locked commit for an Action is what's executed on the runner. Lockfiles are also verified for forgeries. The sha must exist in the refs it's stated to exist in. Repository identity is recorded and redirects and mismatches are blocked at runtime. 

Finally, locked actions must have a branch that the commit being locked exists within. This is to make impostor commit style attacks harder.

## Limitations

There are currently eligibility limitations for workflows that can be onboarded to lockfiles:
- Workflows in the lockfile cannot use local-path actions, these will be skipped for onboarding. This is also a short-term gap.

## License

This project is licensed under the terms of the MIT open source license. See [LICENSE](./LICENSE) for the full terms.

## Maintainers

gh-actions-lock is maintained by @github/actions-dispatch-reviewers. See [CODEOWNERS](./CODEOWNERS).

## Support

Support is best-effort and community-based. Please file bugs and feature requests as [GitHub issues](https://github.com/github/gh-actions-lock/issues). See [SUPPORT.md](./SUPPORT.md) for details.

