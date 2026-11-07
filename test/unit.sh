#!/usr/bin/env bash
# Runs the tier that needs nothing outside Go (go test ./...) through gotestsum,
# and fails if any test is skipped or the count differs from the sources'.
# JELLYSCALE_LOG_DIR keeps the test results (unit.json) after the run.
# The result is recorded under .results (test/lib.sh).
set -uo pipefail
source "$(dirname "$0")/lib.sh"
cd "$repo" || exit 1
make_work unit || exit 1
finish() { local status=$?; result_finish $status; rm -rf "$work"; }
trap finish EXIT
result_start
result_name=unit
results=${JELLYSCALE_LOG_DIR:-$(result_logdir unit)}
mkdir -p "$results"
result_log=$results
go tool gotestsum --format testname --jsonfile "$results/unit.json" -- -count=1 ./...
status=$?
bash test/report.sh go "$results/unit.json" "go test (unit tier)" plain | tee "$work/report.out"
[[ ${PIPESTATUS[0]} -eq 0 ]] || status=1
result_summary=$(sed -n 1p "$work/report.out")
exit $status
