#!/usr/bin/env bash
# Prints a git tree hash for the working directory, including untracked files
# and respecting .gitignore. It uses a throwaway index, so it creates no commit
# and leaves the real staging area alone. Compare two with: git diff <a> <b>
set -euo pipefail
index=$(mktemp)
trap 'rm -f "$index"' EXIT
rm -f "$index"
export GIT_INDEX_FILE=$index
git add -A
git write-tree
