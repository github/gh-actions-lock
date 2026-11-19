#!/usr/bin/env bash
# Downloads the gh-actions-lock release binary for this runner and checks it
# before use: against checksums.txt when VERSION is the release this action
# tag was stamped with, otherwise against the release's build attestation.
set -euo pipefail

repo=github/gh-actions-lock
sums="$GITHUB_ACTION_PATH/checksums.txt"
: "${OS:?}" "${ARCH:?}" "${RUNNER_TEMP:?}"

die() {
  echo "::error::$*" >&2
  exit 1
}

if [ -z "${VERSION:-}" ]; then
  die "set the version input, or use a verify-v* release tag of $repo/actions/verify"
fi
[[ "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$ ]] ||
  die "invalid version '$VERSION'; expected vX.Y.Z or vX.Y.Z-rc.N"

case "$OS" in
  Linux) os=linux ext= ;;
  macOS) os=darwin ext= ;;
  Windows) os=windows ext=.exe ;;
  *) die "unsupported runner OS '$OS'" ;;
esac
case "$ARCH" in
  X64) arch=amd64 ;;
  ARM64) arch=arm64 ;;
  *) die "unsupported runner architecture '$ARCH'" ;;
esac
asset="${os}-${arch}${ext}"

dir="$RUNNER_TEMP/gh-actions-lock"
bin="$dir/gh-actions-lock$ext"
mkdir -p "$dir"
curl -fsSL --retry 3 -o "$bin" "https://github.com/$repo/releases/download/$VERSION/$asset"

sha256() {
  if command -v sha256sum >/dev/null; then
    sha256sum "$1" | awk '{ print $1 }'
  else
    shasum -a 256 "$1" | awk '{ print $1 }'
  fi
}

stamped=
[ -f "$sums" ] && stamped="$(awk 'NR == 1 && $1 == "#" { print $2 }' "$sums")"
if [ "$VERSION" = "$stamped" ]; then
  want="$(awk -v a="$asset" '$2 == a { print $1 }' "$sums")"
  [ -n "$want" ] || die "no checksum for $asset in $sums"
  got="$(sha256 "$bin")"
  [ "$got" = "$want" ] || die "checksum mismatch for $asset $VERSION: got $got, want $want"
  echo "Verified $asset $VERSION against the stamped checksum."
else
  GH_HOST=github.com gh attestation verify "$bin" \
    --repo "$repo" \
    --signer-workflow "$repo/.github/workflows/release.yml" \
    --source-ref "refs/tags/$VERSION" >/dev/null ||
    die "attestation check failed for $asset $VERSION"
  echo "Verified $asset $VERSION against its build attestation."
fi

chmod +x "$bin"
echo "bin=$bin" >>"$GITHUB_OUTPUT"
