# gh-actions-pin

Manage your workflow dependencies.

## Install

```bash
gh extension install github/gh-actions-pin
```

## Usage

Run through the UX feedback loop [here](https://github.com/github/gh-actions-pin/discussions/3).

## Demo

The `demo/` directory contains self-contained fixtures and a helper script for reproducing every scenario in the [design proposal](https://gist.github.com/nodeselector/4ca66e60f1a1760f41bbab3fb84d3d7c):

```bash
demo/try-it.sh                  # list available scenarios
demo/try-it.sh check-autofix    # run one scenario
demo/try-it.sh all              # run all non-interactive scenarios
```

Fixtures are reset to clean state before each run. See `demo/try-it.sh --help` for the full list.

## Related projects

`gh-actions-pin` shares vocabulary with [`zizmor`](https://github.com/zizmorcore/zizmor) for the checks that overlap — `impostor-commit`, `unpinned-uses`, `ref-version-mismatch` — so findings dedup at the code-scanning level and we cite zizmor's published definitions rather than minting our own. For non-pinning audits (template injection, excessive permissions, secret handling, trigger hygiene, cache poisoning), use `zizmor`. The lockfile schema lives in [`actions-lockfile/`](./actions-lockfile/) (staging for the future [`github/actions-lockfile`](https://github.com/github/actions-lockfile) repo).

