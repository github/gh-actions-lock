#!/usr/bin/env bash
# Reset all demo fixtures to clean state before VHS recording.
# Run this before each tape, or before a full batch recording.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

# --- check-autofix: unpinned workflow ---
cat > demo/workflows-check/ci.yml << 'EOF'
name: CI
on:
  push:
    branches: [main]
  pull_request:

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - run: go test ./...

  lint:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: golangci/golangci-lint-action@v6
        with:
          version: latest
EOF

# --- upgrade: pinned to older versions ---
cat > demo/workflows-upgrade/ci.yml << 'EOF'
name: CI
on:
  push:
    branches: [main]
  pull_request:

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4.2.0
      - uses: actions/setup-go@v5.3.0
        with:
          go-version-file: go.mod
      - run: go test ./...

  lint:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4.2.0
      - uses: golangci/golangci-lint-action@v6.3.0
        with:
          version: latest

# Automatically generated and managed by gh-actions-pin
dependencies:
  - github.com/actions/checkout@v4.2.0:sha1-d631b412a1b64f6193a3f0e66939e2ee43570fdc
  - github.com/actions/setup-go@v5.3.0:sha1-f111f3307d8850f501ac008e886eec1fd1932a34
  - github.com/golangci/golangci-lint-action@v6.3.0:sha1-a4dca80b52c4d4be1eee7deb2e0d6c045e0ef49a
EOF

# --- sha-as-ref: bare SHA, no tag ---
cat > demo/workflows-interactive/sha-as-ref.yml << 'EOF'
name: SHA-as-ref demo
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: nodeselector/actions-test-fixtures/nested-composite@ea53476fdc172d8552df5af9658a45a367e4f41d
EOF

# --- unreleased-commit: SHA not in any release ---
cat > demo/workflows-interactive/unreleased-commit.yml << 'EOF'
name: Unreleased commit demo
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: nodeselector/actions-test-fixtures/nested-composite@dc9920815252718c9bfb9e3ac7d25d4fbf5ca9b1
EOF

echo "✓ All demo fixtures reset"
