#!/usr/bin/env bash
# Runs Go tests against disposable Postgres, SeaweedFS and Garage, on
# Docker-assigned ports.
set -uo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
pg=jellyscale-it-pg-$$ sw=jellyscale-it-sw-$$ ga=jellyscale-it-garage-$$
clean() { docker rm -f "$pg" "$sw" "$ga" >/dev/null 2>&1; rm -rf "$work"; }
clean; mkdir -p "$work"; trap clean EXIT
docker run -d --rm --name "$pg" -e POSTGRES_PASSWORD=it -p 127.0.0.1::5432 postgres:18-alpine >/dev/null || exit 1
port=$(docker port "$pg" 5432/tcp | head -1 | sed 's/.*://')
# The image first runs a temporary server that listens on the unix socket only,
# then restarts it on TCP. Wait for TCP, inside the container and then on the
# published port, twice in a row so the restart can't slip between the checks.
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

source "$repo/test/s3-lib.sh"
start_seaweed "$sw" "$work" conformance || exit 1
start_garage "$ga" "$work" conformance || exit 1
export JELLYSCALE_TEST_S3="seaweedfs=s3://conformance?endpoint=http://127.0.0.1:$seaweed_port&key=$seaweed_key&secret=$seaweed_secret,garage=s3://conformance?endpoint=http://127.0.0.1:$garage_port&region=garage&key=$garage_key&secret=$garage_secret"

cd "$repo" && go test -p 1 -count=1 "$@" ./...
