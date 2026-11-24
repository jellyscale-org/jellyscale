#!/usr/bin/env bash
# Runs every Go test, the integration tier included (build tag integration),
# against disposable Postgres, SeaweedFS and Garage on Docker-assigned ports.
# Needs Docker, curl, and ffmpeg with libx264, libx265 and libass, ffprobe, and a font.
# A test that lacks any of them fails; none is skipped.
# JELLYSCALE_LOG_DIR keeps the test results (tests.json) after the run.
# INTEGRATION_SHARD=i/n runs one slice of the tests of internal/transcode and
# internal/api and INTEGRATION_SHARD=rest every other package (test/shard.sh); CI
# runs them side by side. Without it, or with -run arguments, the whole tier runs.
# INTEGRATION_PROCS=n (n >= 2) runs the shard as n go test processes at once, each over
# its own share of the shard's tests (test/shard.sh) and against a Postgres of its own,
# then the tests of test/alone.txt on their own. INTEGRATION_PROC=k runs only process
# k of them (one Postgres), to reproduce its share by itself.
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
procs=${INTEGRATION_PROCS:-1} proc=${INTEGRATION_PROC:-}
[[ $procs =~ ^[1-9][0-9]*$ ]] || { echo "INTEGRATION_PROCS=$procs is not a whole number of at least 1" >&2; exit 1; }
if [[ $procs -ge 2 && -z $shard ]]; then echo "INTEGRATION_PROCS=$procs needs INTEGRATION_SHARD: processes run a shard's tests" >&2; exit 1; fi
if [[ -n $proc ]]; then
  [[ $procs -ge 2 && $proc =~ ^[0-9]+$ && $proc -lt $procs ]] || { echo "INTEGRATION_PROC=$proc is not a process number (0 to INTEGRATION_PROCS-1, with INTEGRATION_PROCS of 2 or more)" >&2; exit 1; }
fi
make_work integration || exit 1
# Processes side by side, or just one of them, or the shard as a whole in one go.
if [[ -n $proc ]]; then ks=("$proc"); elif [[ $procs -ge 2 ]]; then mapfile -t ks < <(seq 0 $((procs - 1))); else ks=(0); fi
pgs=()
for k in "${ks[@]}"; do pgs+=("jellyscale-it-pg-$$-$k"); done
sw=jellyscale-it-sw-$$ ga=jellyscale-it-garage-$$
clean() { docker rm -fv "${pgs[@]}" "$sw" "$ga" >/dev/null 2>&1; }
finish() {
  local status=$?
  trap '' TERM HUP INT
  for pid in $(jobs -p); do pkill -TERM -P "$pid" 2>/dev/null; kill "$pid" 2>/dev/null; done
  clean; result_finish $status; rm -rf "$work"
}
clean; trap finish EXIT; trap 'exit 143' TERM HUP INT
result_start
if [[ $# -eq 0 ]]; then
  result_name=integration
  if [[ -n $shard ]]; then result_name=integration-${shard/\//of}; fi
  [[ -n $proc ]] && result_name=
fi
# One Postgres serves every package of a process in turn, each test starting
# servers and workers with pools of their own, and a burst of concurrent
# requests on top: the default 100 connections ran out in merge-queue runs
# ("too many clients already", SQLSTATE 53300), failing tests at random.
# A process has a whole Postgres, not a database in a shared one: some tests end
# backends chosen from pg_locks, which is cluster-wide.
for pg in "${pgs[@]}"; do
  docker run -d --rm --name "$pg" -e POSTGRES_PASSWORD=it -p 127.0.0.1::5432 postgres:18-alpine -c max_connections=1000 >/dev/null || exit 1
done
urls=()
for pg in "${pgs[@]}"; do
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
  urls+=("postgres://postgres:it@127.0.0.1:$port/postgres")
done
export JELLYSCALE_TEST_DATABASE_URL=${urls[0]}

source "$repo/test/s3-lib.sh"
start_seaweed "$sw" "$work" conformance || exit 1
start_garage "$ga" "$work" conformance || exit 1
export JELLYSCALE_TEST_S3="seaweedfs=s3://conformance?endpoint=http://127.0.0.1:$seaweed_port&key=$seaweed_key&secret=$seaweed_secret,garage=s3://conformance?endpoint=http://127.0.0.1:$garage_port&region=garage&key=$garage_key&secret=$garage_secret"

# Extra arguments pick tests (-run), so the count of tests is only checked without them.
cd "$repo" || exit 1
results=${JELLYSCALE_LOG_DIR:-$(result_logdir "${result_name:-integration}")}
mkdir -p "$results"
result_log=$results
tier=integration
packages=(./...)
if [[ -n $shard ]]; then
  mapfile -t packages < <(bash test/shard.sh args "$shard") && [[ ${#packages[@]} -gt 0 ]] || { echo "test/shard.sh could not split the tier as $shard" >&2; exit 1; }
  tier=integration:$shard
fi
extra=("$@")
go_test() { # jsonfile, then go test's arguments
  go tool gotestsum --format testname --jsonfile "$1" -- -tags integration -p 1 -count=1 -timeout 45m "${@:2}"
}
# go_runs <dir> <line>...: one go test run for each group of lines, a blank line between two groups
# (test/shard.sh args: one run for each package). Each run has its own test record; <dir>/tests.json is all of them.
go_runs() {
  local dir=$1 run=() k=0 line st=0; shift
  : >"$dir/tests.json"
  for line in "$@" ""; do
    if [[ -n $line ]]; then run+=("$line"); continue; fi
    [[ ${#run[@]} -gt 0 ]] || continue
    go_test "$dir/tests.$k.json" "${run[@]}" "${extra[@]}" || st=1
    cat "$dir/tests.$k.json" >>"$dir/tests.json"; rm -f "$dir/tests.$k.json"
    run=() k=$((k + 1))
  done
  return $st
}
# One go test process, in a subshell of its own: <dir> <database url> <TMPDIR> <file of go test lines>.
run_unit() {
  local dir=$1 url=$2 lines=()
  mapfile -t lines <"$4"
  mkdir -p "$dir" "$3"
  ( export JELLYSCALE_TEST_DATABASE_URL=$url TMPDIR=$3; go_runs "$dir" "${lines[@]}" )
}
unit_report() { # dir, title, tier: the process's own report, printed when it fails
  bash test/report.sh go "$1/tests.json" "$2" "$3" >"$1/report.out" 2>&1
  local st=$?
  [[ $st -eq 0 ]] || cat "$1/report.out"
  return $st
}
status=0
if [[ $procs -ge 2 && -z $proc ]]; then
  for k in "${ks[@]}"; do bash test/shard.sh args "$shard" "$k/$procs" >"$work/args.$k" || exit 1; done
  alone=$(bash test/shard.sh count "$shard" alone) || exit 1
  pids=()
  for k in "${ks[@]}"; do run_unit "$results/p$k" "${urls[$k]}" "$tmp_dir/p$k" "$work/args.$k" & pids+=($!); done
  for k in "${ks[@]}"; do wait "${pids[$k]}" || status=1; done
  # The tests that cannot share the machine run once the other processes have ended.
  if [[ $alone -gt 0 ]]; then
    bash test/shard.sh args "$shard" alone >"$work/args.alone" || exit 1
    run_unit "$results/alone" "${urls[0]}" "$tmp_dir/alone" "$work/args.alone" || status=1
  fi
  : >"$results/tests.json"
  for k in "${ks[@]}"; do
    [[ $# -gt 0 ]] || unit_report "$results/p$k" "go test (integration tier, shard $shard, process $k/$procs)" "$tier $k/$procs" || status=1
    cat "$results/p$k/tests.json" >>"$results/tests.json"; rm -f "$results/p$k/tests.json"
  done
  if [[ $alone -gt 0 ]]; then
    [[ $# -gt 0 ]] || unit_report "$results/alone" "go test (integration tier, shard $shard, alone)" "$tier alone" || status=1
    cat "$results/alone/tests.json" >>"$results/tests.json"; rm -f "$results/alone/tests.json"
  fi
else
  part=; [[ -n $proc ]] && part="$proc/$procs"
  if [[ -n $part ]]; then
    bash test/shard.sh args "$shard" "$part" >"$work/args.0" || exit 1
    tier="$tier $part"
  else
    printf '%s\n' "${packages[@]}" >"$work/args.0"
  fi
  run_unit "$results" "${urls[0]}" "$tmp_dir" "$work/args.0" || status=1
fi
[[ $# -gt 0 ]] && tier=
bash test/report.sh go "$results/tests.json" "go test (integration tier${shard:+, shard $shard}${proc:+, process $proc/$procs})" "$tier" | tee "$work/report.out"
[[ ${PIPESTATUS[0]} -eq 0 ]] || status=1
result_summary=$(sed -n 1p "$work/report.out")
# CI uploads this for the job that adds the shards up (test/check-shards.sh).
if [[ -n $shard && -z $proc && $status -eq 0 ]]; then
  echo "integration $shard $(sed -n '1s/.*: \([0-9]*\) tests;.*/\1/p' "$work/report.out")" >"$results/shard-integration-${shard/\//of}.txt"
fi
exit $status
