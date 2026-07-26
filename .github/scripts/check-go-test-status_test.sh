#!/usr/bin/env bash
# Regression tests for check-go-test-status.sh.
set -uo pipefail

script="$(dirname "$0")/check-go-test-status.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
failures=0

# run <name> <go-test-status> <want-exit>; go test output on stdin.
run() {
  local name="$1" status="$2" want="$3"
  cat > "$tmp/out.txt"
  bash "$script" "$status" "$tmp/out.txt" > /dev/null 2>&1
  local got=$?
  if [ "$got" -ne "$want" ]; then
    echo "FAIL: $name: exit $got, want $want"
    failures=$((failures + 1))
  fi
}

run "clean pass" 0 0 <<'EOF'
ok  	github.com/x/a	0.1s
EOF

run "cleanup flake: access denied on go-build test binary" 1 0 <<'EOF'
ok  	github.com/x/a	0.1s
go: unlinkat C:\Users\RUNNER~1\AppData\Local\Temp\go-build4071066476\b577\service.test.exe: Access is denied.
EOF

run "cleanup flake: go-build test binary in use" 1 0 <<'EOF'
ok  	github.com/x/a	0.1s
go: unlinkat C:\Temp\go-build123\b1\hooks.test.exe: The process cannot access the file because it is being used by another process.
EOF

run "test failure" 1 1 <<'EOF'
--- FAIL: TestX (0.1s)
FAIL	github.com/x/a	0.1s
EOF

run "build failure" 2 2 <<'EOF'
FAIL	github.com/x/a [build failed]
EOF

run "timeout panic" 2 2 <<'EOF'
panic: test timed out after 5m0s
FAIL	github.com/x/a	300s
EOF

run "access denied outside go-build" 1 1 <<'EOF'
ok  	github.com/x/a	0.1s
go: open C:\private\go.mod: Access is denied.
EOF

run "unlinkat non-test file" 1 1 <<'EOF'
ok  	github.com/x/a	0.1s
go: unlinkat C:\repo\important.txt: Access is denied.
EOF

run "unknown nonzero cause" 1 1 <<'EOF'
ok  	github.com/x/a	0.1s
go: some unknown error
EOF

run "failure marker beats cleanup flake" 1 1 <<'EOF'
--- FAIL: TestX (0.1s)
FAIL	github.com/x/a	0.1s
go: unlinkat C:\Temp\go-build123\b1\a.test.exe: Access is denied.
EOF

if [ "$failures" -ne 0 ]; then
  echo "$failures test(s) failed"
  exit 1
fi
echo "all checks passed"
