#!/usr/bin/env bash
# Runs a command with one named mutant of internal/api compiled in, through go's -overlay, so the working tree
# is never touched. Usage: test/mutant.sh <mutant> <command...>, for example
#   test/mutant.sh any-token go test -tags integration -run '^TestAccess$' ./internal/api
#   GOFLAGS is read by every go command the command starts, so test/roles.sh builds the mutant too:
#   test/mutant.sh token-memory test/roles.sh <workdir>
# The mutants are in test/mutant.py. One that no longer applies to the code (its anchor is gone) exits 2 and says so,
# which is a failure of the mutant and never a pass of the test it was meant to fail.
set -euo pipefail
[[ $# -ge 2 ]] || { echo "usage: test/mutant.sh <mutant> <command...>" >&2; exit 2; }
repo=$(cd "$(dirname "$0")/.." && pwd)
name=$1
shift
dir=$(mktemp -d "${TMPDIR:-/tmp}/jellyscale-mutant.XXXXXX")
trap 'rm -rf "$dir"' EXIT
python3 -I "$repo/test/mutant.py" "$name" "$repo" "$dir" || exit 2
GOFLAGS="${GOFLAGS:-} -overlay=$dir/overlay.json" "$@"
