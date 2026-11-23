#!/usr/bin/env bash
# test/leaks.sh [<base> <head>] fails when a tracked file, or a commit message in base..head,
# matches a pattern in $LEAK_PATTERNS (extended regexes, one per line). It prints file and
# line only, never the matched text.
set -euo pipefail
cd "$(dirname "$0")/.."
if [ -z "${LEAK_PATTERNS:-}" ]; then
  echo "leaks: LEAK_PATTERNS is empty" >&2
  exit 1
fi
patterns=$(mktemp)
messages=$(mktemp)
trap 'rm -f "$patterns" "$messages"' EXIT
printf '%s\n' "$LEAK_PATTERNS" | grep -v '^[[:space:]]*$' >"$patterns"
found=0
if hits=$(git grep -nIiE -f "$patterns" -- . | cut -d: -f1,2); then
  echo "leaks: a tracked file matches a private pattern:" >&2
  echo "$hits" >&2
  found=1
fi
if [ $# -eq 2 ]; then
  git log --format='%H%n%B' "$1..$2" >"$messages"
  if grep -qiE -f "$patterns" "$messages"; then
    echo "leaks: a commit message in $1..$2 matches a private pattern:" >&2
    git log --format='%H' "$1..$2" | while read -r sha; do
      git log -1 --format='%B' "$sha" | grep -qiE -f "$patterns" && echo "  $sha" >&2
    done
    found=1
  fi
fi
exit $found
