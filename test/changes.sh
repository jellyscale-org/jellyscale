#!/usr/bin/env bash
# Says whether a change touches code: prints "docs-only" when every changed file
# is a Markdown file in the repository root or under docs/, or tasks/prd.json (the
# plan; the unit tier checks what reads it), and "code" otherwise.
# CI skips the media shards for a docs-only pull request. Anything else counts as
# code, .claude/ and compat/ included, and so does a diff that can't be read or
# is empty: when unsure, run.
# Usage: changes.sh <git diff arguments, e.g. base...head>
set -uo pipefail
list=$(mktemp)
trap 'rm -f "$list"' EXIT
git diff --no-renames --name-only -z "$@" >"$list" 2>/dev/null || { echo code; exit 0; }
[[ -s $list ]] || { echo code; exit 0; }
while IFS= read -r -d '' file; do
  case $file in
  docs/*.md | tasks/prd.json) ;;
  */*) echo code; exit 0 ;;
  *.md) ;;
  *) echo code; exit 0 ;;
  esac
done <"$list"
echo docs-only
