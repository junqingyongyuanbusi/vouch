#!/usr/bin/env bash
# VOUCH canary — P1.1 hard metric: 3 fixtures + 5 real repos → ≥7/8 zero-config.
# Requires network (clones repos). Clones are cached under $TMPDIR/vouch-canary-repos.
set -euo pipefail
cd "$(dirname "$0")/../.."
echo "== vouch canary: real-repo zero-config detection =="
go test -tags canary ./internal/detector -run TestCanary -v -count=1 -timeout 600s
