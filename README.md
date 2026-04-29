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

