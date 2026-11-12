#!/usr/bin/env bash
# scripts/mutate-check.sh — Apply temporary mutation, verify test fails, and restore file.
set -eo pipefail

usage() {
  cat <<EOF
Usage: scripts/mutate-check.sh <file> <sed-expr> <go-test-pattern> [package]
       scripts/mutate-check.sh --self-test

Applies a temporary mutation using sed, verifies that the target Go test FAILS,
and restores the original file under all exit conditions.

Arguments:
  file              Source file to mutate
  sed-expr          sed replacement expression (e.g. 's/v == "yes"/v == "mutated"/')
  go-test-pattern   Pattern passed to 'go test -run' (e.g. 'TestDoctorLingerCheck')
  package           Optional Go package (default: .)

Exit codes:
  0  Mutation successfully broke the test (test failed as expected)
  1  Mutation was NOT caught (test still passed)
  2  Usage error or mutation had no effect on file (sed matched 0 lines)
EOF
}

self_test() {
  echo "mutate-check: running self-test..."
  local tmpdir
  tmpdir="$(mktemp -d)"
  trap 'rm -rf "$tmpdir"' EXIT INT TERM HUP

  cat <<'EOF' > "$tmpdir/go.mod"
module selftest
go 1.22
EOF

  cat <<'EOF' > "$tmpdir/calc.go"
package selftest

func Add(a, b int) int {
	return a + b
}
EOF

  cat <<'EOF' > "$tmpdir/calc_test.go"
package selftest

import "testing"

func TestAdd(t *testing.T) {
	if Add(2, 3) != 5 {
		t.Fatalf("Add(2, 3) != 5")
	}
}
EOF

  local orig_content
  orig_content="$(cat "$tmpdir/calc.go")"

  # Case 1: Mutation that breaks the test (a + b -> a - b) -> must return 0
  echo "  [self-test 1] mutation that breaks test..."
  (cd "$tmpdir" && "$OLDPWD/$0" calc.go 's/a + b/a - b/' 'TestAdd' .) >/dev/null 2>&1
  local res=$?
  if [ $res -ne 0 ]; then
    echo "self-test failed: expected 0 for breaking mutation, got $res" >&2
    exit 1
  fi
  if [ "$(cat "$tmpdir/calc.go")" != "$orig_content" ]; then
    echo "self-test failed: calc.go not restored after test 1" >&2
    exit 1
  fi

  # Case 2: Mutation with 0 matches (sed matches nothing) -> must return 2
  echo "  [self-test 2] mutation matching 0 lines..."
  (cd "$tmpdir" && "$OLDPWD/$0" calc.go 's/nonexistent_xyz/dummy/' 'TestAdd' .) >/dev/null 2>&1 || res=$?
  if [ $res -ne 2 ]; then
    echo "self-test failed: expected 2 for 0-line match, got $res" >&2
    exit 1
  fi
  if [ "$(cat "$tmpdir/calc.go")" != "$orig_content" ]; then
    echo "self-test failed: calc.go not restored after test 2" >&2
    exit 1
  fi

  # Case 3: Mutation that test does NOT catch (adds unused comment) -> must return 1
  echo "  [self-test 3] mutation not caught by test..."
  (cd "$tmpdir" && "$OLDPWD/$0" calc.go 's/return a + b/return a + b \/\/ ignored/' 'TestAdd' .) >/dev/null 2>&1 || res=$?
  if [ $res -ne 1 ]; then
    echo "self-test failed: expected 1 for uncaught mutation, got $res" >&2
    exit 1
  fi
  if [ "$(cat "$tmpdir/calc.go")" != "$orig_content" ]; then
    echo "self-test failed: calc.go not restored after test 3" >&2
    exit 1
  fi

  echo "mutate-check: self-test passed (all cases verified)!"
  exit 0
}

if [ "$1" = "--self-test" ]; then
  self_test
fi

if [ "$1" = "-h" ] || [ "$1" = "--help" ] || [ $# -lt 3 ]; then
  usage
  exit 2
fi

FILE="$1"
SED_EXPR="$2"
TEST_PATTERN="$3"
PKG="${4:-./cmd/fleet}"

if [ ! -f "$FILE" ]; then
  echo "mutate-check: ERROR: file '$FILE' not found" >&2
  exit 2
fi

BACKUP="$(mktemp)"
cp -p "$FILE" "$BACKUP"

restore() {
  if [ -f "$BACKUP" ]; then
    cp -p "$BACKUP" "$FILE" 2>/dev/null || true
    rm -f "$BACKUP"
  fi
}
trap restore EXIT INT TERM HUP

MUTATED="$(mktemp)"
sed -e "$SED_EXPR" "$FILE" > "$MUTATED"

if cmp -s "$FILE" "$MUTATED"; then
  echo "mutate-check: ERROR: mutation had no effect on $FILE (sed expression matched 0 lines)" >&2
  rm -f "$MUTATED"
  exit 2
fi

cp -p "$MUTATED" "$FILE"
rm -f "$MUTATED"

echo "mutate-check: applied mutation to $FILE:"
diff -u "$BACKUP" "$FILE" || true
echo "---"
echo "mutate-check: running test: go test -v $PKG -run \"$TEST_PATTERN\""

set +e
go test -v "$PKG" -run "$TEST_PATTERN"
TEST_STATUS=$?
set -e

echo "---"
if [ $TEST_STATUS -eq 0 ]; then
  echo "mutate-check: FAILURE: mutation was NOT caught! Test passed despite mutation." >&2
  exit 1
else
  echo "mutate-check: SUCCESS: test failed as expected (exit code $TEST_STATUS). Test caught the mutation."
  exit 0
fi

