#!/usr/bin/env bash
# End-to-end npm entry-point test: build the wrapper + the *current* platform
# package from a synthetic goreleaser dist, pack them, install into a temp
# prefix, and run the CLI through node/npx.
#
# This is what CI must exercise (a symlink into npm/platforms proves nothing
# about the published layout or about the launcher being committed). It is also
# required to be idempotent: the tracked tree must be byte-identical afterwards.
set -euo pipefail
cd "$(dirname "$0")/../.."

VERSION=${VERSION:-0.0.0-test}
PLAT=$(node -e 'process.stdout.write(process.platform + "-" + process.arch)')
case "$PLAT" in
  darwin-arm64) goos=darwin; goarch=arm64 ;;
  darwin-x64)   goos=darwin; goarch=amd64 ;;
  linux-x64)    goos=linux;  goarch=amd64 ;;
  linux-arm64)  goos=linux;  goarch=arm64 ;;
  *) echo "unsupported platform $PLAT"; exit 2 ;;
esac

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

before=$(git status --porcelain 2>/dev/null || true)

echo "--- building binary into a synthetic goreleaser dist ---"
mkdir -p "$TMP/dist/vouch_${goos}_${goarch}"
go build -o "$TMP/dist/vouch_${goos}_${goarch}/vouch" ./cmd/vouch

echo "--- staging packages (source tree untouched) ---"
VERSION="$VERSION" DIST="$TMP/dist" STAGE="$TMP/npm" scripts/npm/build.sh --only "$PLAT"

for d in "$TMP/npm/platforms/vouch-$PLAT" "$TMP/npm/vouch"; do
  (cd "$d" && npm pack --pack-destination "$TMP" >/dev/null)
done

echo "--- installing into a clean prefix ---"
mkdir -p "$TMP/proj" && cd "$TMP/proj"
npm init -y >/dev/null 2>&1
npm install --no-audit --no-fund "$TMP"/vouch-*.tgz "$TMP"/vouch-cli-*.tgz >/dev/null

echo "--- running through the wrapper ---"
./node_modules/.bin/vouch version
npx --no-install vouch version

cd - >/dev/null
after=$(git status --porcelain 2>/dev/null || true)
if [[ "$before" != "$after" ]]; then
  echo "error: the roundtrip modified the tracked tree:" >&2
  diff <(printf '%s\n' "$before") <(printf '%s\n' "$after") >&2 || true
  exit 1
fi
echo "npm roundtrip: OK (tree unchanged)"
