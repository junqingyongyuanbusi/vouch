#!/usr/bin/env bash
# VOUCH verify-level canary (P1.7).
#
# Runs the real `vouch verify` against a fixed set of real repositories and
# reports the P1 acceptance numbers:
#   - zero-config success rate  (a three-state verdict with no UNVERIFIED)
#   - UNVERIFIED rate
#   - incremental P50 wall time (second run on an unchanged repo)
#
# Usage:
#   scripts/canary/verify.sh [--subset N] [--filter substr]
#
# Environment: VOUCH_BIN (default: builds ./cmd/vouch into a temp dir),
#              CANARY_CACHE (default: $TMPDIR/vouch-canary-verify)
set -uo pipefail
cd "$(dirname "$0")/../.."

ROOT=$(pwd)
CACHE=${CANARY_CACHE:-${TMPDIR:-/tmp}/vouch-canary-verify}
SUBSET=0
FILTER=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --subset) SUBSET=$2; shift 2 ;;
    --filter) FILTER=$2; shift 2 ;;
    *) echo "unknown arg $1"; exit 2 ;;
  esac
done

mkdir -p "$CACHE"
RESULTS="$CACHE/results.tsv"
: > "$RESULTS"

if [[ -n "${VOUCH_BIN:-}" ]]; then
  VOUCH=$VOUCH_BIN
else
  VOUCH=$(mktemp -d)/vouch
  go build -o "$VOUCH" ./cmd/vouch || exit 1
fi
echo "vouch bin: $VOUCH"

now_ms() { python3 -c 'import time;print(int(time.time()*1000))'; }

run_repo() {
  local name=$1 url=$2 setup=$3
  local dir="$CACHE/$(echo "$name" | tr '/' '-')"
  local clone_s=0 setup_s=0

  if [[ ! -d "$dir/.git" ]]; then
    local t0 t1
    t0=$(now_ms)
    git clone --depth 1 --quiet "$url" "$dir" >/dev/null 2>&1 || { echo "$name	clone_failed" >> "$RESULTS"; return; }
    t1=$(now_ms); clone_s=$(( (t1 - t0) / 1000 ))
  fi

  case "$setup" in
    pytest)
      if [[ ! -x "$dir/.venv/bin/pytest" ]]; then
        local s0 s1
        s0=$(now_ms)
        (cd "$dir" && python3 -m venv .venv && .venv/bin/pip install --quiet pytest && .venv/bin/pip install --quiet -e . >/dev/null 2>&1) || true
        s1=$(now_ms); setup_s=$(( (s1 - s0) / 1000 ))
      fi
      export PATH="$dir/.venv/bin:$PATH"
      ;;
    npm)
      if [[ ! -d "$dir/node_modules" ]]; then
        local s0 s1
        s0=$(now_ms)
        (cd "$dir" && npm install --no-audit --no-fund --silent >/dev/null 2>&1) || true
        s1=$(now_ms); setup_s=$(( (s1 - s0) / 1000 ))
      fi
      ;;
    none) ;;
  esac

  local out="$CACHE/$(echo "$name" | tr '/' '_').json"
  local t0 t1 first_ms exit_code verdict
  t0=$(now_ms)
  (cd "$dir" && "$VOUCH" verify --path "$dir" --ci --json --output "$out" >/dev/null 2>&1)
  exit_code=$?
  t1=$(now_ms); first_ms=$(( t1 - t0 ))

  verdict=$(python3 - "$out" <<'PY' 2>/dev/null || echo "no-bundle"
import json, sys
try:
    d = json.load(open(sys.argv[1]))
    print(d.get("verdict", "unknown"))
except Exception:
    print("no-bundle")
PY
)

  # incremental run: same repo, unchanged tree (base cache should hit)
  local t2 t3 second_ms
  t2=$(now_ms)
  (cd "$dir" && "$VOUCH" verify --path "$dir" --ci --json --output "$out" >/dev/null 2>&1)
  t3=$(now_ms); second_ms=$(( t3 - t2 ))

  echo "$name	$verdict	exit=$exit_code	first=${first_ms}ms	incremental=${second_ms}ms	clone=${clone_s}s	setup=${setup_s}s" >> "$RESULTS"
  printf '%-24s %-11s exit=%s first=%sms incr=%sms\n' "$name" "$verdict" "$exit_code" "${first_ms}" "${second_ms}"
}

count=0
while IFS='|' read -r name url setup; do
  [[ -z "${name// }" || "$name" == \#* ]] && continue
  [[ -n "$FILTER" && "$name" != *"$FILTER"* ]] && continue
  count=$((count + 1))
  [[ $SUBSET -gt 0 && $count -gt $SUBSET ]] && break
  run_repo "$name" "$url" "$setup"
done < scripts/canary/repos.txt

echo
echo "=== summary ==="
python3 - "$RESULTS" <<'PY'
import sys, statistics

rows = []
for line in open(sys.argv[1]):
    parts = line.rstrip("\n").split("\t")
    if len(parts) < 2:
        continue
    name, verdict = parts[0], parts[1]
    incr = 0
    for p in parts[2:]:
        if p.startswith("incremental="):
            incr = int(p.split("=")[1].rstrip("ms"))
    rows.append((name, verdict, incr))

total = len(rows)
ok = [r for r in rows if r[1] in ("VERIFIED", "BROKEN")]
unverified = [r for r in rows if r[1] == "UNVERIFIED"]
incremental = [r[2] for r in rows if r[2] > 0]

print(f"repos:            {total}")
print(f"three-state ok:   {len(ok)}/{total}  (target >= 7/10)")
if total:
    print(f"UNVERIFIED rate:  {len(unverified)}/{total}  (target <= 20%)")
if incremental:
    print(f"incremental P50:  {int(statistics.median(incremental))}ms  (target <= 60000ms)")
    print(f"incremental max:  {max(incremental)}ms")
for name, verdict, incr in rows:
    print(f"  {name:24} {verdict:11} incr={incr}ms")

gate = len(ok) >= 7 and (not total or len(unverified) / total <= 0.2) and (not incremental or statistics.median(incremental) <= 60000)
print("GATE:", "PASS" if gate else "FAIL")
sys.exit(0 if gate else 1)
PY
