#!/usr/bin/env bash
# Assemble the npm wrapper + platform packages from goreleaser artifacts.
#
#   VERSION=0.1.0 DIST=dist [STAGE=dist/npm] scripts/npm/build.sh [--only <plat>] [--allow-empty] [--publish]
#
# Everything is written into STAGE (default dist/npm), never into the tracked
# npm/ tree: version stamping must not dirty a source checkout, and a publish
# must not depend on a dirty tree either. Fails when a package would ship
# without its binary or without the launcher, so a broken layout can never
# publish an empty wrapper.
set -euo pipefail
cd "$(dirname "$0")/../.."

VERSION=${VERSION:-0.1.0}
DIST=${DIST:-dist}
STAGE=${STAGE:-dist/npm}
ALLOW_EMPTY=0
PUBLISH=0
ONLY=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --allow-empty) ALLOW_EMPTY=1; shift ;;
    --publish) PUBLISH=1; shift ;;
    # Stage a single platform package. Used by the roundtrip test, which can
    # only build the host binary; a release build stages all four and fails if
    # any one is missing.
    --only) ONLY=${2:-}; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done
if [[ -n "$ONLY" && ! -d "npm/platforms/vouch-$ONLY" ]]; then
  echo "unknown platform: $ONLY" >&2; exit 2
fi

command -v npm >/dev/null || { echo "npm is required" >&2; exit 2; }

# The launcher is source; if it is missing the package publishes a wrapper that
# cannot spawn anything (this exact bug was hidden by an unanchored gitignore).
if [[ ! -f npm/vouch/bin/vouch.js ]]; then
  echo "error: npm/vouch/bin/vouch.js is missing; the wrapper has no launcher" >&2
  exit 1
fi

rm -rf "$STAGE"
mkdir -p "$STAGE/platforms"
cp -R npm/vouch "$STAGE/vouch"
for d in npm/platforms/*/; do
  name=$(basename "$d")
  mkdir -p "$STAGE/platforms/$name/bin"
  cp "$d/package.json" "$STAGE/platforms/$name/package.json"
done

# Find the binary for a platform. goreleaser's dist layout has varied across
# versions (<binary>_<goos>_<goarch>/<binary> and variants with _v<version>),
# so resolve by pattern instead of hardcoding one shape.
find_bin() { # <goos> <goarch>
  local goos=$1 goarch=$2
  local hits
  hits=$(find "$DIST" -type f \( -name vouch -o -name 'vouch.exe' \) \
    -not -path "$STAGE/*" -path "*${goos}_${goarch}*" 2>/dev/null | sort | head -1 || true)
  if [[ -z "$hits" && "$goarch" == "amd64" ]]; then
    # older goreleaser templates used x86_64
    hits=$(find "$DIST" -type f -name vouch -not -path "$STAGE/*" \
      -path "*${goos}_x86_64*" 2>/dev/null | sort | head -1 || true)
  fi
  [[ -n "$hits" ]] && echo "$hits"
}

copied=0
for spec in darwin:arm64:darwin-arm64 darwin:amd64:darwin-x64 linux:arm64:linux-arm64 linux:amd64:linux-x64; do
  IFS=: read -r goos goarch plat <<<"$spec"
  if [[ -n "$ONLY" && "$plat" != "$ONLY" ]]; then
    rm -rf "$STAGE/platforms/vouch-$plat"
    continue
  fi
  src=$(find_bin "$goos" "$goarch" || true)
  if [[ -z "$src" ]]; then
    echo "MISSING binary for $plat (looked under $DIST for *${goos}_${goarch}*)"
    continue
  fi
  cp "$src" "$STAGE/platforms/vouch-$plat/bin/vouch"
  chmod +x "$STAGE/platforms/vouch-$plat/bin/vouch"
  echo "packed $plat <- $src"
  copied=$((copied + 1))
done

if [[ $copied -eq 0 && $ALLOW_EMPTY -eq 0 ]]; then
  echo "error: no platform binaries packed; refusing to produce an empty wrapper" >&2
  exit 1
fi

echo "--- stamping version $VERSION ---"
for f in "$STAGE"/vouch/package.json "$STAGE"/platforms/*/package.json; do
  python3 - "$f" "$VERSION" <<'PY'
import json, sys, pathlib
p = pathlib.Path(sys.argv[1])
d = json.loads(p.read_text())
d["version"] = sys.argv[2]
if "optionalDependencies" in d:
    d["optionalDependencies"] = {k: sys.argv[2] for k in d["optionalDependencies"]}
p.write_text(json.dumps(d, indent=2) + "\n")
PY
done

echo "--- validating staged packages ---"
for dir in "$STAGE"/platforms/*/; do
  if [[ $ALLOW_EMPTY -eq 0 && ! -x "$dir/bin/vouch" ]]; then
    echo "error: $(basename "$dir") has no executable bin/vouch" >&2
    exit 1
  fi
done
(cd "$STAGE/vouch" && npm pack --dry-run >/dev/null && echo "npm pack dry-run ok: wrapper")

if [[ $PUBLISH -eq 1 ]]; then
  echo "--- publishing (from $STAGE) ---"
  for d in "$STAGE"/platforms/*; do (cd "$d" && npm publish --access public); done
  (cd "$STAGE/vouch" && npm publish --access public)
else
  echo "staged packages in $STAGE (publish with --publish)"
fi
