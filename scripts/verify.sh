#!/usr/bin/env bash
# Verifies the machine-checkable criteria in docs/conformance.md.
#
# Each criterion names the Go tests that prove it. This script extracts those
# names, confirms each still exists, and runs them. A criterion whose test has
# been renamed or deleted fails loudly rather than quietly becoming unverified,
# which is the usual way a conformance document rots.
set -uo pipefail
cd "$(dirname "$0")/.." || exit 1

DOC=docs/conformance.md
[ -f "$DOC" ] || { echo "missing $DOC"; exit 1; }

echo "== build ==" && go build ./... || exit 1
echo "== vet =="   && go vet ./...   || exit 1

# Cache the full test inventory once.
inventory=$(go test ./... -list '.*' 2>/dev/null | grep -E '^Test[A-Za-z0-9_]+$' | sort -u)

missing=0
declare -a to_run=()
while IFS= read -r row; do
  id=$(sed -E 's/^\| *(C-[A-Z]+-[0-9]+).*/\1/' <<<"$row")
  while read -r testname; do
    [ -z "$testname" ] && continue
    if ! grep -qx -- "$testname" <<<"$inventory"; then
      echo "MISSING: $id references $testname, which does not exist"
      missing=$((missing+1))
    else
      to_run+=("$testname")
    fi
  done < <(grep -oE '`Test[A-Za-z0-9_]+`' <<<"$row" | tr -d '`')
done < <(grep -E '^\| *C-[A-Z]+-[0-9]+ *\|' "$DOC")

echo "== running ${#to_run[@]} criterion-bound tests =="
if [ ${#to_run[@]} -gt 0 ]; then
  pattern="^($(printf '%s|' "${to_run[@]}" | sed 's/|$//'))\$"
  if go test ./... -run "$pattern" -count=1 >/tmp/jm_verify.log 2>&1; then
    echo "  all criterion-bound tests pass"
  else
    echo "  FAILURES:"; grep -E '^(---|\s+)' /tmp/jm_verify.log | head -20; exit 1
  fi
fi

echo "== full suite =="
if go test ./... -count=1 >/dev/null 2>&1; then echo "  all tests pass"; else echo "  FULL SUITE FAILED"; exit 1; fi

echo
echo "== conformance summary =="
total=0
for s in PASS PARTIAL PENDING MANUAL ASSUMED; do
  n=$(grep -cE "^\|.*\| *$s( —.*)? *\|? *$" "$DOC" || true)
  total=$((total+n))
  printf '  %-8s %3d\n' "$s" "$n"
done
printf '  %-8s %3d\n' "TOTAL" "$total"

if [ "$missing" -gt 0 ]; then
  echo; echo "FAIL: $missing criterion reference(s) point at tests that do not exist"; exit 1
fi
echo; echo "OK: every criterion-bound test exists and passes"
