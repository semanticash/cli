#!/usr/bin/env bash
# check-go-test-status.sh <go-test-exit-status> <output-file>
#
# Preserve nonzero `go test` exits except a known Windows cleanup error:
# unlinkat may fail to remove a temporary test binary that is still in use.
# Recognized failure markers take precedence; all other errors fail closed.
set -euo pipefail

status="$1"
output="$2"

if [ "$status" -eq 0 ]; then
  exit 0
fi

if grep -qE '^(--- FAIL:|FAIL|panic:)' "$output"; then
  exit "$status"
fi

if grep -qE '^go: unlinkat .*go-build[^:]*\.test\.exe: (Access is denied\.|.* being used by another process\.)$' "$output"; then
  echo "Ignoring Windows go-build test-binary cleanup error."
  exit 0
fi

echo "go test exited $status without a recognized failure marker; failing closed."
exit "$status"
