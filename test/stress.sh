#!/usr/bin/env bash
# The control-plane stress run: how many concurrent streams one Postgres primary
# carries for jellyscale's control-plane traffic, with no video (ARCHITECTURE.md,
# "Control-plane ceiling"). Not part of any CI tier; it takes about 8 minutes.
#   test/stress.sh <workdir>
# It starts a disposable Postgres on a Docker-assigned port (max_connections 2000,
# because every fake worker holds a connection of its own, as a real one does),
# runs TestControlPlaneLadder (build tag stress) at V = 50 ... 1600 viewers with
# W = V/4 workers, 60 seconds a step, and writes the report to
# <workdir>/stress-report-pool<N>.txt and to stdout.
# JELLYSCALE_STRESS_STEPS=3200,6400 replaces the ladder's numbers of viewers.
# JELLYSCALE_STRESS_POOL=N sizes the pgx pool the viewers' and workers' queries
# share (default: db.DefaultMaxConns); run it for several N to compare.
set -uo pipefail
source "$(dirname "$0")/lib.sh"
[[ $# -eq 1 ]] || { sed -n '2,/^set /p' "$0" | sed '$d' | sed 's/^# \{0,1\}//' >&2; exit 2; }
command -v docker >/dev/null && docker info >/dev/null 2>&1 || { echo "test/stress.sh needs a running Docker daemon for Postgres" >&2; exit 1; }
make_work stress "$1" || exit 1
pg=jellyscale-stress-pg-$$
clean() { docker rm -fv "$pg" >/dev/null 2>&1; }
finish() { clean; rm -rf "$tmp_base" "$work/t"; }
clean; trap finish EXIT
docker run -d --rm --name "$pg" -e POSTGRES_PASSWORD=it -p 127.0.0.1::5432 postgres:18-alpine \
  -c max_connections=2000 >/dev/null || exit 1
port=$(docker port "$pg" 5432/tcp | head -1 | sed 's/.*://')
ready=0
for _ in $(seq 1 120); do
  if docker exec "$pg" psql -h 127.0.0.1 -U postgres -Atc 'select 1' >/dev/null 2>&1 \
    && (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
    ready=$((ready + 1)); [ "$ready" -ge 2 ] && break
  else
    ready=0
  fi
  sleep 0.5
done
[ "$ready" -ge 2 ] || { echo "postgres never became reachable on port $port" >&2; exit 1; }

export JELLYSCALE_TEST_DATABASE_URL="postgres://postgres:it@127.0.0.1:$port/postgres"
export JELLYSCALE_STRESS_CONTAINER=$pg
pool=${JELLYSCALE_STRESS_POOL:-default}
report=$work/stress-report-pool$pool.txt
export JELLYSCALE_STRESS_REPORT=$report.body
cd "$repo" || exit 1
log=$work/stress-run.log
go test -tags stress -count=1 -timeout 60m -v -run '^TestControlPlaneLadder$' ./internal/transcode 2>&1 | tee "$log"
status=${PIPESTATUS[0]}
{
  echo "machine: $(nproc) cores, $(awk '/MemTotal/ {printf "%.0f GB RAM", $2/1048576}' /proc/meminfo), $(grep -m1 'model name' /proc/cpuinfo | sed 's/.*: //')"
  echo "postgres: $(docker exec "$pg" postgres --version), max_connections=2000, pgx pool $pool"
  echo "date: $(date -u +%FT%TZ)"
  if [[ -f $report.body ]]; then
    cat "$report.body"
  else
    # The ladder stopped at a step that could not keep its rate: the steps it
    # finished are in the log, which is the report.
    echo "the run stopped early: $(grep -m1 'stress_test.go.*V=' "$log" | sed 's/^ *//')"
    grep -E '^        ' "$log" | sed 's/^        //'
  fi
} >"$report"
rm -f "$report.body"
cat "$report"
exit $status
