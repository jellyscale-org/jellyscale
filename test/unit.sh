#!/usr/bin/env bash
# Runs the tier that needs nothing outside Go (go test ./...) through gotestsum,
# and fails if any test is skipped or the count differs from the sources'.
# JELLYSCALE_LOG_DIR keeps the test results (unit.json) after the run.
set -uo pipefail
cd "$(dirname "$0")/.." || exit 1
results=${JELLYSCALE_LOG_DIR:-}
if [[ -z $results ]]; then
  results=$(mktemp -d)
  trap 'rm -rf "$results"' EXIT
fi
mkdir -p "$results"
go tool gotestsum --format testname --jsonfile "$results/unit.json" -- -count=1 ./...
status=$?
bash test/report.sh go "$results/unit.json" "go test (unit tier)" plain || status=1
exit $status
