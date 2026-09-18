#!/usr/bin/env bash
# Deterministic agent-loop walkthrough: VERIFIED → injected regression → BROKEN
# (the exact feedback the Stop hook feeds back) → fix → VERIFIED → loop guard.
#
# This is the reproducible evidence behind docs/agent-loop.md. It runs on a
# throwaway copy of the ts-vitest fixture, so the repository is never modified.
set -euo pipefail
cd "$(dirname "$0")/../.."
ROOT=$(pwd)
VOUCH=${VOUCH_BIN:-$ROOT/bin/vouch}
FIXTURE=$ROOT/testdata/fixtures/ts-vitest
[[ -x $VOUCH ]] || { echo "build first: make build (or set VOUCH_BIN)"; exit 2; }
[[ -d $FIXTURE/node_modules ]] || { echo "fixture deps missing: cd $FIXTURE && npm install"; exit 2; }

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
cp -R "$FIXTURE"/. "$WORK/"
ln -sfn "$FIXTURE/node_modules" "$WORK/node_modules"
cd "$WORK"
git init -q . && git add -A && git -c user.email=v@v -c user.name=v commit -qm base

step() { printf '\n== %s ==\n' "$1"; }

step "1. clean change -> expect VERIFIED"
set +e
$VOUCH verify --ci >/dev/null 2>&1; echo "verify exit=$? (0 = VERIFIED)"
set -e

step "2. agent introduces a regression (src/add.ts returns the wrong value)"
cp src/add.ts src/add.ts.bak
python3 - <<'PYEDIT'
import pathlib
p = pathlib.Path("src/add.ts")
src = p.read_text()
patched = src.replace("return a+b", "return a+b+1")
if patched == src:
    raise SystemExit("demo bug: regression was not injected (pattern not found)")
p.write_text(patched)
PYEDIT

step "3. Stop hook fires -> expect exit 2 and blocking feedback on stderr"
set +e
out=$($VOUCH hook claude-code <<< '{"stop_hook_active":false}' 2>&1); code=$?
set -e
printf '%s\n' "$out"
echo "hook exit=$code (2 = block the agent; the text above is what it gets back)"

step "4. agent fixes the cause"
mv src/add.ts.bak src/add.ts

step "5. Stop hook fires again -> expect exit 0 and release"
set +e
out=$($VOUCH hook claude-code <<< '{"stop_hook_active":false}' 2>&1); code=$?
set -e
printf '%s\n' "$out"
echo "hook exit=$code"

step "6. loop guard: the agent is already continuing because of this hook"
set +e
$VOUCH hook claude-code <<< '{"stop_hook_active":true}' >/dev/null 2>&1; code=$?
set -e
echo "hook exit=$code (0 = never ping-pong)"
