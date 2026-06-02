#!/usr/bin/env bash
# Record all VHS demo GIFs. Resets fixtures between each recording.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

RESET="demo/vhs/reset-fixtures.sh"
TAPES=(
  demo/vhs/check-autofix.tape
  demo/vhs/ci-failure.tape
  demo/vhs/upgrade.tape
  demo/vhs/upgrade-version.tape
  demo/vhs/edit-repin.tape
  demo/vhs/tamper-warning.tape
  demo/vhs/sha-as-ref.tape
  demo/vhs/unreleased-commit.tape
)

for tape in "${TAPES[@]}"; do
  echo ""
  echo "=== Resetting fixtures ==="
  bash "$RESET"

  echo "=== Recording: $tape ==="
  vhs "$tape"
  echo "✓ Done: $tape"
done

echo ""
echo "=== All recordings complete ==="
echo "GIFs written to demo/vhs/out/ (gitignored)."
echo "Publish with:"
for tape in "${TAPES[@]}"; do
  gif="${tape##*/}"
  gif="${gif%.tape}.gif"
  echo "  vhs publish demo/vhs/out/$gif"
done
