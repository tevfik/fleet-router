#!/usr/bin/env bash
# fleet QA harness: the fleet's own models write and judge tests for this repository.
#
#   scripts/selftest.sh tests [pkg…]   tester writes a test file per package, `go test` must pass, validator judges it
#   scripts/selftest.sh hunt  <file…>  hunter reports defects, validator confirms or refutes each one
#
# Needs a running daemon (FLEET_LISTEN / default 127.0.0.1:4711) with the roles tester, validator and hunter.
# Nothing the models write is trusted: a test file survives only if it compiles, passes and the validator
# says it asserts something that can fail. Output: $OUT (default: ./qa-out).
set -uo pipefail
cd "$(dirname "$0")/.."
FLEET=${FLEET:-./fleet}
OUT=${OUT:-qa-out}
TOK=${TOK:-16000}          # thinking models need room: reasoning counts against max tokens
HUNT_TOK=${HUNT_TOK:-40000} # reading a whole file and reasoning about it needs even more
ROUNDS=${ROUNDS:-2}        # repair rounds for a failing generated test
export PATH="$HOME/.local/go/bin:$PATH"
mkdir -p "$OUT"

need() { "$FLEET" status 2>/dev/null | grep -q "^  $1 " || { echo "missing role: $1 (fleet role set $1 <endpoint>, or fleet role init --apply)"; exit 2; }; }

# the last fenced block that is a complete Go file (starts a package) from stdin → stdout.
# Models explain first and show snippets (error messages, fragments) before the real file.
fence() { python3 -c '
import re,sys
t=sys.stdin.read()
blocks=[b for b in re.findall(r"```(?:go|golang)?[ \t]*\n(.*?)```",t,re.S) if re.match(r"\s*(//.*\n\s*)*package \w+",b)]
sys.stdout.write(blocks[-1] if blocks else "")'; }

# Signatures of the other fleet packages the sources use (types, fields, methods) — without them models
# invent hooks and constructors that do not exist. Two levels, so embedded types are covered too.
api_docs() { # $1 = package dir
  python3 - "$1" <<'PY'
import re,subprocess,sys,glob,os
d=sys.argv[1]
src="".join(open(f).read() for f in glob.glob(d+"/*.go") if not f.endswith("_test.go"))
imports={}
for m in re.finditer(r'^\s*(?:(\w+)\s+)?"(github.com/tevfik/fleet-router/internal/[\w/]+)"',src,re.M):
    imports[m.group(1) or m.group(2).split("/")[-1]]=m.group(2)
seen=set(); out=[]
def doc(pkg,sym):
    if (pkg,sym) in seen or len(seen)>40: return ""
    seen.add((pkg,sym))
    r=subprocess.run(["go","doc","-all",pkg,sym],capture_output=True,text=True)
    return r.stdout[:2500]
todo=[]
for alias,path in imports.items():
    for sym in sorted(set(re.findall(r'\b'+alias+r'\.([A-Z]\w*)',src))): todo.append((path,sym))
for level in range(2):
    nxt=[]
    for path,sym in todo:
        t=doc(path,sym)
        if t:
            out.append(t)
            for a,p2 in imports.items(): pass
            for m in re.finditer(r'\b(config|server|discover|client|board|roles)\.([A-Z]\w*)',t):
                nxt.append(("github.com/tevfik/fleet-router/internal/"+m.group(1),m.group(2)))
    todo=nxt
print("\n".join(out))
PY
}

pkg_src() { # non-test sources of a package directory, with file headers
  for f in "$1"/*.go; do
    case "$f" in *_test.go) continue;; esac
    echo "=== $f ==="; cat "$f"
  done
}

cmd_tests() {
  need tester; need validator
  local pkgs=("$@")
  if [ ${#pkgs[@]} -eq 0 ]; then
    while IFS= read -r d; do ls "$d"/*_test.go >/dev/null 2>&1 || pkgs+=("$d"); done < <(find internal -type d | sort)
  fi
  for d in "${pkgs[@]}"; do
    ls "$d"/*.go >/dev/null 2>&1 || continue
    local name pkgname out="$OUT/$(echo "$d" | tr / _)"
    pkgname=$(grep -h '^package ' "$d"/*.go | grep -v _test | head -1 | awk '{print $2}')
    name="$d/zz_fleet_test.go"
    echo "── $d (package $pkgname)"
    pkg_src "$d" > "$out.src"
    printf 'Write a Go test file for the package below. Output ONLY the file in one ```go fence, package %s, using only the standard library.\nTest behaviour that exists in the code; assert specific values; include edge cases and error paths. The test must compile and pass against the code exactly as written.\n\n' "$pkgname" > "$out.prompt"
    cat "$out.src" >> "$out.prompt"
    { echo; echo "=== exact API of the other packages this code uses (use ONLY these names; do not invent hooks, mocks or constructors) ==="; api_docs "$d"; } >> "$out.prompt"
    "$FLEET" ask -n "$TOK" tester - < "$out.prompt" 2> "$out.err" | tee "$out.raw.0" | fence > "$name"
    if [ ! -s "$name" ]; then echo "   REJECTED: the tester returned no code ($(head -c 120 "$out.err"))"; rm -f "$name"; continue; fi
    local ok=0
    for r in $(seq 0 "$ROUNDS"); do
      : > "$out.test"
      if go vet "./$d" > "$out.vet" 2>&1 && go test -count=1 "./$d" > "$out.test" 2>&1; then ok=1; break; fi
      [ "$r" -eq "$ROUNDS" ] && break
      echo "   round $((r+1)): failing, asking the tester to repair"
      { echo "Your test file fails. Fix the TEST (not the code) and output the complete corrected file in one \`\`\`go fence. Standard library only (no go-cmp, testify or other modules); use only names that appear in the sources and API below."; echo; echo "--- errors ---"; head -c 4000 "$out.vet" "$out.test"; echo; echo "--- your file ---"; cat "$name"; echo; echo "--- code under test ---"; cat "$out.src"; } > "$out.prompt"
      "$FLEET" ask -n "$TOK" tester - < "$out.prompt" 2> "$out.err" | tee "$out.raw.$((r+1))" | fence > "$name.new"
      [ -s "$name.new" ] && mv "$name.new" "$name" || { rm -f "$name.new"; break; }
    done
    if [ $ok -ne 1 ]; then echo "   REJECTED: still failing after $ROUNDS repair rounds"; cp "$name" "$out.rejected.go" 2>/dev/null; rm -f "$name"; continue; fi
    # the validator reads the passing test and decides whether it is worth keeping
    { echo "A generated Go test passes. Decide if it is worth keeping. Answer with exactly one first line: VERDICT: KEEP or VERDICT: DROP, then at most 5 lines of reasons."
      echo "DROP if: assertions are trivially true, only check err == nil, restate the implementation, never fail when the code is broken, or encode obviously wrong behaviour as expected."
      echo; echo "--- test ---"; cat "$name"; echo; echo "--- code under test ---"; cat "$out.src"; } > "$out.prompt"
    "$FLEET" ask -n "$TOK" validator - < "$out.prompt" > "$out.verdict" 2>&1
    if grep -qi 'VERDICT: *KEEP' "$out.verdict"; then
      echo "   KEPT   $name ($(grep -c '^func Test' "$name") tests)"
    else
      echo "   DROPPED by the validator: $(grep -i -m1 -A1 VERDICT "$out.verdict" | tr '\n' ' ' | cut -c1-160)"; cp "$name" "$out.dropped.go"; rm -f "$name"
    fi
  done
  echo; go test ./... 2>&1 | grep -v 'no test files' | tail -20
}

cmd_hunt() {
  need hunter; need validator
  for f in "$@"; do
    local out="$OUT/hunt_$(echo "$f" | tr / _)"
    echo "── hunting in $f"
    { echo "Find concrete defects in this file. For each: 'Finding: path:line — defect', 'Trigger:', 'Effect:'. If none, say 'no findings'."; echo; echo "=== $f ==="; cat -n "$f"; } > "$out.prompt"
    "$FLEET" ask -n "$HUNT_TOK" hunter - < "$out.prompt" > "$out.findings" 2>&1
    if grep -q '^error:' "$out.findings"; then echo "   ERROR: the hunter gave no answer ($(grep -m1 '^error:' "$out.findings" | cut -c1-110))"; continue; fi
    if grep -qi 'no findings' "$out.findings" && ! grep -qi '^Finding' "$out.findings"; then echo "   no findings"; continue; fi
    { echo "Check each finding below against the source. For EACH finding output: 'Claim: …', 'Verdict: CONFIRMED|REFUTED|UNCLEAR', 'Evidence: path:line — what the code really does'."; echo; echo "--- findings ---"; cat "$out.findings"; echo; echo "=== $f ==="; cat -n "$f"; } > "$out.prompt"
    "$FLEET" ask -n "$TOK" validator - < "$out.prompt" > "$out.verdicts" 2>&1
    echo "   findings: $(grep -ci '^finding' "$out.findings") · confirmed: $(grep -ci 'verdict: *confirmed' "$out.verdicts") · refuted: $(grep -ci 'verdict: *refuted' "$out.verdicts") (details in $out.verdicts — confirm by reading the code yourself)"
  done
}

case "${1:-}" in
  tests) shift; cmd_tests "$@";;
  hunt)  shift; [ $# -gt 0 ] || { echo "usage: $0 hunt <file…>"; exit 2; }; cmd_hunt "$@";;
  *) echo "usage: $0 tests [pkg…] | hunt <file…>"; exit 2;;
esac
