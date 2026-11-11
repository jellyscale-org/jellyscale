#!/usr/bin/env bash
# Runs every Go test, the integration tier included (build tag integration),
# against disposable Postgres, SeaweedFS and Garage on Docker-assigned ports.
# Needs Docker, curl, and ffmpeg with libx264, libx265 and libass, ffprobe, and a font.
# A test that lacks any of them fails; none is skipped.
# JELLYSCALE_LOG_DIR keeps the test results (tests.json) after the run.
# INTEGRATION_SHARD=i/n runs one slice of internal/transcode's tests and
# INTEGRATION_SHARD=rest every other package (test/shard.sh); CI runs them side by
# side. Without it, or with -run arguments, the whole tier runs.
# A full run (or a whole shard) records its result under .results (test/lib.sh).
set -uo pipefail
source "$(dirname "$0")/lib.sh"

missing=()
for tool in docker ffmpeg ffprobe fc-list curl; do command -v "$tool" >/dev/null || missing+=("$tool"); done
if [[ ${#missing[@]} -gt 0 ]]; then
  echo "test/integration.sh needs ${missing[*]} on PATH (it needs docker and curl, ffmpeg and ffprobe built with libx264, libx265, libsvtav1 and libass, and fontconfig with a font: fonts-dejavu-core, or font-dejavu on Alpine)" >&2
  exit 1
fi
docker info >/dev/null 2>&1 || { echo "test/integration.sh needs a running Docker daemon for Postgres, SeaweedFS and Garage" >&2; exit 1; }
encoders=$(ffmpeg -hide_banner -encoders 2>/dev/null)
filters=$(ffmpeg -hide_banner -filters 2>/dev/null)
for want in libx264 libx265 libsvtav1; do
  grep -q " $want " <<<"$encoders" || { echo "test/integration.sh needs ffmpeg built with $want" >&2; exit 1; }
done
grep -q " subtitles " <<<"$filters" || { echo "test/integration.sh needs ffmpeg built with libass (the subtitles filter)" >&2; exit 1; }
[[ -n $(fc-list 2>/dev/null) ]] || { echo "test/integration.sh needs a font that fontconfig finds (fc-list lists none): fonts-dejavu-core, or font-dejavu on Alpine" >&2; exit 1; }

shard=${INTEGRATION_SHARD:-}
if [[ -n $shard && $# -gt 0 ]]; then
  echo "test/integration.sh: INTEGRATION_SHARD and arguments for go test can't be combined" >&2
  exit 1
fi
make_work integration || exit 1
pg=jellyscale-it-pg-$$ sw=jellyscale-it-sw-$$ ga=jellyscale-it-garage-$$
clean() { docker rm -fv "$pg" "$sw" "$ga" >/dev/null 2>&1; }
finish() { local status=$?; clean; result_finish $status; rm -rf "$work"; }
clean; trap finish EXIT
result_start
if [[ $# -eq 0 ]]; then result_name=integration; [[ -n $shard ]] && result_name=integration-${shard/\//of}; fi
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

# Extra arguments pick tests (-run), so the count of tests is only checked without them.
cd "$repo" || exit 1
results=${JELLYSCALE_LOG_DIR:-$(result_logdir "${result_name:-integration}")}
mkdir -p "$results"
result_log=$results
packages=(./...)
tier=integration
if [[ -n $shard ]]; then
  mapfile -t packages < <(bash test/shard.sh args "$shard") && [[ ${#packages[@]} -gt 0 ]] || { echo "test/shard.sh could not split the tier as $shard" >&2; exit 1; }
  tier=integration:$shard
fi
go tool gotestsum --format testname --jsonfile "$results/tests.json" -- -tags integration -p 1 -count=1 -timeout 30m "${packages[@]}" "$@"
status=$?
[[ $# -gt 0 ]] && tier=
bash test/report.sh go "$results/tests.json" "go test (integration tier${shard:+, shard $shard})" $tier | tee "$work/report.out"
[[ ${PIPESTATUS[0]} -eq 0 ]] || status=1
result_summary=$(sed -n 1p "$work/report.out")
# CI uploads this for the job that adds the shards up (test/check-shards.sh).
if [[ -n $shard && $status -eq 0 ]]; then
  echo "integration $shard $(sed -n '1s/.*: \([0-9]*\) tests;.*/\1/p' "$work/report.out")" >"$results/shard-integration-${shard/\//of}.txt"
fi
exit $status
