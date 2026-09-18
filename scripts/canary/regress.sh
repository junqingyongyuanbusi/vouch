#!/usr/bin/env bash
# VOUCH regression-injection canary.
#
# The verify-level canary (verify.sh) answers "does vouch produce a verdict on
# real repositories". It cannot answer the question that matters: when a real
# regression is present, does vouch actually catch it? A tool that returns
# VERIFIED on every clean repo scores perfectly while detecting nothing.
#
# This script injects a change whose correct verdict is known, one arm at a
# time, and reports the two rates that decide whether the tool works:
#
#   false-clear  regression -> VERIFIED   must be 0; a missed regression is the
#                                         one failure mode with no user-visible
#                                         symptom
#   false-block  harmless   -> BROKEN     erodes trust until the tool is ignored
#
# Usage:
#   scripts/canary/regress.sh [--subset N] [--filter substr] [--fixtures]
#
#     --fixtures  run against the three local testdata fixtures instead of the
#                 real repositories: no network, deterministic, seconds. Use it
#                 to validate the injector itself before spending minutes on
#                 real clones.
#
# Environment: VOUCH_BIN (default: builds ./cmd/vouch into a temp dir),
#              CANARY_CACHE (default: $TMPDIR/vouch-canary-regress)
set -uo pipefail
cd "$(dirname "$0")/../.."

ROOT=$(pwd)
CACHE=${CANARY_CACHE:-${TMPDIR:-/tmp}/vouch-canary-regress}
SUBSET=0
FILTER=""
FIXTURES=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --subset) SUBSET=$2; shift 2 ;;
    --filter) FILTER=$2; shift 2 ;;
    --fixtures) FIXTURES=1; shift ;;
    *) echo "unknown arg $1"; exit 2 ;;
  esac
done

mkdir -p "$CACHE"
# Per-mode results file: a fixture run and a real-repo run share $CACHE (clones
# are expensive to re-fetch), but must not share their result rows — interleaved
# writes from two runs produce a rate table that describes neither.
if [[ $FIXTURES -eq 1 ]]; then
  RESULTS="$CACHE/regress-fixtures.tsv"
else
  RESULTS="$CACHE/regress.tsv"
fi
: > "$RESULTS"

if [[ -n "${VOUCH_BIN:-}" ]]; then
  VOUCH=$VOUCH_BIN
else
  VOUCH=$(mktemp -d)/vouch
  go build -o "$VOUCH" ./cmd/vouch || exit 1
fi
echo "vouch bin: $VOUCH"
echo "cache:     $CACHE"

ARMS=(baseline regression harmless removed)

# assert_sandboxed refuses to mutate anything outside the throwaway cache. The
# injector rewrites source files in place, so a wrong path here would destroy a
# real working tree -- this check runs before every single arm, not once at
# startup, because $dir is recomputed per repository.
assert_sandboxed() {
  local dir=$1
  local real_dir real_cache real_root
  real_dir=$(cd "$dir" 2>/dev/null && pwd -P) || { echo "FATAL: $dir unreadable" >&2; exit 2; }
  real_cache=$(cd "$CACHE" && pwd -P)
  real_root=$(cd "$ROOT" && pwd -P)
  if [[ "$real_dir" == "$real_root" || "$real_dir" == "$real_root"/* ]]; then
    echo "FATAL: refusing to mutate inside the vouch working tree: $real_dir" >&2
    exit 2
  fi
  if [[ "$real_dir" != "$real_cache"/* ]]; then
    echo "FATAL: target is outside the canary cache: $real_dir" >&2
    exit 2
  fi
  if [[ ! -d "$real_dir/.git" ]]; then
    echo "FATAL: target is not a git clone: $real_dir" >&2
    exit 2
  fi
}

# restore returns the clone to pristine HEAD. A failed restore is fatal for the
# repo: continuing would measure the next arm against a polluted tree and
# silently report a number that means nothing.
restore() {
  local dir=$1
  git -C "$dir" reset --hard --quiet 2>/dev/null && git -C "$dir" clean -fdq 2>/dev/null
}

# read_bundle extracts the verdict and the signals each arm is judged on.
read_bundle() {
  python3 - "$1" <<'PY' 2>/dev/null || echo "no-bundle|0|0|"
import json, sys
try:
    d = json.load(open(sys.argv[1]))
except Exception:
    print("no-bundle|0|0|"); raise SystemExit
verdict = d.get("verdict", "unknown")
regressions = removed = 0
for ev in d.get("evidence", []) or []:
    delta = ev.get("delta") or {}
    regressions += len(delta.get("regressions") or [])
    removed += len(delta.get("removed_tests") or [])
claims = d.get("unverified_claims") or []
reasons = ";".join(str(c)[:60] for c in claims[:2])
print(f"{verdict}|{regressions}|{removed}|{reasons}")
PY
}

run_arm() {
  local name=$1 dir=$2 arm=$3
  assert_sandboxed "$dir"

  if ! restore "$dir"; then
    echo "$name	$arm	SKIP	reason=restore-failed" >> "$RESULTS"
    printf '  %-11s %s\n' "$arm" "SKIP (restore failed)"
    return 1
  fi

  local inject_out inject_rc
  inject_out=$(python3 scripts/canary/mutate.py --arm "$arm" --repo "$dir" 2>&1)
  inject_rc=$?
  if [[ $inject_rc -eq 2 ]]; then
    echo "FATAL: injector refused: $inject_out" >&2
    exit 2
  fi
  if [[ $inject_rc -ne 0 ]]; then
    # Unsupported layout: recorded as SKIP and excluded from the rate
    # denominators. Counting it as a pass would inflate the numbers; counting
    # it as a failure would blame vouch for the injector's limits.
    echo "$name	$arm	SKIP	reason=inject-failed" >> "$RESULTS"
    printf '  %-11s %s\n' "$arm" "SKIP (inject failed)"
    return 0
  fi

  local out="$CACHE/$(echo "$name" | tr '/' '_')_$arm.json"
  (cd "$dir" && "$VOUCH" verify --path "$dir" --ci --json --output "$out" >/dev/null 2>&1)
  local exit_code=$?

  local parsed verdict regressions removed reasons
  parsed=$(read_bundle "$out")
  IFS='|' read -r verdict regressions removed reasons <<< "$parsed"

  echo "$name	$arm	$verdict	exit=$exit_code	regressions=$regressions	removed=$removed	reasons=$reasons" >> "$RESULTS"
  printf '  %-11s %-11s exit=%s regressions=%s removed=%s\n' "$arm" "$verdict" "$exit_code" "$regressions" "$removed"
  restore "$dir"
}

run_repo() {
  local name=$1 url=$2 setup=$3
  local dir="$CACHE/$(echo "$name" | tr '/' '-')"

  if [[ ! -d "$dir/.git" ]]; then
    git clone --depth 1 --quiet "$url" "$dir" >/dev/null 2>&1 || {
      echo "$name	-	SKIP	reason=clone-failed" >> "$RESULTS"
      printf '%-26s %s\n' "$name" "SKIP (clone failed)"
      return
    }
  fi

  case "$setup" in
    pytest)
      if [[ ! -x "$dir/.venv/bin/pytest" ]]; then
        # Install the project's own test extras when it declares them: several
        # of these repos need plugins (pytest-httpbin, pytest-mock) that a bare
        # pytest cannot supply, and without them collection fails on both
        # sides -- an environment failure the run would otherwise report as an
        # unverifiable repo.
        (cd "$dir" && python3 -m venv .venv \
          && .venv/bin/pip install --quiet --upgrade pip \
          && .venv/bin/pip install --quiet pytest \
          && { .venv/bin/pip install --quiet -e '.[tests]' \
            || .venv/bin/pip install --quiet -e '.[test]' \
            || .venv/bin/pip install --quiet -e '.[dev]' \
            || .venv/bin/pip install --quiet -e . ; } >/dev/null 2>&1) || true
      fi
      export PATH="$dir/.venv/bin:$PATH"
      ;;
    npm)
      if [[ ! -d "$dir/node_modules" ]]; then
        # Use the package manager the project actually declares. `npm install`
        # against a pnpm workspace leaves a tree its own test script cannot
        # use, and the run then measures a broken environment rather than the
        # tool -- the arms all come back UNVERIFIED and the rate table has no
        # denominator left.
        if [[ -f "$dir/pnpm-lock.yaml" ]] && command -v pnpm >/dev/null 2>&1; then
          (cd "$dir" && pnpm install --silent >/dev/null 2>&1) || true
        elif [[ -f "$dir/yarn.lock" ]] && command -v yarn >/dev/null 2>&1; then
          (cd "$dir" && yarn install --silent >/dev/null 2>&1) || true
        else
          (cd "$dir" && npm install --no-audit --no-fund --silent >/dev/null 2>&1) || true
        fi
      fi
      ;;
    none)
      # Go modules still need their dependencies resolved before `go test` can
      # build; without it both sides fail to compile and nothing is measured.
      if [[ -f "$dir/go.mod" ]]; then
        (cd "$dir" && go mod download >/dev/null 2>&1) || true
      fi
      ;;
  esac

  printf '%s\n' "$name"
  for arm in "${ARMS[@]}"; do
    run_arm "$name" "$dir" "$arm" || break
  done
}

# --- fixture mode: seed local clones from testdata --------------------------

seed_fixtures() {
  local fx
  for fx in go-std py-pytest ts-vitest; do
    local src="$ROOT/testdata/fixtures/$fx"
    local dir="$CACHE/fixture-$fx"
    [[ -d "$src" ]] || continue
    if [[ ! -d "$dir/.git" ]]; then
      mkdir -p "$dir"
      # Copy the fixture's sources, not its installed dependencies: node_modules
      # is a shared tree the scenario tests also use, and the injector must
      # never reach it.
      (cd "$src" && tar -cf - --exclude node_modules --exclude .venv --exclude __pycache__ .) | (cd "$dir" && tar -xf -)
      git -C "$dir" init -q
      git -C "$dir" config user.email canary@vouch.local
      git -C "$dir" config user.name "vouch canary"
      printf 'node_modules\n.venv\n__pycache__\n' > "$dir/.gitignore"
      git -C "$dir" add -A
      git -C "$dir" commit -qm "fixture baseline"
      # Dependencies are copied, never symlinked, and only after the commit so
      # they stay untracked and `git clean -fd` between arms cannot delete them.
      # A symlink here would point the canary's runs at the fixture the
      # scheduler tests also use: vitest rewrites node_modules/.vite on every
      # run, so the injected failure would land in a shared tree. Prefer a
      # copy-on-write clone; fall back to a plain copy when the filesystem
      # cannot clone.
      if [[ -d "$src/node_modules" && ! -e "$dir/node_modules" ]]; then
        # Copy into a staging path and rename into place: `cp -R src dst` means
        # "copy into dst" once dst exists, so a run interrupted midway would
        # leave node_modules/node_modules and every runner missing.
        local staging="$dir/.node_modules.staging"
        rm -rf "$staging"
        if cp -c -R "$src/node_modules" "$staging" 2>/dev/null \
          || cp -a --reflink=always "$src/node_modules" "$staging" 2>/dev/null \
          || cp -R "$src/node_modules" "$staging"; then
          mv "$staging" "$dir/node_modules"
        else
          rm -rf "$staging"
          echo "warning: could not stage node_modules for $fx" >&2
        fi
      fi
    fi
    printf 'fixture/%s\n' "$fx"
    local arm
    for arm in "${ARMS[@]}"; do
      run_arm "fixture/$fx" "$dir" "$arm" || break
    done
  done
}

if [[ $FIXTURES -eq 1 ]]; then
  seed_fixtures
else
  count=0
  while IFS='|' read -r name url setup; do
    [[ -z "${name// }" || "$name" == \#* ]] && continue
    [[ -n "$FILTER" && "$name" != *"$FILTER"* ]] && continue
    count=$((count + 1))
    [[ $SUBSET -gt 0 && $count -gt $SUBSET ]] && break
    run_repo "$name" "$url" "$setup"
  done < scripts/canary/repos.txt
fi

echo
echo "=== summary ==="
python3 - "$RESULTS" <<'PY'
import sys
from collections import defaultdict

rows = defaultdict(dict)
skips = []
for line in open(sys.argv[1]):
    parts = line.rstrip("\n").split("\t")
    if len(parts) < 3:
        continue
    name, arm, verdict = parts[0], parts[1], parts[2]
    fields = {}
    for p in parts[3:]:
        if "=" in p:
            k, v = p.split("=", 1)
            fields[k] = v
    if verdict == "SKIP":
        skips.append((name, arm, fields.get("reason", "?")))
        continue
    rows[name][arm] = (verdict, fields)

# A repo whose untouched baseline is not VERIFIED cannot judge the other arms:
# its tests were already failing, so "BROKEN" says nothing about the injection.
clean, unclean = {}, {}
for name, arms in rows.items():
    base = arms.get("baseline", ("missing", {}))[0]
    (clean if base == "VERIFIED" else unclean)[name] = arms

def cell(arms, arm):
    v, f = arms.get(arm, ("-", {}))
    if arm == "removed" and v != "-":
        return v + ("+warn" if int(f.get("removed", 0)) > 0 else "")
    return v

hdr = f"{'repo':26} {'baseline':11} {'regression':11} {'harmless':11} {'removed':11}"
print(hdr)
print("-" * len(hdr))
for name in sorted(rows):
    arms = rows[name]
    mark = "" if name in clean else "   (baseline not clean)"
    print(f"{name:26} {cell(arms,'baseline'):11} {cell(arms,'regression'):11} "
          f"{cell(arms,'harmless'):11} {cell(arms,'removed'):11}{mark}")

false_clear = [n for n, a in clean.items()
               if a.get("regression", ("-", {}))[0] == "VERIFIED"]
regression_seen = [n for n, a in clean.items() if "regression" in a]
false_block = [n for n, a in clean.items()
               if a.get("harmless", ("-", {}))[0] == "BROKEN"]
harmless_seen = [n for n, a in clean.items() if "harmless" in a]
removed_warned = [n for n, a in clean.items()
                  if int(a.get("removed", ("-", {}))[1].get("removed", 0)) > 0]
removed_seen = [n for n, a in clean.items() if "removed" in a]

unverified = defaultdict(int)
for arms in rows.values():
    for verdict, fields in arms.values():
        if verdict == "UNVERIFIED":
            unverified[(fields.get("reasons") or "unattributed")[:48]] += 1

print()
print(f"false-clear (regression -> VERIFIED): {len(false_clear)}/{len(regression_seen)}   MUST BE 0")
print(f"false-block (harmless  -> BROKEN):    {len(false_block)}/{len(harmless_seen)}")
print(f"coverage drop visible (removed):      {len(removed_warned)}/{len(removed_seen)}")
if unclean:
    print(f"baseline not clean (excluded):        {len(unclean)}  {sorted(unclean)}")
if skips:
    print(f"skipped:                              {len(skips)}")
    for name, arm, reason in skips:
        print(f"    {name} [{arm}] {reason}")
if unverified:
    print("UNVERIFIED attribution:")
    for reason, n in sorted(unverified.items(), key=lambda kv: -kv[1]):
        print(f"    {n}x {reason or '(no reason recorded)'}")

if false_clear:
    print()
    print("FAIL: a regression was reported as VERIFIED -- vouch cleared code it should have blocked:")
    for n in false_clear:
        print(f"    {n}")
    sys.exit(1)
print("GATE: PASS")
PY
