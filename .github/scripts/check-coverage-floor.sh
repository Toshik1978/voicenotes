#!/usr/bin/env bash
#
# Fail when total coverage in a Go coverprofile is below a floor (default 80%).
#
# Shared by `task test:coverage` and .github/workflows/ci.yml so the floor has
# one definition. Usage: check-coverage-floor.sh [profile] [floor-percent]
#
# While the COVERPKG packages do not exist yet, the profile holds only its
# `mode:` header: `go tool cover` then reports `0.0%`, which is "nothing to
# measure", not "nothing covered". That case passes with a notice. The moment
# any statement is instrumented, the floor is enforced.

set -euo pipefail

profile="${1:-coverage.txt}"
floor="${2:-80}"

if [ ! -f "$profile" ]; then
  echo "error: coverage profile $profile not found" >&2
  exit 1
fi

if ! grep -qv '^mode:' "$profile"; then
  echo "coverage: no statements in the gate scope yet; floor of ${floor}% not enforced"
  exit 0
fi

total=$(go tool cover -func="$profile" | awk '/^total:/ {gsub("%", "", $3); print $3}')
if [ -z "$total" ]; then
  echo "error: could not read total coverage from $profile" >&2
  exit 1
fi

if awk -v t="$total" -v f="$floor" 'BEGIN { exit !(t < f) }'; then
  echo "error: coverage ${total}% is below the ${floor}% floor" >&2
  exit 1
fi
echo "coverage: ${total}% (floor ${floor}%)"
