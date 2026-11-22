#!/usr/bin/env bash
# Says whether a change touches code: prints "docs-only" when every changed path is
# under .claude/ or tasks/ (the workflows, the agents, the plan and the backlog), a
# Markdown file in the repository root, or docs/<name>.md (not deeper), and "code" otherwise.
# It prints "plan" for a docs-only change with nothing under .claude/: the plan, the
# backlog and the docs, which CI does not test at all (owner, 2026-11-22). A change to
# .claude/ stays docs-only, because lint-and-unit parses the workflow scripts.
# CI skips the media tiers for a change confined to those: nothing a media tier runs
# may read .claude/ or tasks/ (except tasks/prd.json in a unit test), and the guard
# test, test/mediaguard, fails when a file no unit test runs names such a path.
# Anything else counts as code, compat/ and .github/ included, and so does a diff that
# can't be read or is empty: when unsure, run.
# Renames are not detected, so a file moved between the accepted places and code
# shows as both paths and counts as code; one moved between accepted places does not.
# Usage: changes.sh <git diff arguments, e.g. base...head>
set -uo pipefail
list=$(mktemp)
trap 'rm -f "$list"' EXIT
git diff --no-renames --name-only -z "$@" >"$list" 2>/dev/null || { echo code; exit 0; }
[[ -s $list ]] || { echo code; exit 0; }
kind=plan
while IFS= read -r -d '' file; do
  case $file in
  .claude/*) kind=docs-only ;;
  tasks/*) ;;
  docs/*/*) echo code; exit 0 ;;
  docs/*.md) ;;
  */*) echo code; exit 0 ;;
  *.md) ;;
  *) echo code; exit 0 ;;
  esac
done <"$list"
echo "$kind"
