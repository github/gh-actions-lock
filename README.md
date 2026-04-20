# gh-actions-pin

Lock and verify GitHub Actions workflow dependencies from the command line.

This extension resolves `uses:` references to their current commit SHAs and
previews the resulting inline `dependencies:` changes by default. Use `--write`
to apply them. It also supports validation, targeted refreshes, ref upgrades,
and lockfile cleanup.

When a workflow's direct action refs have changed since the last lock, the
write path refuses to silently bless those edits by default. Use `upgrade` for
intentional bumps, or pass `--allow-ref-changes` if you really mean it.

## Install

```bash
gh extension install github/gh-actions-pin
```

## Usage

```bash
# Preview all workflows in .github/workflows/
gh actions-pin

# Write the proposed lockfile changes
gh actions-pin --write

# Explicitly acknowledge direct workflow ref edits
gh actions-pin --write --allow-ref-changes

# Preview a specific workflow and inspect the proposed delta
gh actions-pin .github/workflows/ci.yml --diff

# Check the current lock state
gh actions-pin check

# Refresh a targeted action
gh actions-pin update --action actions/checkout

# Upgrade a workflow action to the latest stable tag and repin it
gh actions-pin upgrade --action actions/checkout --write

# Upgrade to a specific version
gh actions-pin upgrade --action actions/checkout --version v5 --write

# Upgrade only checkout refs currently on v5 to v6
gh actions-pin upgrade --action actions/checkout --from v5 --version v6 --write

# Remove stale entries
gh actions-pin tidy
```

## Notes

- with no arguments, the extension discovers workflow files under `.github/workflows/`
- local path actions (`uses: ./path`) are currently skipped
- the default preview prints a compact summary and points you at `--diff` for closer review
- use `--hostname` if you need to target a specific GitHub Enterprise host
