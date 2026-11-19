#!/usr/bin/env bash
# Tests of the test scripts themselves, which need nothing but Go, node, git and
# bash: how the shards divide the work and are checked, what counts as a
# docs-only change, the fixture cache with a stand-in ffmpeg, and the results
# and temp dirs the scripts leave. Run by CI beside test/unit.sh.
set -uo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fails=0
check() { # name want got
  if [[ $2 == "$3" ]]; then echo "ok    $1"; else echo "FAIL  $1: want '$2', got '$3'"; fails=$((fails + 1)); fi
}
fails_with() { # name command...: the command must exit non-zero
  local name=$1; shift
  if "$@" >"$tmp/out" 2>&1; then echo "FAIL  $name: succeeded"; fails=$((fails + 1)); else echo "ok    $name"; fi
}

# `go test -list` answers the same until the tests change, and the real one links every test binary (about 5 CPU seconds
# for rest). So the checkout's own listings come from a cache: the first call of each is the real one.
gocache="$tmp/gocache"; mkdir -p "$gocache"
cat >"$gocache/go" <<'SHIM'
#!/bin/bash
if [[ $1 == test && " $* " == *" -list "* && $PWD == "$GOCACHE_REPO" ]]; then
  f=$GOCACHE_DIR/$(printf '%s\0' "$@" | md5sum | cut -c1-32)
  if [[ ! -f $f ]]; then PATH=${PATH#"$GOCACHE_DIR":} "$GOCACHE_REAL" "$@" >"$f.$$" || { cat "$f.$$"; rm -f "$f.$$"; exit 1; }; mv "$f.$$" "$f"; fi
  cat "$f"; exit 0
fi
# go starts go again by name for some commands: not this one.
PATH=${PATH#"$GOCACHE_DIR":} exec "$GOCACHE_REAL" "$@"
SHIM
chmod +x "$gocache/go"
export GOCACHE_REAL=$(go env GOROOT)/bin/go GOCACHE_DIR=$gocache GOCACHE_REPO=$repo
export PATH="$gocache:$PATH"

# --- shard.sh (JS-129): internal/transcode and internal/api are dealt by time, in one list; every test of the
# tier lands in one shard of ci.yml's matrix, and only one. The interface these checks use, which the build gives it:
#   test/shard.sh list <i/n|rest>   one line "<package> <test>" for each test the shard runs, the package relative
#                                   to the module (internal/api), sorted; `count` is the number of those lines
#   test/shard.sh args rest         the package list of rest, as today
#   test/shard.sh args <i/n>        the arguments of the go test runs of a dealt shard, one argument per line, a blank
#                                   line between two runs (one run for each package, so that a test name in both
#                                   packages is run once in each); test/integration.sh runs each with -tags integration
#   test/shard-times.txt            lines "<package> <test> <seconds>" under '#' header lines
# The shards of the matrix, from ci.yml itself:
cd "$repo" || exit 1
matrix=$(python3 -I - .github/workflows/ci.yml <<'EOF'
import sys, yaml
jobs = yaml.safe_load(open(sys.argv[1]))["jobs"]
print(" ".join(e["shard"] for e in jobs["integration-shard"]["strategy"]["matrix"]["include"]))
EOF
)
dealt_shards=$(for s in $matrix; do [[ $s == rest ]] || echo "$s"; done)
# JS-130: the processes a job runs side by side, "<shard> <n>" for each entry of the matrix (0 when it names none).
# The build gives each matrix entry a `procs` and the job `INTEGRATION_PROCS: ${{ matrix.procs }}`.
shard_procs=$(python3 -I - .github/workflows/ci.yml <<'EOF2'
import sys, yaml
for e in yaml.safe_load(open(sys.argv[1]))["jobs"]["integration-shard"]["strategy"]["matrix"]["include"]:
    print(e["shard"], e.get("procs", 0))
EOF2
)
procs_of() { awk -v s="$1" '$1 == s { print $2 }' <<<"$shard_procs"; }
shares_of() { # shard: "<shard> <k>/<m>" for each process of its job, or "<shard>" alone when the job runs one
  local m k; m=$(procs_of "$1")
  if [[ ${m:-0} -ge 2 ]]; then for ((k = 0; k < m; k++)); do echo "$1 $k/$m"; done; else echo "$1"; fi
}
pkg_names() { # go packages...: "<package relative to the module> <test>" for each test go test -list finds
  go test -tags integration -list '.*' "$@" 2>/dev/null | awk '/^ok/ { p = $2; sub(/^jellyscale\//, "", p); for (i = 0; i < n; i++) print p, b[i]; n = 0; next } /^(Test|Example|Fuzz)/ { b[n++] = $1 }' | LC_ALL=C sort
}
all=$(pkg_names ./...)
dealt_pkgs="internal/transcode internal/api"
check "the integration matrix has dealt shards and rest" "yes" "$([[ -n $dealt_shards && $matrix == *rest* ]] && echo yes || echo "no: $matrix")"
in_shards=$(for s in $matrix; do bash test/shard.sh list "$s" | LC_ALL=C sort; done | LC_ALL=C sort)
check "the shards of ci.yml's matrix hold every test of the tier, by package and name" "$all" "$in_shards"
check "no test is in two shards" "" "$(echo "$in_shards" | uniq -d)"
dealt_lines=$(for s in $dealt_shards; do bash test/shard.sh list "$s"; done | LC_ALL=C sort)
want_dealt=$(echo "$all" | grep -E '^(internal/transcode|internal/api) ')
check "the dealt shards hold every test of internal/transcode and internal/api once" "$want_dealt" "$dealt_lines"
rest_lines=$(bash test/shard.sh list rest | LC_ALL=C sort)
check "rest holds every test of the other packages once" "$(echo "$all" | grep -vE '^(internal/transcode|internal/api) ')" "$rest_lines"
rest_packages=$(bash test/shard.sh args rest)
check "rest's package list holds neither dealt package" "0" "$(echo "$rest_packages" | grep -cE '/internal/(transcode|api)$')"
shards_with_api=0
for s in $dealt_shards; do bash test/shard.sh list "$s" | grep -q '^internal/api ' && shards_with_api=$((shards_with_api + 1)); done
check "internal/api's tests are dealt with internal/transcode's, longest first: more than one dealt shard holds some (not one shard keeping it whole)" "yes" "$([[ $shards_with_api -ge 2 ]] && echo yes || echo "$shards_with_api shard(s)")"
counts=0
for s in $matrix; do counts=$((counts + $(bash test/shard.sh count "$s"))); done
check "the shards' counts add up to the tier" "$(echo "$all" | wc -l | tr -d ' ')" "$counts"
for s in $matrix; do check "count $s is the number of tests list $s gives" "$(bash test/shard.sh list "$s" | wc -l | tr -d ' ')" "$(bash test/shard.sh count "$s")"; done
fails_with "a shard past n is refused" bash test/shard.sh count 3/3
check "the split is the same when dealt again" "$(bash test/shard.sh list 2/4)" "$(bash test/shard.sh list 2/4)"
n_dealt=$(echo "$dealt_shards" | head -1 | sed 's|.*/||')
first_dealt=$(echo "$dealt_shards" | head -1)
check "the matrix's dealt shards are i/n for one n, every i" "$(seq 0 $((n_dealt - 1)) | sed "s|\$|/$n_dealt|")" "$(echo "$dealt_shards")"

check "test/shard-times.txt has three fields on every line, a number last" "0" "$(grep -v '^#' test/shard-times.txt | awk 'NF != 3 || $3 !~ /^[0-9]+(\.[0-9]+)?$/' | wc -l | tr -d ' ')"
check "test/shard-times.txt names each test once" "" "$(grep -v '^#' test/shard-times.txt | awk '{ print $1, $2 }' | sort | uniq -d)"
# Its header: made by test/shard-times.sh from at least three CI runs, on CI's runners.
header=$(grep '^#' test/shard-times.txt)
check "the times file's header does not say 'developer machine'" "0" "$(echo "$header" | grep -ci 'developer machine')"
check "... and says the times are from CI's runners" "1" "$(echo "$header" | grep -ciE "CI.s runners" | awk '{print ($1>0)}')"
check "... and names at least three distinct run ids" "yes" "$(echo "$header" | grep -oE '[0-9]{10,}' | sort -u | awk 'END { print (NR >= 3) ? "yes" : "only " NR }')"

# The seconds the times file gives a shard's tests; a test the file lacks counts as its median, as shard.sh deals it.
shard_secs() { # repo-dir times-file shard [share]: a share is "k/m", one process of the job
  local dir=$1 times=$2 shard=$3 share=${4:-}
  SHARD_TIMES=$times bash "$dir/test/shard.sh" list "$shard" $share | awk -v times="$times" '
    BEGIN { while ((getline line < times) > 0) if (line !~ /^#/ && split(line, f, " ") == 3) { t[f[1] " " f[2]] = f[3] + 0; v[++k] = f[3] + 0 } 
      for (a = 2; a <= k; a++) { x = v[a]; b = a - 1; while (b >= 1 && v[b] > x) { v[b + 1] = v[b]; b-- } v[b + 1] = x }
      median = k ? v[int((k + 1) / 2)] : 1 }
    { s += ($0 in t) ? t[$0] : median }
    END { printf "%.1f\n", s }'
}
secs_all=$(for s in $dealt_shards; do shard_secs "$repo" test/shard-times.txt "$s"; done | sort -n)
check "by the file's times the dealt shards are within 1.25 times of each other" "yes" \
  "$(echo "$secs_all" | awk 'NR == 1 { min = $1 } { max = $1 } END { print (max <= 1.25 * min) ? "yes" : "no (" min "s to " max "s)" }')"
# No dealt shard's seconds pass two thirds of go test's -timeout, read from test/integration.sh.
limit_minutes=$(sed -n 's/.*go tool gotestsum.* -timeout \([0-9]*\)m.*/\1/p' test/integration.sh | head -1)
check "test/integration.sh gives go test a -timeout of at least 45m" "yes" "$([[ ${limit_minutes:-0} -ge 45 ]] && echo yes || echo "${limit_minutes:-none}")"
# The alone tests of a job run one after another beside its shares, so a job's test time is its largest share plus
# the seconds of its alone tests (test/alone.txt): both guards count them, or listing tests there would loosen what a job may take.
share_job_secs() { # repo-dir times-file shard [share]
  local secs alone=0
  secs=$(shard_secs "$1" "$2" "$3" "${4:-}")
  [[ -n ${4:-} ]] && alone=$(shard_secs "$1" "$2" "$3" alone)
  awk -v a="$secs" -v b="$alone" 'BEGIN { printf "%.1f\n", a + b }'
}
budget_violations() { # repo-dir times-file limit-seconds shard...: one line for each process share (JS-130, rest too) over two thirds of the limit
  local dir=$1 times=$2 limit=$3 s secs sh share; shift 3
  for s in "$@"; do
    while read -r sh share; do
      secs=$(share_job_secs "$dir" "$times" "$sh" "$share")
      awk -v s="$secs" -v l="$limit" 'BEGIN { exit !(s > l * 2 / 3) }' && echo "shard $sh${share:+ process $share}: ${secs}s is over two thirds of ${limit}s"
    done < <(shares_of "$s")
  done
}
check "no process share's seconds in the times file pass two thirds of go test's -timeout" "" "$(budget_violations "$repo" test/shard-times.txt $((limit_minutes * 60)) $matrix)"
# And none passes 1500 s (25 min): the queue run's 27-minute limit on a shard job less about 2 minutes of job overhead.
cap_violations() { # repo-dir times-file shard...: one line for each process share (JS-130, rest too) over 1500 s
  local dir=$1 times=$2 s secs sh share; shift 2
  for s in "$@"; do
    while read -r sh share; do
      secs=$(share_job_secs "$dir" "$times" "$sh" "$share")
      awk -v s="$secs" 'BEGIN { exit !(s > 1500) }' && echo "shard $sh${share:+ process $share}: ${secs}s is over 1500s"
    done < <(shares_of "$s")
  done
}
check "no process share's seconds in the times file pass 1500 s (rest's too)" "" "$(cap_violations "$repo" test/shard-times.txt $matrix)"

# The same, on a module of its own: two dealt packages with a test name in both, and stand-in times files.
mod="$tmp/mod"
mkdir -p "$mod/internal/transcode" "$mod/internal/api" "$mod/internal/other" "$mod/test"
printf 'module jellyscale\n\ngo 1.22\n' >"$mod/go.mod"
cp test/shard.sh "$mod/test/"
mkgo() { printf 'package %s\n\nimport "testing"\n\n' "$1"; shift; for n in "$@"; do printf 'func %s(t *testing.T) {}\n' "$n"; done; }
mkgo transcode TestA TestB TestSame TestNew >"$mod/internal/transcode/a_test.go"
mkgo api TestC TestSame >"$mod/internal/api/a_test.go"
mkgo other TestO >"$mod/internal/other/a_test.go"
modlist() { (cd "$mod" && SHARD_TIMES="$1" bash test/shard.sh list "$2" | LC_ALL=C sort); }
# What a dealt shard RUNS is what it lists: the go test runs of 'args', in the stand-in module, ran exactly the
# "<package> <test>" pairs 'list' gives, and every pair once over the shards. A -run regex over two packages would run
# a name found in both of them twice in one shard (and out of the other); this shows it. Needs no Docker.
ran() { # times-file shard: the "<package> <test>" pairs that the runs of shard.sh args made
  local times=$1 shard=$2 line out="$tmp/ran-args" run=()
  (cd "$mod" && SHARD_TIMES=$times bash test/shard.sh args "$shard") >"$out" || { echo "args failed"; return; }
  (cd "$mod"; set -f
    flush() { [[ ${#run[@]} -gt 0 ]] && go test -tags integration -count=1 -json "${run[@]}" 2>/dev/null; run=(); }
    { while IFS= read -r line; do if [[ -z $line ]]; then flush; else run+=("$line"); fi; done <"$out"; flush; }
  ) | python3 -I -c '
import json, sys
for l in sys.stdin:
    try: e = json.loads(l)
    except ValueError: continue
    if e.get("Action") == "run" and e.get("Test") and "/" not in e["Test"]: print(e["Package"].removeprefix("jellyscale/"), e["Test"])' | LC_ALL=C sort
}
printf '# stand-in\ninternal/transcode TestA 50\ninternal/api TestC 40\ninternal/transcode TestB 12\ninternal/transcode TestSame 10\ninternal/api TestSame 9\ninternal/transcode TestNew 8\n' >"$tmp/mod-times-run.txt"
for sh in 0/2 1/2; do
  check "shard $sh runs, with go test, exactly the tests it lists (a name in both packages once, in its own package)" "$(modlist "$tmp/mod-times-run.txt" $sh)" "$(ran "$tmp/mod-times-run.txt" $sh)"
done
check "every test of the module ran once over the two shards' go test runs, a shared name once in each package" "internal/api TestC
internal/api TestSame
internal/transcode TestA
internal/transcode TestB
internal/transcode TestNew
internal/transcode TestSame" "$( { ran "$tmp/mod-times-run.txt" 0/2; ran "$tmp/mod-times-run.txt" 1/2; } | LC_ALL=C sort)"
# TestNew is not in this file: it counts as the median and is dealt once.
printf '# stand-in\ninternal/transcode TestA 50\ninternal/api TestC 40\ninternal/transcode TestB 12\ninternal/transcode TestSame 10\ninternal/api TestSame 9\n' >"$tmp/mod-times.txt"
two=$(modlist "$tmp/mod-times.txt" 0/2; modlist "$tmp/mod-times.txt" 1/2)
check "a test name in both dealt packages runs once in each package, in no shard twice" "internal/api TestSame
internal/transcode TestSame" "$(echo "$two" | grep ' TestSame$' | LC_ALL=C sort)"
check "every test of the module's dealt packages is in one of two shards" "internal/api TestC
internal/api TestSame
internal/transcode TestA
internal/transcode TestB
internal/transcode TestNew
internal/transcode TestSame" "$(echo "$two" | LC_ALL=C sort)"
check "a test missing from the times file is dealt to exactly one shard" "1" "$(echo "$two" | grep -c ' TestNew$')"
check "rest of the module holds the other package only" "internal/other TestO" "$(modlist "$tmp/mod-times.txt" rest)"
check "the module's rest package list holds neither dealt package" "jellyscale/internal/other" "$(cd "$mod" && bash test/shard.sh args rest)"
# Every test timed: A 50, C 40, B 12, Same(transcode) 10, Same(api) 9, New 8; longest first, each to the shard with the least so far.
printf '# stand-in\ninternal/transcode TestA 50\ninternal/api TestC 40\ninternal/transcode TestB 12\ninternal/transcode TestSame 10\ninternal/api TestSame 9\ninternal/transcode TestNew 8\n' >"$tmp/mod-times.txt"
check "the deal is longest first, each to the shard with the least so far (shard 0)" "internal/transcode TestA
internal/transcode TestNew
internal/transcode TestSame" "$(modlist "$tmp/mod-times.txt" 0/2)"
check "... (shard 1)" "internal/api TestC
internal/api TestSame
internal/transcode TestB" "$(modlist "$tmp/mod-times.txt" 1/2)"
# A limit of 30 s allows 20 s a shard. A is 20 and the other five 4 each, dealt 20 and 20: exactly two thirds passes.
printf 'internal/transcode TestA 20\ninternal/api TestC 4\ninternal/transcode TestB 4\ninternal/transcode TestSame 4\ninternal/api TestSame 4\ninternal/transcode TestNew 4\n' >"$tmp/mod-times-exact.txt"
check "a shard at exactly two thirds of the limit passes" "" "$(budget_violations "$mod" "$tmp/mod-times-exact.txt" 30 0/2 1/2)"
sed 's/TestA 20/TestA 21/' "$tmp/mod-times-exact.txt" >"$tmp/mod-times-over.txt"
over=$(budget_violations "$mod" "$tmp/mod-times-over.txt" 30 0/2 1/2)
check "a shard over two thirds of the limit fails, naming the shard and its seconds" "shard 0/2: 21.0s is over two thirds of 30s" "$over"
# A test the file lacks counts as its median, never as 0: A and B are 15, the other four count 15 each, dealt 45 and 45. At 0 they would be 15 and 15.
printf 'internal/transcode TestA 15\ninternal/transcode TestB 15\n' >"$tmp/mod-times-median.txt"
check "tests missing from the times file count as the median in the budget, never as 0" "2" "$(budget_violations "$mod" "$tmp/mod-times-median.txt" 30 0/2 1/2 | wc -l | tr -d ' ')"

# --- shard.mjs: each scenario runs once, a worker-stopper last on the last shard.
for i in 0 1 2; do E2E_SHARD=$i/3 node test/e2e/shard_test.mjs >"$tmp/shard$i" 2>&1; done
check "shard 0 runs every third scenario" "run   s1 ran s1 run   s4 ran s4 run   s7 ran s7 shard 0/3: ran 3 of 11" "$(tr '\n' ' ' <"$tmp/shard0" | sed 's/ $//')"
check "shard 2 runs its share, then the worker-stoppers in call order" "run   s3 ran s3 run   s6 ran s6 run   s9 ran s9 run   kill A ran kill A run   kill B ran kill B shard 2/3: ran 5 of 11" "$(tr '\n' ' ' <"$tmp/shard2" | sed 's/ $//')"
check "every scenario ran once over the three shards" "$(printf 'kill A\nkill B\ns1\ns2\ns3\ns4\ns5\ns6\ns7\ns8\ns9\n')" "$(cat "$tmp"/shard? | sed -n 's/^ran //p' | sort)"
check "E2E_ONLY filters after the count" "run   s4 ran s4 shard 0/3: ran 1 of 11" "$(E2E_SHARD=0/3 E2E_ONLY='^s4$' node test/e2e/shard_test.mjs | tr '\n' ' ' | sed 's/ $//')"
fails_with "a shard past n is refused by the gate" env E2E_SHARD=3/3 node test/e2e/shard_test.mjs

# --- player.mjs (JS-099): the e2e helpers wait out a player that reloads its source, in a page that does it in the order CI's failing run saw.
check "the shared player helpers pass test/e2e/player_test.mjs" ok "$(node test/e2e/player_test.mjs >"$tmp/player" 2>&1 && echo ok || tail -20 "$tmp/player")"
check "the recovery helpers pass test/e2e/recovery_test.mjs" ok "$(node test/e2e/recovery_test.mjs >"$tmp/recovery" 2>&1 && echo ok || tail -20 "$tmp/recovery")"
check "run.sh copies player.mjs next to the gates, which import it" 1 "$(grep -c 'test/e2e/player.mjs' test/e2e/run.sh | awk '{print ($1>0)}')"
# The record of JS-099 (docs/testing.md) ties each named failure to what failed.
for line in "reading 'seeking'" "timed out waiting for playback past 22" "Stream ends prematurely" "image_burn_failures_integration_test.go:138" "passed 20 of 20"; do
  check "docs/testing.md records: $line" 1 "$(grep -cF -- "$line" docs/testing.md | awk '{print ($1>0)}')"
done

# --- report.sh lines: a failed scenario is one greppable line on the job summary, without its duration.
printf 'ok    first (1.0s)\nFAIL  second: it broke (2.5s)\n' | GITHUB_STEP_SUMMARY="$tmp/summary" bash test/report.sh lines test/e2e/play.mjs "e2e play" >/dev/null 2>&1
check "report.sh lines writes 'failed: <gate>.<scenario>' for a failure" "failed: e2e play.second: it broke" "$(grep '^failed: ' "$tmp/summary")"

# --- report.sh timings: a run that passed keeps what its tests logged, in the output.
printf '%s\n' \
  '{"Action":"output","Package":"p","Test":"TestMoreWorkersFinishTheFilmSooner","Output":"    helpers_test.go:578: speedup over one worker: 2 workers 1.62x\n"}' \
  '{"Action":"output","Package":"p","Test":"TestMoreWorkersFinishTheFilmSooner","Output":"    helpers_test.go:579: one CPU per physical core: [0 2]\n"}' \
  '{"Action":"pass","Package":"p","Test":"TestMoreWorkersFinishTheFilmSooner","Elapsed":9}' >"$tmp/passed.json"
check "report.sh timings prints a timing a passing test logged, and not a line with none" "timing  TestMoreWorkersFinishTheFilmSooner: speedup over one worker: 2 workers 1.62x" "$(bash test/report.sh timings "$tmp/passed.json")"

# --- check-shards.sh
records="$tmp/records"
mkdir "$records"
echo "integration 0/3 40"  >"$records/a.txt"; echo "integration 1/3 40" >"$records/b.txt"
echo "integration 2/3 40"  >"$records/c.txt"; echo "integration rest 30" >"$records/d.txt"
bash test/check-shards.sh integration "$records" 150 >/dev/null 2>&1; check "integration shards that add up pass" 0 $?
fails_with "a test no shard ran fails" bash test/check-shards.sh integration "$records" 151
rm "$records/d.txt"
fails_with "a missing shard fails" bash test/check-shards.sh integration "$records" 150
echo "integration rest 0" >"$records/d.txt"
fails_with "a shard that ran nothing fails" bash test/check-shards.sh integration "$records" 120
rm -r "$records"; mkdir "$records"
printf 'browse 0 1 7 7\n' >"$records/a.txt"; printf 'transcode 0 2 20 41\n' >"$records/b.txt"; printf 'transcode 1 2 21 41\n' >"$records/c.txt"
bash test/check-shards.sh e2e "$records" "browse transcode" >/dev/null 2>&1; check "e2e shards that add up pass" 0 $?
printf 'transcode 1 2 20 41\n' >"$records/c.txt"
fails_with "a scenario no shard ran fails" bash test/check-shards.sh e2e "$records" "browse transcode"
printf 'transcode 1 2 21 42\n' >"$records/c.txt"
fails_with "shards that disagree on the total fail" bash test/check-shards.sh e2e "$records" "browse transcode"
printf 'transcode 0 2 41 41\n' >"$records/b.txt"; printf 'transcode 1 2 0 41\n' >"$records/c.txt"
fails_with "a shard that ran nothing fails" bash test/check-shards.sh e2e "$records" "browse transcode"
rm "$records/c.txt"
fails_with "a shard with no record fails" bash test/check-shards.sh e2e "$records" "browse transcode"
fails_with "an empty directory fails" bash test/check-shards.sh e2e "$tmp" "browse"
printf 'browse 0 1 7 7\n' >"$records/a.txt"; printf 'transcode 0 1 41 41\n' >"$records/b.txt"; rm -f "$records/c.txt"
bash test/check-shards.sh e2e "$records" "browse transcode" >/dev/null 2>&1; check "every expected gate present passes" 0 $?
fails_with "a whole gate that no shard ran fails" bash test/check-shards.sh e2e "$records" "browse play transcode"
printf 'browse 0 1 7 7\n\n' >"$records/a.txt"
fails_with "a blank or malformed record line fails" bash test/check-shards.sh e2e "$records" "browse transcode"

# --- the differential tier is a gate of the required e2e check, and documented.
printf 'browse 0 1 7 7\n' >"$records/a.txt"; rm -f "$records/b.txt" "$records/c.txt"
fails_with "a differential gate that no job ran fails" bash test/check-shards.sh e2e "$records" "browse differential"
printf 'differential 0 1 30 30\n' >"$records/b.txt"
bash test/check-shards.sh e2e "$records" "browse differential" >/dev/null 2>&1; check "a differential record that adds up passes" 0 $?
printf 'differential 0 1 0 30\n' >"$records/b.txt"
fails_with "a differential run that checked nothing fails" bash test/check-shards.sh e2e "$records" "browse differential"
check "CI runs test/differential/run.sh in an e2e shard" 1 "$(grep -c 'test/differential/run.sh' .github/workflows/ci.yml | awk '{print ($1>=1)}')"
check "the required e2e check expects the differential gate" 1 "$(grep -c 'check-shards.sh e2e shards ".*differential' .github/workflows/ci.yml)"
for word in test/differential/run.sh test/differential/requests.yaml compat/differences.yaml; do
  check "docs/testing.md explains $word" 1 "$(grep -c -F "$word" docs/testing.md | awk '{print ($1>=1)}')"
done

# --- allowlist entries are scoped to requests; the acceptance run puts four bugs back.
check "docs/testing.md tells how to scope an allowlist entry to a request" 1 "$(grep -c -E 'requests:' docs/testing.md | awk '{print ($1>=1)}')"
check "compat/differences.yaml scopes entries by request, not route" 0 "$(grep -c -E '^- route:|^  route:' compat/differences.yaml)"
check "acceptance.sh runs four broken builds" 4 "$(grep -c '^mutate ' test/differential/acceptance.sh)"

# --- a gate's last line, as run.sh checks it.
printf 'ok    a\n\nPASS\nshard 1/3: ran 4 of 12\n' >"$tmp/gate"
check "a gate's shard line is read" "1 3 4 12" "$(bash test/check-shards.sh line "$tmp/gate")"
printf 'ok    a\n\nPASS\n' >"$tmp/gate"
fails_with "a gate that printed no shard line fails" bash test/check-shards.sh line "$tmp/gate"
printf 'FAILED\nshard 0/3: ran 0 of 12\n' >"$tmp/gate"
fails_with "a shard that ran nothing fails" bash test/check-shards.sh line "$tmp/gate"
check "with E2E_ONLY, a gate where nothing matched passes" "0 3 0 12" "$(E2E_ONLY=x bash test/check-shards.sh line "$tmp/gate")"
printf 'shard 0/3: ran 2 of 12\nmore output\n' >"$tmp/gate"
fails_with "a shard line that is not the last line fails" bash test/check-shards.sh line "$tmp/gate"
fails_with "a missing file fails" bash test/check-shards.sh line "$tmp/none"

# --- changes.sh (JS-132): docs-only means every path is under .claude/ or tasks/, a Markdown file in the root, or docs/*.md.
g="$tmp/git"
git init -q "$g" && cd "$g" || exit 1
git config user.email t@t && git config user.name t
mkdir -p docs .claude/agents .claude/workflows compat src tasks internal/tasks docs/.claude .claude-old
echo 1 >tasks/prd.json && echo 1 >tasks/other.json && echo 1 >tasks/backlog.json && echo 1 >README.md && echo 1 >docs/a.md && echo 1 >docs/a.txt
echo 1 >src/a.go && echo 1 >src/b.go && echo 1 >.claude/agents/qa.md && echo 1 >.claude/workflows/build.js && echo 1 >compat/x.md
echo 1 >.claude-old/x && echo 1 >.claudeignore && echo 1 >tasks.json && echo 1 >internal/tasks/x.go && echo 1 >docs/.claude/x && mkdir -p docs/sub && echo 1 >docs/sub/x.md
mkdir -p .github/workflows && echo 1 >.github/workflows/ci.yml
git add -A && git commit -qm base
# try <name> <want> <command...>: runs the command in the repository, asks changes.sh about it, then puts the tree back.
try() {
  local name=$1 want=$2; shift 2
  "$@"
  git add -A && git commit -qm change
  check "changes.sh: $name" "$want" "$(bash "$repo/test/changes.sh" HEAD~1 HEAD)"
  git reset -q --hard HEAD~1 && git clean -fdq
}
edit() { local f; for f in "$@"; do echo 2 >>"$f"; done; }
try "a root .md and docs/*.md are docs-only" docs-only edit README.md docs/a.md
try "tasks/prd.json is docs-only" docs-only edit tasks/prd.json
try "another file under tasks/ is docs-only" docs-only edit tasks/other.json
try "tasks/backlog.json alone is docs-only" docs-only edit tasks/backlog.json
try "a .md under .claude/ is docs-only" docs-only edit .claude/agents/qa.md
try "a .js under .claude/ is docs-only" docs-only edit .claude/workflows/build.js
try "a new file of any type under .claude/ and tasks/ is docs-only" docs-only bash -c 'mkdir -p .claude/deep/er tasks/a && echo 1 >.claude/deep/er/x.yaml && echo 1 >tasks/a/b.bin'
try ".claude/, tasks/, a root .md and docs/*.md together are docs-only" docs-only edit .claude/workflows/build.js tasks/backlog.json README.md docs/a.md
try "a Go file with a doc is code" code edit README.md src/a.go
try "a Go file with .claude/ and tasks/ is code" code edit .claude/agents/qa.md tasks/backlog.json src/a.go
try "compat/x.md is code" code edit compat/x.md
try "docs/a.txt is code" code edit docs/a.txt
try "docs/sub/x.md is code" code edit docs/sub/x.md
try "docs/a.txt with .claude/ is code" code edit docs/a.txt .claude/agents/qa.md
try ".github/workflows/ci.yml is code" code edit .github/workflows/ci.yml
try ".github/workflows/ci.yml with tasks/ is code" code edit .github/workflows/ci.yml tasks/backlog.json
try ".claude-old/x is code" code edit .claude-old/x
try "a root .claudeignore is code" code edit .claudeignore
try "a root tasks.json is code" code edit tasks.json
try "internal/tasks/x.go is code" code edit internal/tasks/x.go
try "docs/.claude/x is code" code edit docs/.claude/x
try "a Go file renamed into .claude/ is code" code git mv src/a.go .claude/a.go
try "a Go file renamed into tasks/ is code" code git mv src/a.go tasks/a.go
try "a file moved out of .claude/ to a code path is code" code git mv .claude/agents/qa.md src/qa.go
try "a file moved out of tasks/ to a code path is code" code git mv tasks/other.json src/other.json
try "a file moved out of .claude/ to a root .md is docs-only (both paths are accepted kinds; no rename detection is asked for)" docs-only git mv .claude/agents/qa.md moved.md
try "a deleted Go file is code" code git rm -q src/a.go
try "a deleted file under .claude/ is docs-only" docs-only git rm -q .claude/agents/qa.md
try "a rename inside .claude/ is docs-only" docs-only git mv .claude/agents/qa.md .claude/agents/qa2.md
try "a rename from .claude/ to tasks/ is docs-only" docs-only git mv .claude/agents/qa.md tasks/qa.md
try "a rename from tasks/ to .claude/ is docs-only" docs-only git mv tasks/other.json .claude/other.json
try "a deletion under tasks/ with a rename under .claude/ is docs-only" docs-only bash -c 'git rm -q tasks/other.json && git mv .claude/agents/qa.md .claude/qa.md'
try "a renamed root .md that becomes .txt is code" code git mv README.md moved.txt
try "code renamed into docs/ is code" code git mv src/a.go docs/b.md
check "an empty diff is code" code "$(bash "$repo/test/changes.sh" HEAD HEAD)"
check "an unreadable diff is code" code "$(bash "$repo/test/changes.sh" nosuchbase...HEAD)"
cd "$repo" || exit 1

# --- ci.yml (JS-132): the changes job on a change confined to .claude/ and tasks/. Its script is run for each event.
cj="$tmp/changes-job"
mkdir -p "$cj/repo" && cd "$cj/repo" || exit 1
git init -q . && git config user.email t@t && git config user.name t
mkdir -p .claude/workflows tasks src && echo 1 >.claude/workflows/build.js && echo 1 >tasks/backlog.json && echo 1 >src/a.go
git add -A && git commit -qm base && base=$(git rev-parse HEAD)
echo 2 >>.claude/workflows/build.js && echo 2 >>tasks/backlog.json && git commit -qam claude-only && claude_only=$(git rev-parse HEAD)
echo 2 >>src/a.go && git commit -qam code && with_code=$(git rev-parse HEAD)
python3 -I - "$repo/.github/workflows/ci.yml" "$repo/test/changes.sh" >"$cj/run.sh" <<'EOF'
import sys, yaml
steps = yaml.safe_load(open(sys.argv[1]))["jobs"]["changes"]["steps"]
print([s for s in steps if s.get("id") == "diff"][0]["run"].replace("test/changes.sh", sys.argv[2]))
EOF
changes_job() { # event base head full: prints the job's outputs on one line
  : >"$cj/out"
  GITHUB_EVENT_NAME=$1 GITHUB_OUTPUT=$cj/out PR_BASE=$2 PR_HEAD=$3 QUEUE_BASE=$2 QUEUE_HEAD=$3 FULL=$4 bash "$cj/run.sh" >/dev/null 2>&1
  tr '\n' ' ' <"$cj/out"
}
names() { grep -oE '\.claude/|tasks/' <<<"$1" | sort -u | wc -l | tr -d ' '; }
out=$(changes_job merge_group "$base" "$claude_only" false)
check "ci.yml: a merge group of .claude/ and tasks/ skips the media tiers, naming both" "code=false 2" "$(grep -o 'code=false' <<<"$out") $(names "$out")"
check "ci.yml: ... and gives not the old reason" "0" "$(grep -c 'only docs or tasks/prd.json changed' <<<"$out")"
out=$(changes_job pull_request "$base" "$claude_only" true)
check "ci.yml: a ci:full pull request of .claude/ and tasks/ skips the media tiers, naming both" "code=false 2" "$(grep -o 'code=false' <<<"$out") $(names "$out")"
out=$(changes_job merge_group "$base" "$with_code" false)
check "ci.yml: a merge group with code runs the media tiers" "code=true" "$(grep -o 'code=true' <<<"$out")"
out=$(changes_job pull_request "$base" "$with_code" true)
check "ci.yml: a ci:full pull request with code runs the media tiers" "code=true" "$(grep -o 'code=true' <<<"$out")"
out=$(changes_job pull_request "$base" "$claude_only" false)
check "ci.yml: a pull request without ci:full says the media tiers run in the merge queue" "code=false 1" "$(grep -o 'code=false' <<<"$out") $(grep -c 'a pull request: the media tiers run in the merge queue' <<<"$out")"
out=$(changes_job push "$base" "$with_code" false)
check "ci.yml: a push to main says it was tested in its merge group" "code=false 1" "$(grep -o 'code=false' <<<"$out") $(grep -c 'a push to main, tested in its merge group' <<<"$out")"
cd "$repo" || exit 1
js132=$(python3 -I - .github/workflows/ci.yml <<'EOF'
import sys, yaml
jobs = yaml.safe_load(open(sys.argv[1]))["jobs"]
lu = jobs["lint-and-unit"]
print("lint-and-unit has no needs or if" if "needs" not in lu and "if" not in lu else "lint-and-unit has needs or if")
want = "needs.changes.outputs.code == 'true'"
for j in ("integration-total", "integration-shard", "roles", "e2e-shard"):
    print(f"{j} still waits for changes and runs only for code" if jobs[j].get("needs") == "changes" and jobs[j].get("if") == want else f"{j} changed")
print("at most five shards" if len(jobs["integration-shard"]["strategy"]["matrix"]["include"]) <= 5 else "matrix has more than five entries")
print("cancel-media-jobs kept" if "cancel-media-jobs" in jobs else "cancel-media-jobs gone")
print("integration says why the shards did not run" if "did not run: $WHY" in str(jobs["integration"]["steps"]) else "integration lost its reason")
print("e2e says why the shards did not run" if "did not run: $WHY" in str(jobs["e2e"]["steps"]) else "e2e lost its reason")
EOF
)
check "ci.yml: lint-and-unit runs on every change" "lint-and-unit has no needs or if" "$(sed -n 1p <<<"$js132")"
for j in integration-total integration-shard roles e2e-shard; do
  check "ci.yml: $j is gated on changes as before" "$j still waits for changes and runs only for code" "$(grep "^$j " <<<"$js132")"
done
check "ci.yml: the dealt shards and cancel-media-jobs stay" "at most five shards cancel-media-jobs kept" "$(grep -E '^(at most|cancel-media)' <<<"$js132" | paste -sd' ')"
check "ci.yml: integration and e2e still say why the shards did not run" "2" "$(grep -c 'says why the shards did not run' <<<"$js132")"
head_ci=$(sed -n '/^jobs:/q;p' .github/workflows/ci.yml)
head_ch=$(sed -n '/^set -uo/q;p' test/changes.sh)
check "ci.yml's top comment gives the rule for .claude/ and tasks/ and names the guard test" "yes" "$(grep -q '\.claude/' <<<"$head_ci" && grep -q 'tasks/' <<<"$head_ci" && grep -qi 'guard' <<<"$head_ci" && echo yes || echo no)"
check "changes.sh's top comment gives the rule for .claude/ and tasks/ and names the guard test" "yes" "$(grep -q '\.claude/' <<<"$head_ch" && grep -q 'tasks/' <<<"$head_ch" && grep -qi 'guard' <<<"$head_ch" && echo yes || echo no)"

# --- fixtures: cached.sh with a stand-in ffmpeg that writes a file and logs the call.
shim="$tmp/shim"
mkdir "$shim"
cat >"$shim/ffmpeg" <<'E1'
#!/bin/sh
[ "$1" = -version ] && { echo "ffmpeg version stand-in"; exit 0; }
# FAKE_FFMPEG_FAIL_AFTER=n makes the n+1st call and the ones after fail, as a killed run would stop.
[ -n "$FAKE_FFMPEG_FAIL_AFTER" ] && [ "$(wc -l <"$FAKE_FFMPEG_LOG")" -ge "$FAKE_FFMPEG_FAIL_AFTER" ] && exit 1
[ -n "$FAKE_FFMPEG_HANG" ] && [ "$(wc -l <"$FAKE_FFMPEG_LOG")" -ge "$FAKE_FFMPEG_HANG" ] && exec sleep 5
echo "$*" >>"$FAKE_FFMPEG_LOG"
for last; do :; done
case $last in
  # An MP4 is the smallest one that has an HEVC track (an empty mdat, an hvcC box with no content),
  # which test/fixtures/dovi writes a Dolby Vision box into.
  *.mp4) echo AAAAFGZ0eXBpc29tAAACAGlzb20AAAAIbWRhdAAAAK1tb292AAAApXRyYWsAAACdbWRpYQAAAJVtaW5mAAAAjXN0YmwAAACFc3RzZAAAAAAAAAABAAAAdWh2YzEAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAfaHZjQwAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA | base64 -d >"$last" ;;
  # The bytes the Cues recipe cuts at, near the end.
  *) printf 'xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\034S\273kzz' >"$last" ;;
esac
E1
echo font >"$tmp/stand-in.ttf"
printf '#!/bin/sh\necho %s\n' "$tmp/stand-in.ttf" >"$shim/fc-match"
chmod +x "$shim"/*
export FAKE_FFMPEG_FAIL_AFTER= FAKE_FFMPEG_LOG="$tmp/ffmpeg.log" E2E_FIXTURE_CACHE="$tmp/cache" TMPDIR="$tmp"
: >"$FAKE_FFMPEG_LOG"
FAKE_FFMPEG_FAIL_AFTER=3 PATH="$shim:$PATH" bash test/fixtures/cached.sh "$tmp/w0" >/dev/null 2>&1
check "a run that stops partway fails" 1 "$([[ $? -ne 0 ]] && echo 1 || echo 0)"
check "and leaves nothing in the cache" "" "$(ls -A "$tmp/cache")"
: >"$FAKE_FFMPEG_LOG"
FAKE_FFMPEG_HANG=2 PATH="$shim:$PATH" bash test/fixtures/cached.sh "$tmp/wk" >/dev/null 2>&1 &
hung=$!
for _ in $(seq 50); do [[ $(wc -l <"$FAKE_FFMPEG_LOG") -ge 2 ]] && break; sleep 0.1; done
kill -9 $hung; wait $hung 2>/dev/null
check "a run killed during generation leaves nothing in the cache" "" "$(ls -A "$tmp/cache")"
mkdir "$tmp/cache/.tmp.old" && touch -d '5 hours ago' "$tmp/cache/.tmp.old"
: >"$FAKE_FFMPEG_LOG"
FAKE_FFMPEG_FAIL_AFTER= PATH="$shim:$PATH" bash test/fixtures/cached.sh "$tmp/w1" >/dev/null 2>&1; check "the first run makes the fixtures" 0 $?
first=$(wc -l <"$FAKE_FFMPEG_LOG")
check "the first run called ffmpeg" 1 "$(( first > 0 ))"
check "the films reach the workdir" 1 "$([[ -f "$tmp/w1/media/transcode/Zeta Interlaced (2050)/Zeta Interlaced (2050).mkv" && -f "$tmp/w1/media/sources/Epsilon Film (2022).mkv" ]] && echo 1 || echo 0)"
PATH="$shim:$PATH" bash test/fixtures/cached.sh "$tmp/w2" >/dev/null 2>&1; check "the second run succeeds" 0 $?
check "the second run makes no ffmpeg call" "$first" "$(wc -l <"$FAKE_FFMPEG_LOG")"
check "the second run's workdir has the films" "$(cd "$tmp/w1/media" && find . -type f | sort | md5sum)" "$(cd "$tmp/w2/media" && find . -type f | sort | md5sum)"
check "a stale half entry is swept" 0 "$([[ -e $tmp/cache/.tmp.old ]] && echo 1 || echo 0)"
check "the cache holds the one entry, whole" "$(PATH="$shim:$PATH" bash test/fixtures/key.sh)" "$(ls -A "$tmp/cache")"
key=$(PATH="$shim:$PATH" bash test/fixtures/key.sh)
mkdir "$tmp/cache/oldkey" && touch -d '20 days ago' "$tmp/cache/oldkey" "$tmp/cache/$key"
: >"$FAKE_FFMPEG_LOG"
PATH="$shim:$PATH" bash test/fixtures/cached.sh "$tmp/w3" >/dev/null 2>&1
check "an old entry for the current key (as CI restores it) is used, not swept" 0 "$(wc -l <"$FAKE_FFMPEG_LOG")"
check "an old entry for another key is swept" "$key" "$(ls -A "$tmp/cache")"
mkdir -p "$tmp/copy/test" "$tmp/copy/internal" && cp -r test/fixtures "$tmp/copy/test/" && cp -r internal/testenv "$tmp/copy/internal/"
check "a copy of the recipes has the same key" "$key" "$(PATH="$shim:$PATH" bash "$tmp/copy/test/fixtures/key.sh")"
echo "# changed" >>"$tmp/copy/test/fixtures/make.sh"
check "editing a recipe changes the key" 1 "$([[ $(PATH="$shim:$PATH" bash "$tmp/copy/test/fixtures/key.sh") != "$key" ]] && echo 1 || echo 0)"
echo other >"$tmp/stand-in.ttf"
check "another font changes the key" 1 "$([[ $(PATH="$shim:$PATH" bash test/fixtures/key.sh) != "$key" ]] && echo 1 || echo 0)"
echo font >"$tmp/stand-in.ttf"
printf '#!/bin/sh\necho "ffmpeg version other"\n' >"$tmp/ffmpeg2"; mkdir "$tmp/shim2"; mv "$tmp/ffmpeg2" "$tmp/shim2/ffmpeg"; chmod +x "$tmp/shim2/ffmpeg"
check "another ffmpeg changes the key" 1 "$([[ $(PATH="$tmp/shim2:$PATH" bash test/fixtures/key.sh) != "$key" ]] && echo 1 || echo 0)"
unset E2E_FIXTURE_CACHE TMPDIR

# --- lib.sh: results, the work dir and the sweep, in a repo of their own.
lab="$tmp/lab"
mkdir -p "$lab/test" "$lab/tools"
cp test/lib.sh "$lab/test/" && cp tools/snapshot.sh "$lab/tools/"
git init -q "$lab" && printf '/.results/\n' >"$lab/.gitignore" && echo 1 >"$lab/f"
(
  cd "$lab" || exit 1
  export JELLYSCALE_TEST_ROOT="$tmp/root"
  source test/lib.sh
  sleep 60 & alive=$!; echo $alive >"$tmp/alive"
  mkdir -p "$tmp/root/x.999999999.dead" "$tmp/root/y.$alive.live" "$tmp/root/other" "$tmp/deadwork/t"
  echo junk >"$tmp/deadwork/t/junk" && ln -s "$tmp/deadwork/t" "$tmp/root/x.999999999.dead/t"
  make_work demo
  echo "$TMPDIR $work" >"$tmp/make_work.out"
  make_work given "$tmp/a/very/long/work/dir/that/someone/passed"
  echo "$TMPDIR $work" >"$tmp/make_work_given.out"
  make_work short "$tmp/short"
  echo "$TMPDIR" >"$tmp/make_work_short.out"
  ls "$tmp/root" >"$tmp/root.ls"
  kill $alive
  result_start; result_name=full result_summary="3 passed" result_log=/some/log
  result_finish 0
  result_name=failing; result_finish 1
  result_name=changed; echo 2 >>f; result_finish 0
  result_name=; result_finish 0
)
read -r tmpdir workdir <"$tmp/make_work.out"
check "TMPDIR is the work dir's t" "$workdir/t" "$tmpdir"
read -r tmpdir workdir <"$tmp/make_work_given.out"
check "TMPDIR stays short when the work dir is long" "$tmp/root" "$(dirname "$(dirname "$tmpdir")")"
check "and it leads into the given dir, so a killed run leaves its files there" "$workdir/t" "$(readlink "$tmpdir")"
check "a short given dir gets the same link" "$tmp/short/t" "$(readlink "$(cat "$tmp/make_work_short.out")")"
check "the sweep removes a dead run's temp files in its given work dir" 0 "$([[ -e $tmp/deadwork/t ]] && echo 1 || echo 0)"
check "and the work dir is the one given" "$tmp/a/very/long/work/dir/that/someone/passed" "$workdir"
check "the sweep removes the dir of an exited script and keeps a live one's" "other y.$(cat "$tmp/alive").live" "$(grep -v '^\(demo\|given\|short\)\.' "$tmp/root.ls" | tr '\n' ' ' | sed 's/ $//')"
out=$(ls "$lab/.results"/*/ | sort | tr '\n' ' ')
check "results: a pass and a fail are written; a changed tree and no name write nothing" "failing full " "$out"
check "a pass says so, with its log" "status=pass log=/some/log" "$(cd "$lab/.results"/*/ && sed -n 's/^\(status\|log\)=/\1=/p' full | tr '\n' ' ' | sed 's/ $//')"
check "a fail says so" "status=fail" "$(grep '^status=' "$lab/.results"/*/failing)"
check "writing .results leaves the tree alone" 0 "$(cd "$lab" && git stash list | wc -l)"

# --- putfile -delete empties a store.
mkdir "$tmp/store"
go build -o "$tmp/putfile" ./test/e2e/putfile || fails=$((fails + 1))
echo hi >"$tmp/obj"
"$tmp/putfile" "file://$tmp/store" "a/b.txt" "$tmp/obj" && "$tmp/putfile" "file://$tmp/store" "c.txt" "$tmp/obj"
check "putfile stored two objects" 2 "$(find "$tmp/store" -type f | wc -l)"
"$tmp/putfile" -delete "file://$tmp/store"
check "putfile -delete removes them all" 0 "$(find "$tmp/store" -type f | wc -l)"

# --- the control-plane stress run: in no CI tier, there to be run, and written up.
tier_tests=$(go test -tags integration -list '.*' ./... 2>/dev/null | grep -E '^Test' | grep -c 'ControlPlaneLadder')
check "the stress ladder is not in the integration tier" 0 "$tier_tests"
check "the stress ladder is not in the unit tier" 0 "$(go test -list '.*' ./... 2>/dev/null | grep -c 'ControlPlaneLadder')"
check "the stress ladder exists under its own tag" 1 "$(go test -tags stress -list '.*' ./internal/transcode 2>/dev/null | grep -c '^TestControlPlaneLadder$')"
check "test/stress.sh exists" 1 "$([[ -f test/stress.sh ]] && echo 1 || echo 0)"
check "the integration shards still tile the tier with the stress files in" "$all" "$in_shards"
arch=$(sed -n '/^## Control-plane ceiling/,/^## /p' ARCHITECTURE.md)
check "ARCHITECTURE.md has a Control-plane ceiling section" 1 "$([[ -n $arch ]] && echo 1 || echo 0)"
for v in 50 100 200 400 800 1600; do
  check "the ceiling section has a row for V=$v" 1 "$(grep -c "^| *$v *|" <<<"$arch")"
done
check "the ceiling section states streams per primary" 1 "$(grep -ci 'streams' <<<"$arch" | awk '{print ($1>0)}')"
check "the ceiling section says how to rerun" 1 "$(grep -c 'test/stress.sh' <<<"$arch" | awk '{print ($1>0)}')"
check "the ceiling section recommends on NATS" 1 "$(grep -c 'NATS' <<<"$arch" | awk '{print ($1>0)}')"
check "the ceiling section names the cores and RAM of the machine" 1 "$(grep -ciE 'cores?|threads' <<<"$arch" | awk '{print ($1>0)}')"

# A workflow script runs as the body of an async function; a stray backtick in
# a prompt ends its template literal, and the workflow then can't start at all.
for f in .claude/workflows/*.js; do
  check "$f parses" ok "$(node -e 'const s = require("fs").readFileSync(process.argv[1], "utf8").replace(/^export /m, ""); new Function("return (async () => {" + s + "\n})"); console.log("ok")' "$f" 2>&1 | tail -1)"
done

# --- tools/tiers.sh: the build workflow's tier gate, with stand-in tier scripts.
lab=$tmp/tiers; mkdir -p "$lab/tools" "$lab/test"
cp tools/tiers.sh tools/snapshot.sh "$lab/tools/"
cat >"$lab/test/stub.sh" <<'EOF'
name=$1; tree=$(bash tools/snapshot.sh); sleep 1; mkdir -p ".results/$tree"
status=pass; [[ $name == "${STUB_FAIL:-}" ]] && status=fail
printf 'tier=%s\nstatus=%s\nsummary=stub\nlog=l\n' "$name" "$status" >".results/$tree/$name"
EOF
echo 'bash test/stub.sh unit' >"$lab/test/unit.sh"
echo 'n=integration-${INTEGRATION_SHARD/\//of}; [[ $n == "${STUB_LOSE:-}" ]] && exit 1; bash test/stub.sh "$n"' >"$lab/test/integration.sh"
(
  cd "$lab" && git init -q && echo /.results/ >.gitignore
  tree=$(bash tools/snapshot.sh)
  export HOME=$tmp/home
  tiers() { bash tools/tiers.sh "$@" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d["done"], " ".join(r["name"]+"="+r["status"] for r in d["records"]))'; }
  check "tiers.sh runs every missing tier and reports each" "True unit=pass integration-rest=pass integration-0of4=pass integration-1of4=pass integration-2of4=pass integration-3of4=pass" "$(tiers "$tree" 60)"
  check "tiers.sh leaves no state after a clean gate" "" "$(ls "$HOME/.cache/jellyscale/agents" 2>/dev/null)"
  check "tiers.sh refuses a tree that is not the working tree's" "True tree=fail" "$(tiers 0000 5)"
  rm -rf .results
  check "tiers.sh reports a failing shard and one that ended without a record" "True unit=pass integration-rest=pass integration-0of4=pass integration-1of4=fail integration-2of4=fail integration-3of4=pass" "$(STUB_FAIL=integration-1of4 STUB_LOSE=integration-2of4 tiers "$tree" 60)"
  check "tiers.sh does not start a lost run again" "0" "$(STUB_LOSE=integration-2of4 tiers "$tree" 5 >/dev/null; pgrep -cf "$lab/test" || true)"
  rm -rf .results "$HOME/.cache"; mkdir -p ".results/$tree"
  printf 'status=pass\n' >".results/$tree/unit"; printf 'status=pass\n' >".results/$tree/integration"
  check "one record for the whole integration tier answers for its shards" "True unit=pass integration=pass" "$(tiers "$tree" 5)"
  exit "$fails"
)
fails=$((fails + $?))

# --- ci.yml: every job on the pinned runner, with a time limit, and every apt step bounded:
# ubuntu-latest would change ffmpeg under CI silently, and a stalled mirror hung jobs for 40 min.
unbounded=$(python3 -I - .github/workflows/ci.yml <<'EOF'
import sys, yaml
jobs = yaml.safe_load(open(sys.argv[1]))["jobs"]
for name, job in jobs.items():
    if job.get("runs-on") != "ubuntu-24.04" or "timeout-minutes" not in job:
        print(name)
    for step in job.get("steps", []):
        if "apt.sh" in str(step.get("run", "")) and "timeout-minutes" not in step:
            print(name + ": apt step")
EOF
)
check "every CI job is pinned and bounded, and every apt step too" "" "$unbounded"

# --- JS-070: the conformance checker in the e2e gates and the SDK decoders. Names the task implies:
# compat/conform/cmd/conform check -schema <file> -rules <file> -label <name> reads the JSONL the
# gates record (test/e2e/conform.mjs) on stdin, prints "<label>: checked N JSON responses on M routes"
# and fails on a violation or when it checked none; test/sdkjvm/decode.sh <Model> <file> decodes with
# jellyfin-sdk-kotlin and fails, naming java, when java is missing.
conform() { go run ./compat/conform/cmd/conform check -schema compat/schema-12.4.json -rules compat/conform.yaml -label "e2e test"; }
line='{"request":"GET /System/Info/Public","method":"GET","path":"/System/Info/Public","status":200,"contentType":"application/json","body":"{\"ServerName\":5}"}'
fails_with "the conform command fails on a response that breaks the schema" bash -c "printf '%s\n' '$line' | go run ./compat/conform/cmd/conform check -schema compat/schema-12.4.json -rules compat/conform.yaml -label t"
check "the conform command names the field" "1" "$(printf '%s\n' "$line" | conform 2>&1 | grep -c 'GET /System/Info/Public.*ServerName\|ServerName.*GET /System/Info/Public')"
fails_with "the conform command fails when it checked nothing" bash -c "go run ./compat/conform/cmd/conform check -schema compat/schema-12.4.json -rules compat/conform.yaml -label t </dev/null"
for gate in browse play subtitles failover transcode; do
  check "the $gate gate records its JSON responses for the checker" "1" "$(grep -c 'conform.mjs' test/e2e/$gate.mjs)"
done
check "run.sh checks each gate's recorded responses with the conform command" "1" "$(grep -c 'compat/conform/cmd/conform' test/e2e/run.sh | grep -c '^[1-9]')"
fails_with "the Kotlin decoder fails, not skips, without java" env PATH=/nonexistent /bin/bash test/sdkjvm/decode.sh BaseItemDto /dev/null
check "the Kotlin decoder names java when it is missing" "1" "$(env PATH=/nonexistent /bin/bash test/sdkjvm/decode.sh BaseItemDto /dev/null 2>&1 | grep -c java)"

# --- JS-114: one process harness for cmd/jellyscale's tests (TestOneProcessHarness reads every test file) and
# one helper module for the e2e gates. Test code only; every scenario and test that ran before still runs.
# Each name of the seven is defined once in all of test/e2e's .mjs files, in one module, and the check names
# the file of a second. Every form counts: function, async function, const, let, var, exported or not, at any indent.
e2e_names='login|viewer|scenario|sql|ffmpegRuns|waitFor|time'
e2e_defs() { # dir: one "name file" per definition of one of the seven
  grep -rHoE --include='*.mjs' "^[[:space:]]*(export[[:space:]]+)?(async[[:space:]]+)?(function[[:space:]]*\*?[[:space:]]*|(const|let|var)[[:space:]]+)($e2e_names)\b" "$1" |
    sed -E "s/^([^:]+):.*[[:space:]]($e2e_names)$/\2 \1/"
}
e2e_defs_report() { # dir: what is wrong with where the seven are defined, naming files; empty when right
  local defs name all
  defs=$(e2e_defs "$1" | sort)
  for name in ${e2e_names//|/ }; do
    [[ $(grep -c "^$name " <<<"$defs") -eq 1 ]] || echo "$name is defined $(grep -c "^$name " <<<"$defs") times: $(grep "^$name " <<<"$defs" | cut -d' ' -f2 | sort -u | tr '\n' ' ')"
  done
  all=$(awk '{ seen[$2] = seen[$2] " " $1 } END { for (f in seen) { n = split(seen[f], a, " "); if (n >= 7) print f } }' <<<"$defs" | sort -u)
  [[ $(grep -c . <<<"$all") -eq 1 ]] || echo "no single module defines all seven (found: $(echo $all))"
  [[ $(cut -d' ' -f2 <<<"$defs" | sort -u | grep -c .) -le 1 ]] || echo "the seven are spread over: $(cut -d' ' -f2 <<<"$defs" | sort -u | tr '\n' ' ')"
}
fixture="$tmp/defs"; mkdir -p "$fixture"
cat >"$fixture/a.mjs" <<'EOF'
export async function login(page) {}
const viewer = async () => {};
function scenario(name) {}
export const waitFor = async () => {};
EOF
cat >"$fixture/b.mjs" <<'EOF'
function helper() {
  let sql = 1;
  var time = 2;
  const ffmpegRuns = () => [];
}
const { login: other } = {}; const timeout = 3; const sqlite = 4; const loginUrl = 5;
EOF
check "the definition check counts every form (function, async, const, let, var, export, indented) and no near name" \
  "ffmpegRuns b.mjs
login a.mjs
scenario a.mjs
sql b.mjs
time b.mjs
viewer a.mjs
waitFor a.mjs" "$(e2e_defs "$fixture" | awk '{ n = split($2, p, "/"); print $1, p[n] }' | sort)"
printf 'const login = 1;\n' >"$fixture/c.mjs"
check "the check says login is in a.mjs and c.mjs" "1" "$(e2e_defs_report "$fixture" | grep 'login is defined 2 times' | grep 'a.mjs' | grep -c 'c.mjs')"
rm -r "$fixture"
check "login, viewer, scenario, sql, ffmpegRuns, waitFor and time are each defined once, all in one shared module of test/e2e" "" "$(e2e_defs_report test/e2e)"

# Every module a gate (or a module) imports is one run.sh copies beside the gates.
copied=$(grep -E '^cp ' test/e2e/run.sh | grep -oE 'test/e2e/[A-Za-z0-9_-]+\.mjs' | sort -u)
imported=$(for f in test/e2e/*.mjs; do case $f in *_test.mjs | */router.mjs) continue ;; esac
  grep -oE "from \"\./[A-Za-z0-9_-]+\.mjs\"" "$f" | sed -E 's|from "\./(.*)"|test/e2e/\1|'; done | sort -u)
check "run.sh copies every module the gates import" "" "$(comm -13 <(echo "$copied") <(echo "$imported"))"
check "run.sh copies every gate it runs" "" "$(for g in browse play subtitles failover transcode; do grep -q "test/e2e/$g.mjs" <<<"$copied" || echo "$g"; done)"

# No gate keeps one of the seven under another name or inline.
check "the sign-in form's selectors appear in one file" "1" "$(grep -l '#txtManualName' test/e2e/*.mjs | grep -c .)"
check "the pgrep listing of the worker's ffmpeg appears in one file (not counting the tests' stand-in pgrep)" "1" "$(grep -l 'pgrep' test/e2e/*.mjs | grep -vc '_test.mjs$')"
for gate in browse play subtitles failover transcode away; do
  for name in ${e2e_names//|/ }; do
    if grep -qE "(^|[^.[:alnum:]_])$name\(" "test/e2e/$gate.mjs"; then
      check "$gate.mjs imports $name from a shared module, as it calls it" 1 \
        "$(grep -Pzo "import\s*\{[^}]*\b$name\b[^}]*\}\s*from\s*\"\./" "test/e2e/$gate.mjs" | tr -d '\0' | grep -c import)"
    fi
  done
done
check "browse.mjs signs in through the shared login" 1 "$(grep -Pzo 'import\s*\{[^}]*\blogin\b[^}]*\}\s*from\s*"\./' test/e2e/browse.mjs | tr -d '\0' | grep -c import)"
check "play.mjs signs in and waits through the shared login and waitFor" 2 "$( (grep -Pzo 'import\s*\{[^}]*\blogin\b[^}]*\}\s*from\s*"\./' test/e2e/play.mjs; grep -Pzo 'import\s*\{[^}]*\bwaitFor\b[^}]*\}\s*from\s*"\./' test/e2e/play.mjs) | tr -d '\0' | grep -c import)"

# The gates, run against a stand-in browser (test/e2e/fake): the scenarios each runs, in order, with the T of
# "shard i/n: ran k of T" and the stopsWorker ones last.
# A scenario that throws fails its gate: its name and reason are printed, the next runs, the shard line is
# printed, the exit is non-zero. Failover kills what it started and resets the router, pass or fail.
rec="$tmp/rec"
bash test/e2e/fake/record.sh "$repo" "$rec" >"$tmp/gates.txt" 2>&1
# A gate's shards between them run each scenario once, and the T each shard prints is the sum.
for gate in subtitles failover transcode; do
  check "$gate: its three shards run T scenarios between them, none twice, each printing the same T" "ok" "$(
    cat "$rec/$gate"-[012].out | awk '/^run   / { n++; if (seen[$0]++) dup = 1 } /^shard / { split($4 " " $6, t, " "); T[$6]++; tot = $6 } END { print (n == tot && !dup && length(T) == 1) ? "ok" : "ran " n " of " tot " dup=" dup }')"
done
for out in "$rec"/*.out; do
  base_name=$(basename "$out" .out)
  check "gate $base_name exits non-zero when its scenarios throw" "1" "$(cat "${out%.out}.status")"
  check "gate $base_name ends with its shard line" "1" "$(tail -1 "$out" | grep -c '^shard [0-9]*/[0-9]*: ran [0-9]* of [0-9]*$')"
  case $base_name in browse | play) continue ;; esac
  ran=$(grep -c '^run   ' "$out")
  unreported=0
  while IFS= read -r name; do grep -qF "FAIL  $name: " "$out" || unreported=$((unreported + 1)); done < <(sed -n 's/^run   //p' "$out")
  check "gate $base_name prints FAIL with the name and the reason for each of its $ran scenarios" "0" "$unreported"
done
for shard in 0 1 2; do
  ran=$(grep -c '^run   ' "$rec/failover-$shard.out")
  resets=$(grep -c '"reset":true' "$rec/failover-$shard/state/router.log")
  check "failover shard $shard resets the router after each of its $ran scenarios" "$ran" "$resets"
  alive=0
  for pid in $(cat "$rec/failover-$shard"/state/replica-*.pid 2>/dev/null); do kill -0 "$pid" 2>/dev/null && alive=$((alive + 1)); done
  check "failover shard $shard leaves none of the replicas it started running" "0" "$alive"
done

# A scenario that leaves ffmpeg running fails its gate (the wait between scenarios ends), and one whose
# ffmpeg exits within the old limits (90 s in subtitles, 60 s elsewhere) does not: the stand-in pgrep shows an
# ffmpeg for ever, or for 75 s (subtitles) or 55 s (the others). The first scenario only.
only() { sed -n 's/^run   //p' "$rec/$1-0.out" | head -1 | sed 's/[][\.*^$+?(){}|/]/\\&/g; s/^/^/'; }
declare -A lasts=([subtitles]=75 [failover]=55 [transcode]=55)
for gate in subtitles failover transcode; do
  for mode in always for:${lasts[$gate]}; do
    ( bash test/e2e/fake/run-gate.sh "$repo" "$tmp/left-$gate-$mode" "$gate" "E2E_SHARD=0/3" "E2E_ONLY=$(only $gate)" "FAKE_PGREP=$mode" >"$tmp/left-$gate-$mode.out" 2>&1; echo $? >"$tmp/left-$gate-$mode.status" ) &
  done
done
wait
for gate in subtitles failover transcode; do
  out="$tmp/left-$gate-always.out"
  check "$gate: ffmpeg left running fails its gate, naming the scenario" "1" "$(grep -c '^FAIL  .*: ffmpeg still running afterwards' "$out")"
  check "$gate: ... the gate exits non-zero and prints its shard line" "1 1" "$(cat "$tmp/left-$gate-always.status") $(tail -1 "$out" | grep -c '^shard 0/3: ran 1 of ')"
  check "$gate: an ffmpeg that exits within the old wait limit is waited for" "0" "$(grep -c 'ffmpeg still running afterwards' "$tmp/left-$gate-for:${lasts[$gate]}.out")"
done

# The binary does not link a package that exists for tests (the harness is not in internal/transcode either).
check "the jellyscale binary links no package that imports testing" "" "$(go list -deps -f '{{if not .Standard}}{{.ImportPath}} {{.Imports}}{{end}}' ./cmd/jellyscale | grep -E ' (\[|.* )testing( .*|\])' | cut -d' ' -f1)"


# --- test/image.sh and test/fixtures/client.sh (JS-124) stop at once, naming what is missing. A tool dir holds
# every program of /usr/bin but docker, ffmpeg, ffprobe and go, which are stand-ins that log each call to
# $FAKE_LOG, so that "nothing was started" is read from the log.
toolbin() { # dir [with...]: with is docker, ffmpeg or go
  local dir=$1; shift
  mkdir -p "$dir"
  for f in /usr/bin/*; do
    case ${f##*/} in docker | ffmpeg | ffprobe | go) ;; *) ln -sf "$f" "$dir/${f##*/}" ;; esac
  done
  for w in "$@"; do
    cat >"$dir/$w" <<'SHIM'
#!/bin/bash
echo "${0##*/} $*" >>"$FAKE_LOG"
case "${0##*/} $1" in
  "docker info" | "docker version") [[ -n ${FAKE_NO_DAEMON:-} ]] && { echo "Cannot connect to the Docker daemon at unix:///var/run/docker.sock" >&2; exit 1; } ;;
  "docker compose") [[ $2 == version && -n ${FAKE_NO_COMPOSE:-} ]] && { echo "docker: 'compose' is not a docker command" >&2; exit 1; } ;;
  "docker pull") [[ -n ${FAKE_NO_PULL:-} ]] && { echo "Error response from daemon: pull access denied" >&2; exit 1; } ;;
esac
exit 0
SHIM
    chmod +x "$dir/$w"
  done
}
tools_run() { # name toolbin script args...: output in $tmp/<name>.out, status in $tmp/<name>.status, calls in $tmp/<name>.log
  local name=$1 bin=$2; shift 2
  : >"$tmp/$name.log"
  [[ -f $repo/$1 ]] || { echo "$1 does not exist" >"$tmp/$name.out"; echo missing >"$tmp/$name.status"; return; }
  (cd "$repo" && timeout 120 /usr/bin/env -i FAKE_LOG="$tmp/$name.log" PATH="$bin" HOME="$HOME" TMPDIR="$tmp" JELLYSCALE_TEST_ROOT="$tmp/root-$name" \
    FAKE_NO_DAEMON="${FAKE_NO_DAEMON:-}" FAKE_NO_COMPOSE="${FAKE_NO_COMPOSE:-}" FAKE_NO_PULL="${FAKE_NO_PULL:-}" \
    /usr/bin/bash "$@") >"$tmp/$name.out" 2>&1
  echo $? >"$tmp/$name.status"
}
status_nonzero() { [[ $(cat "$tmp/$1.status") == missing ]] && { echo "script missing"; return; }; [[ $(cat "$tmp/$1.status") -ne 0 ]] && echo nonzero || echo "exit $(cat "$tmp/$1.status")"; }
mentions() { grep -ciE "$2" "$tmp/$1.out" | awk '{print ($1>0) ? "named" : "not named"}'; }
started() { grep -cE '^(docker (build|buildx|compose (up|-f|--file|-p)|run|pull|create)|ffmpeg|go) ' "$tmp/$1.log"; }

toolbin "$tmp/t-nodocker" ffmpeg go
tools_run nodocker "$tmp/t-nodocker" test/image.sh
check "image.sh without docker on PATH exits non-zero" nonzero "$(status_nonzero nodocker)"
check "... naming docker" named "$(mentions nodocker docker)"
check "... before making a film or building anything" 0 "$(started nodocker)"

toolbin "$tmp/t-nocompose" docker ffmpeg go
FAKE_NO_COMPOSE=1 tools_run nocompose "$tmp/t-nocompose" test/image.sh
check "image.sh without the compose plugin exits non-zero" nonzero "$(status_nonzero nocompose)"
check "... naming compose" named "$(mentions nocompose compose)"
check "... before it builds, starts or makes anything" 0 "$(started nocompose)"

FAKE_NO_DAEMON=1 tools_run nodaemon "$tmp/t-nocompose" test/image.sh
check "image.sh with no daemon answering exits non-zero" nonzero "$(status_nonzero nodaemon)"
check "... naming the daemon" named "$(mentions nodaemon daemon)"
check "... before it builds, starts or makes anything" 0 "$(started nodaemon)"

toolbin "$tmp/t-noffmpeg" docker go
tools_run noffmpeg "$tmp/t-noffmpeg" test/image.sh
check "image.sh with no ffmpeg exits non-zero" nonzero "$(status_nonzero noffmpeg)"
check "... naming ffmpeg" named "$(mentions noffmpeg ffmpeg)"
check "... before it builds or starts anything" 0 "$(started noffmpeg)"

toolbin "$tmp/t-nogo" docker ffmpeg
tools_run nogo "$tmp/t-nogo" test/image.sh
check "image.sh with no Go exits non-zero" nonzero "$(status_nonzero nogo)"
check "... naming Go" named "$(grep -ciwE 'go' "$tmp/nogo.out" | awk '{print ($1>0) ? "named" : "not named"}')"
check "... before it builds or starts anything" 0 "$(started nogo)"

pulled=registry.invalid/jellyscale:main-0123456789ab-fixtures
FAKE_NO_PULL=1 tools_run nopull "$tmp/t-nocompose" test/image.sh "$pulled"
check "image.sh with an image it cannot pull exits non-zero" nonzero "$(status_nonzero nopull)"
check "... naming the image" named "$(grep -cF "$pulled" "$tmp/nopull.out" | awk '{print ($1>0) ? "named" : "not named"}')"
check "... having asked docker to pull it" 1 "$(grep -c "^docker pull .*$pulled" "$tmp/nopull.log" | awk '{print ($1>0)}')"
check "... having built nothing, made no film and started nothing" 0 "$(grep -cE '^(docker (build|buildx|compose (up|-f|--file|-p)|run)|ffmpeg|go) ' "$tmp/nopull.log")"

tools_run clientnoffmpeg "$tmp/t-nogo" test/fixtures/client.sh "$tmp/films-1"
check "client.sh with no ffmpeg exits non-zero" nonzero "$(status_nonzero clientnoffmpeg)"
tools_run clientnogo "$tmp/t-noffmpeg" test/fixtures/client.sh "$tmp/films-2"
check "client.sh with no Go exits non-zero" nonzero "$(status_nonzero clientnogo)"
check "... naming Go" named "$(grep -ciwE 'go' "$tmp/clientnogo.out" | awk '{print ($1>0) ? "named" : "not named"}')"
check "... and the one with no ffmpeg names ffmpeg" named "$(mentions clientnoffmpeg ffmpeg)"
# With every tool present but ffmpeg a stand-in: one that fails, and one that says it worked and writes nothing.
mkdir -p "$tmp/t-badffmpeg" && cp -P "$tmp"/t-nodocker/* "$tmp/t-badffmpeg/" 2>/dev/null
ln -sf "$(go env GOROOT)/bin/go" "$tmp/t-badffmpeg/go"
printf '#!/bin/bash\necho "ffmpeg: recipe failed" >&2\nexit 1\n' >"$tmp/t-badffmpeg/ffmpeg"; chmod +x "$tmp/t-badffmpeg/ffmpeg"
tools_run clientfails "$tmp/t-badffmpeg:$(go env GOROOT)/bin" test/fixtures/client.sh "$tmp/films-3"
check "client.sh exits non-zero when a recipe's ffmpeg fails" nonzero "$(status_nonzero clientfails)"
check "... saying something" 1 "$(wc -c <"$tmp/clientfails.out" | awk '{print ($1>0)}')"
printf '#!/bin/bash\nexit 0\n' >"$tmp/t-badffmpeg/ffmpeg"
tools_run clientempty "$tmp/t-badffmpeg:$(go env GOROOT)/bin" test/fixtures/client.sh "$tmp/films-4"
check "client.sh never exits 0 with a film missing (an ffmpeg that wrote nothing)" nonzero "$(status_nonzero clientempty)"
check "... naming a film that is missing" named "$(mentions clientempty 'film|missing')"

cd "$repo" || exit 1

# --- shard-times.sh (JS-129): the mean seconds of each package's top-level test over the tests.json files in which
# it passed. The interface: shard-times.sh <dir>... where each dir is named for a CI run id and holds that run's
# shard files (tests.json at any depth, as `gh run download` leaves them); it writes $SHARD_TIMES_OUT (default
# test/shard-times.txt), lines "<package> <test> <seconds>" under '#' lines that name the run ids.
runs="$tmp/runs"
mkdir -p "$runs/111/integration-shard-0of4" "$runs/111/integration-shard-1of4" "$runs/222/integration-shard-0of4" "$runs/333/integration-shard-rest"
j() { printf '{"Action":"%s","Package":"jellyscale/%s","Test":"%s","Elapsed":%s}\n' "$1" "$2" "$3" "$4"; }
{ j pass internal/transcode TestX 10; j pass internal/transcode TestX/sub 7; j pass internal/transcode TestY 5; j fail internal/transcode TestW 99
  j pass internal/api TestZ 1; j output internal/api TestZ 0; } >"$runs/111/integration-shard-0of4/tests.json"
{ j pass internal/transcode TestZ 3; j pass internal/api TestT 4; } >"$runs/111/integration-shard-1of4/tests.json"
{ j pass internal/transcode TestX 20; j pass internal/api TestT 5; } >"$runs/222/integration-shard-0of4/tests.json"
{ j fail internal/transcode TestX 100; j fail internal/api TestT 100; j pass internal/transcode TestV 2; } >"$runs/333/integration-shard-rest/tests.json"
SHARD_TIMES_OUT="$tmp/times-out.txt" bash test/shard-times.sh "$runs/111" "$runs/222" "$runs/333" >"$tmp/times.log" 2>&1
check "shard-times.sh writes the mean over the files in which a test passed: not a failed result, not a subtest" "internal/api TestT 4.5
internal/api TestZ 1.0
internal/transcode TestV 2.0
internal/transcode TestX 15.0
internal/transcode TestY 5.0
internal/transcode TestZ 3.0" "$(grep -v '^#' "$tmp/times-out.txt" 2>/dev/null)"
check "... a test that never passed has no entry, and one name in two packages is two entries" "0 2" "$(grep -c 'TestW' "$tmp/times-out.txt" 2>/dev/null | tr -d '\n') $(grep -c ' TestZ ' "$tmp/times-out.txt" 2>/dev/null)"
check "... its header names the run ids it was given and says the times are from CI's runners" "1 1 1 1" "$(h=$(grep '^#' "$tmp/times-out.txt" 2>/dev/null); for want in 111 222 333 "CI.s runners"; do echo "$h" | grep -cE "$want" | awk '{printf "%d ", ($1>0)}'; done | sed 's/ $//')"
check "... the header does not say 'developer machine'" "0" "$(grep '^#' "$tmp/times-out.txt" 2>/dev/null | grep -ci 'developer machine')"
SHARD_TIMES_OUT="$tmp/times-out2.txt" bash test/shard-times.sh "$runs/111" "$runs/222" "$runs/333" >/dev/null 2>&1
check "the same inputs give the same file" "$(cat "$tmp/times-out.txt" 2>/dev/null)" "$(cat "$tmp/times-out2.txt" 2>/dev/null)"
check "shard-times.sh checks in a file it did not write by hand: its header says it was made by test/shard-times.sh" "1" "$(grep '^#' test/shard-times.txt | grep -c 'test/shard-times.sh' | awk '{print ($1>0)}')"

# --- check-shards.sh with shards that hold two dealt packages: the record of each shard is the count test/shard.sh gives it.
rm -rf "$records"; mkdir "$records"
total=$(echo "$all" | wc -l | tr -d ' ')
i=0
for s in $matrix; do echo "integration $s $(bash test/shard.sh count "$s")" >"$records/$i.txt"; i=$((i + 1)); done
bash test/check-shards.sh integration "$records" "$total" >/dev/null 2>&1; check "records of the matrix's shards, two packages in some, add up to the tier" 0 $?
fails_with "a test in no shard (one fewer than the tier has) fails" bash test/check-shards.sh integration "$records" $((total + 1))
fails_with "a test in two shards (one more than the tier has) fails" bash test/check-shards.sh integration "$records" $((total - 1))
echo "integration $(echo "$matrix" | cut -d' ' -f1) $(bash test/shard.sh count "$(echo "$matrix" | cut -d' ' -f1)")" >"$records/dup.txt"
fails_with "a shard that left two records fails" bash test/check-shards.sh integration "$records" "$total"
rm "$records/dup.txt" "$records/0.txt"
fails_with "a shard that left no record fails" bash test/check-shards.sh integration "$records" "$total"
echo "integration $(echo "$matrix" | cut -d' ' -f1) 0" >"$records/0.txt"
fails_with "a shard that ran nothing fails" bash test/check-shards.sh integration "$records" "$total"
# integration.sh's -race run of TestHarness stays in the one shard that runs cmd/jellyscale, and in a whole run.
race_shards=0
for s in $matrix; do
  if [[ $s == rest ]]; then bash test/shard.sh args rest | grep -qx 'jellyscale/cmd/jellyscale'; else bash test/shard.sh list "$s" | grep -q '^cmd/jellyscale '; fi && race_shards=$((race_shards + 1))
done
check "exactly one shard of the matrix runs cmd/jellyscale (so the -race run of TestHarness is in one)" "1" "$race_shards"
check "integration.sh runs TestHarness under -race for a shard with cmd/jellyscale or for the whole tier" "1 1" "$(grep -c 'go test -tags integration -race -count=1 -run TestHarness ./cmd/jellyscale' test/integration.sh | tr -d '\n') $(grep -c '\./\.\.\.' test/integration.sh | awk '{print ($1>0)}')"
# A shard run fails when it ran a count other than shard.sh count gives, with two packages as with one: report.sh asks for it.
check "report.sh holds a shard run to test/shard.sh count" "1" "$(grep -c 'want=$(bash test/shard.sh count' test/report.sh)"

# --- the integration job's summary: one line for each shard, from that shard's own tests.json (shard-summary.sh).
# Interface: shard-summary.sh <dir> <name>...: <dir>/*<name>*/**/tests.json is that shard's; one line each:
# "integration shard <name>: <N>s of tests" (N the summed Elapsed of its top-level tests that passed or failed), or
# "integration shard <name>: no tests.json (missing)"; then the line "refresh test/shard-times.txt" when the slowest
# of the names that are i-of-n is over 1.25 times the fastest (rest is printed, not compared). It always exits 0.
sd="$tmp/sd"
mkshard() { mkdir -p "$sd/integration-shard-$1/logs"; j pass internal/transcode TestA "$2" >"$sd/integration-shard-$1/logs/tests.json"; j pass internal/transcode TestA/sub 999 >>"$sd/integration-shard-$1/logs/tests.json"; j fail internal/api TestB "$3" >>"$sd/integration-shard-$1/logs/tests.json"; }
mkshard 0of2 60 40; mkshard 1of2 100 25; mkshard rest 3000 0
out=$(bash test/shard-summary.sh "$sd" 0of2 1of2 rest 2>&1); status=$?
check "the summary has a line for each shard, rest included, with the seconds of its own tests.json (subtests not counted)" "integration shard 0of2: 100s of tests
integration shard 1of2: 125s of tests
integration shard rest: 3000s of tests" "$(echo "$out" | grep '^integration shard ')"
check "125 s against 100 s is exactly 1.25 times: no line to refresh the times" "0" "$(echo "$out" | grep -c 'refresh test/shard-times.txt')"
check "the summary's exit status is 0" "0" "$status"
mkshard 1of2 101 25
check "126 s against 100 s is over 1.25 times: the line 'refresh test/shard-times.txt'" "1" "$(bash test/shard-summary.sh "$sd" 0of2 1of2 rest 2>&1 | grep -c '^refresh test/shard-times.txt')"
mkshard rest 1 0; mkshard 1of2 100 25
check "rest is not compared: its seconds being far from the others' asks for no refresh" "0" "$(bash test/shard-summary.sh "$sd" 0of2 1of2 rest 2>&1 | grep -c 'refresh test/shard-times.txt')"
# The bound is on the seconds as they are, not as printed: 1251.2 against 1000.6 is 1.25045 times, over, though both print as 1251 and 1001.
mkshard 0of2 1000.6 0; mkshard 1of2 1251.2 0
check "1.25045 times (whole seconds print as 1251 and 1001) is over 1.25: the line to refresh the times" "1" "$(bash test/shard-summary.sh "$sd" 0of2 1of2 rest 2>&1 | grep -c '^refresh test/shard-times.txt')"
mkshard 0of2 1000.6 0; mkshard 1of2 1250.7 0
check "1.2499 times is not over 1.25 though 1251 against 1001 would be" "0" "$(bash test/shard-summary.sh "$sd" 0of2 1of2 rest 2>&1 | grep -c '^refresh test/shard-times.txt')"
mkshard 1of2 100 25
rm -r "$sd/integration-shard-1of2"
out=$(bash test/shard-summary.sh "$sd" 0of2 1of2 rest 2>&1); status=$?
check "a shard without a tests.json is named as missing, not counted as 0 seconds" "integration shard 1of2: no tests.json (missing)" "$(echo "$out" | grep '1of2')"
check "... and the summary still exits 0" "0" "$status"

# --- test/apt.sh (JS-129): packages come from .deb files that actions/cache restores. The interface: APT_DEBS is
# the directory the cache holds (restored before the step, saved after it); APT_ARCHIVES is where apt leaves the
# .debs it downloads (default /var/cache/apt/archives; with stand-ins here, the stand-in apt-get writes there, or
# to the directory of -o Dir::Cache::archives=). Stand-in sudo, apt-get and dpkg log what they are asked, and
# apt-get "downloads" a package named on its command line (a name, not a file) from archive.ubuntu.com, with
# ffmpeg needing libfakedep: ffmpeg's .deb without libfakedep's in the same call is an unmet dependency.
apt="$tmp/apt"; mkdir -p "$apt/bin" "$apt/archives"
cat >"$apt/bin/sudo" <<'SHIM'
#!/bin/bash
echo "sudo $*" >>"$FAKE_APT_LOG"
case $1 in apt-get | dpkg) exec "$@" ;; *) exit 0 ;; esac
SHIM
cat >"$apt/bin/apt-get" <<'SHIM'
#!/bin/bash
echo "apt-get $*" >>"$FAKE_APT_LOG"
archives=${APT_ARCHIVES:-/nonexistent}; args=(); download_only=; reinstall=; print_uris=
while [[ $# -gt 0 ]]; do
  case $1 in
    -o) [[ $2 == Dir::Cache::archives=* ]] && archives=${2#*=}; shift 2 ;;
    -o*) [[ $1 == -oDir::Cache::archives=* ]] && archives=${1#*=}; shift ;;
    --download-only | -d) download_only=1; shift ;;
    --reinstall) reinstall=1; shift ;;
    --print-uris) print_uris=1; shift ;;
    -*) shift ;;
    *) args+=("$1"); shift ;;
  esac
done
case ${args[0]:-} in
update) echo "NET archive.ubuntu.com update" >>"$FAKE_APT_LOG"; exit 0 ;;
install)
  files=() names=()
  for a in "${args[@]:1}"; do if [[ $a == ./* || $a == /* ]]; then files+=("$a"); else names+=("$a"); fi; done
  for f in "${files[@]}"; do [[ -f $f ]] || { echo "E: unable to locate $f" >&2; exit 100; }; done
  if [[ ${#files[@]} -gt 0 ]]; then
    if [[ -n $print_uris ]]; then
      printf '%s\n' "${files[@]}" | grep -q 'ffmpeg_' && ! printf '%s\n' "${files[@]}" | grep -q 'libfakedep_' && echo "'http://archive.ubuntu.com/ubuntu/pool/main/libf/libfakedep_1.0_amd64.deb' libfakedep_1.0_amd64.deb 10 SHA256:0"
      exit 0
    fi
    printf '%s\n' "${files[@]}" | grep -q 'ffmpeg_' && ! printf '%s\n' "${files[@]}" | grep -q 'libfakedep_' && { echo "E: unmet dependencies: ffmpeg needs libfakedep" >&2; exit 100; }
    for f in "${files[@]}"; do echo "INSTALLED $(basename "$f" | sed 's/_.*//')" >>"$FAKE_APT_LOG"; done
  fi
  for n in "${names[@]}"; do
    # A package the image already has is not downloaded, as the real apt-get, unless asked to reinstall it.
    if [[ " ${FAKE_PREINSTALLED:-} " == *" $n "* && -z $reinstall ]]; then echo "already the newest version: $n" >&2; continue; fi
    echo "NET archive.ubuntu.com download $n" >>"$FAKE_APT_LOG"
    mkdir -p "$archives"; : >"$archives/${n}_1.0_amd64.deb"
    [[ $n == ffmpeg ]] && : >"$archives/libfakedep_1.0_amd64.deb"
    if [[ -z $download_only ]]; then echo "INSTALLED $n" >>"$FAKE_APT_LOG"; [[ $n == ffmpeg ]] && echo "INSTALLED libfakedep" >>"$FAKE_APT_LOG"; fi
  done ;;
esac
exit 0
SHIM
cat >"$apt/bin/dpkg" <<'SHIM'
#!/bin/bash
echo "dpkg $*" >>"$FAKE_APT_LOG"
[[ $1 == -i || $1 == --install || $1 == -R ]] || exit 1
shift
files=("$@")
printf '%s\n' "${files[@]}" | grep -q 'ffmpeg_' && ! printf '%s\n' "${files[@]}" | grep -q 'libfakedep_' && { echo "dpkg: dependency problems: ffmpeg needs libfakedep" >&2; exit 1; }
for f in "${files[@]}"; do [[ -f $f ]] && echo "INSTALLED $(basename "$f" | sed 's/_.*//')" >>"$FAKE_APT_LOG"; done
exit 0
SHIM
chmod +x "$apt/bin/"*
apt_run() { # name packages...: output in $apt/<name>.out, calls in $apt/<name>.log, status in $apt/<name>.status
  local name=$1; shift
  : >"$apt/$name.log"
  ( cd "$tmp" && PATH="$apt/bin:$PATH" FAKE_PREINSTALLED="${FAKE_PREINSTALLED:-}" FAKE_APT_LOG="$apt/$name.log" APT_DEBS="$apt/debs" APT_ARCHIVES="$apt/archives" bash "$repo/test/apt.sh" "$@" >"$apt/$name.out" 2>&1; echo $? >"$apt/$name.status" )
}
rm -rf "$apt/debs" "$apt/archives"; mkdir -p "$apt/debs" "$apt/archives"
apt_run miss ffmpeg fontconfig
check "a miss installs as today and exits 0" "0" "$(cat "$apt/miss.status")"
check "... asking archive.ubuntu.com, with three retries on the update and the install" "2" "$(grep -E '^apt-get .*Acquire::Retries=3.* (update|install)' "$apt/miss.log" | wc -l | tr -d ' ')"
check "... installing every package of the list" "INSTALLED ffmpeg
INSTALLED fontconfig" "$(grep '^INSTALLED' "$apt/miss.log" | grep -v libfakedep | sort)"
check "... leaving the .deb of every package it installed, dependencies included, in the cache directory" "ffmpeg_1.0_amd64.deb
fontconfig_1.0_amd64.deb
libfakedep_1.0_amd64.deb" "$(ls "$apt/debs" | sort)"
apt_run hit ffmpeg fontconfig
check "an install from those files alone exits 0" "0" "$(cat "$apt/hit.status")"
check "... runs no apt-get update" "0" "$(grep -c 'apt-get.* update' "$apt/hit.log")"
check "... installs the files with dpkg, never through apt-get install, which found archives to fetch on CI" "0 1" "$(grep -c '^apt-get .*install' "$apt/hit.log") $(grep -c '^dpkg -i' "$apt/hit.log")"
check "... asks no mirror for anything (archive, azure or security)" "0" "$(grep -c '^NET ' "$apt/hit.log")"
check "... installs every package of the list and the dependency" "INSTALLED ffmpeg
INSTALLED fontconfig
INSTALLED libfakedep" "$(grep '^INSTALLED' "$apt/hit.log" | sort)"
# The cache of one list is no help to another list's key, but a directory is a directory: a list the files do not cover is a miss.
rm -rf "$apt/archives"; mkdir -p "$apt/archives"
apt_run other ffmpeg fonts-dejavu-core
check "a package list the cached files do not hold is installed from the mirror, not skipped" "INSTALLED fonts-dejavu-core" "$(grep '^INSTALLED' "$apt/other.log" | grep fonts-dejavu-core)"
# The runner image has fontconfig and fonts-dejavu-core already: a miss must still leave their .debs, or no run ever hits.
rm -rf "$apt/debs" "$apt/archives"; mkdir -p "$apt/debs" "$apt/archives"
FAKE_PREINSTALLED="fontconfig fonts-dejavu-core" apt_run pre_miss ffmpeg fontconfig fonts-dejavu-core
check "a miss on packages the image already has leaves a .deb for each package of the list" "ffmpeg_1.0_amd64.deb
fontconfig_1.0_amd64.deb
fonts-dejavu-core_1.0_amd64.deb
libfakedep_1.0_amd64.deb" "$(ls "$apt/debs" | sort)"
FAKE_PREINSTALLED="fontconfig fonts-dejavu-core" apt_run pre_hit ffmpeg fontconfig fonts-dejavu-core
check "... and the next run is a hit: no update, no mirror" "0 0" "$(grep -c 'apt-get.* update' "$apt/pre_hit.log") $(grep -c '^NET ' "$apt/pre_hit.log")"
# A cache that lacks a dependency the hit needs (run 44012242557: apt could not fetch it with no mirror) falls back
# to the mirror instead of failing the job.
rm -f "$apt/debs"/libfakedep_*.deb; rm -rf "$apt/archives"; mkdir -p "$apt/archives"
FAKE_PREINSTALLED="fontconfig fonts-dejavu-core" apt_run gap ffmpeg fontconfig fonts-dejavu-core
check "a hit whose files lack a dependency still exits 0" "0" "$(cat "$apt/gap.status")"
check "... by installing from the mirror" "INSTALLED ffmpeg" "$(grep '^INSTALLED ffmpeg' "$apt/gap.log" | tail -1)"

# --- ci.yml (JS-129): the deb cache, the jars cache, the guard of the ffmpeg version, no waiting for lint-and-unit, the
# summary and the cancelling of the media jobs when lint-and-unit fails. Each line is "name<TAB>ok" or "name<TAB>why".
ci_results=$(python3 -I - .github/workflows/ci.yml test/integration.sh <<'EOF'
import re, sys, yaml
doc = yaml.safe_load(open(sys.argv[1]))
jobs = doc["jobs"]
integration_sh = open(sys.argv[2]).read()
out = []
def result(name, problem=None):
    out.append(f"{name}\t{problem or 'ok'}")
def steps(job): return jobs[job].get("steps", [])
def apt_index(job):
    for i, s in enumerate(steps(job)):
        if "apt.sh" in str(s.get("run", "")): return i
    return None
def uses_cache(s): return str(s.get("uses", "")).startswith("actions/cache")
def push_only(job): return "github.event_name == 'push'" in str(jobs[job].get("if", ""))

# The packages of each job's install step.
lists = {}
for job in ("integration-shard", "roles", "e2e-shard", "fixture-cache"):
    i = apt_index(job)
    lists[job] = re.search(r"apt\.sh(.*)", steps(job)[i]["run"]).group(1).split()
apt_keys = {}
for job, pkgs in lists.items():
    i = apt_index(job)
    caches = [s for s in steps(job)[:i] if uses_cache(s)]
    if not caches:
        result(f"{job}: a cache of the .deb files is restored before the install step", "no actions/cache step before it"); continue
    c = caches[-1]["with"]
    key = str(c.get("key", ""))
    apt_keys[job] = key
    problems = []
    if "ImageVersion" not in key: problems.append("the key does not hold the runner image version (env.ImageVersion)")
    if not (all(p in key for p in pkgs) or "hashFiles(" in key): problems.append(f"the key does not hold the package list {pkgs}")
    # A restore key may fall back to another runner image's files (test/apt.sh goes to the mirror
    # if dpkg refuses them), never to another package list's.
    for rk in str(c.get("restore-keys", "")).split():
        if not all(p in rk for p in pkgs): problems.append(f"restore-key {rk} would restore another package list's files")
    env = {**jobs[job].get("env", {}), **steps(job)[i].get("env", {})}
    if str(env.get("APT_DEBS", "")) != str(c.get("path", "")): problems.append("APT_DEBS of the install step is not the cache's path")
    result(f"{job}: .deb cache keyed on the image version and the package list, restored before the install", "; ".join(problems) or None)
    nxt = steps(job)[i + 1] if i + 1 < len(steps(job)) else {}
    want = "nproc\nffmpeg -version | head -1\nffmpeg -version | head -1 | grep -q '^ffmpeg version 6\\.1' || { echo 'CI must test ffmpeg 6.1, the oldest jellyscale supports'; exit 1; }\n"
    ok = nxt.get("name") == "CI tests ffmpeg 6.1 on a known number of CPUs" and nxt.get("run") == want
    result(f"{job}: the ffmpeg 6.1 step is unchanged and right after the install", None if ok else "not as it was")
    result(f"{job}: the install step keeps its 5-minute limit", None if steps(job)[i].get("timeout-minutes") == 5 else "no timeout-minutes: 5")
# A push to main saves each key a merge group uses, since a merge group's cache is seen only there.
push_keys = set()
for job in jobs:
    if push_only(job):
        for s in steps(job):
            if uses_cache(s): push_keys.add(str(s["with"].get("key", "")))
for job, key in apt_keys.items():
    if not push_only(job):
        result(f"{job}: a push to main saves the .deb cache for its package list", None if key in push_keys else f"no job that runs only on a push uses the key {key!r}")
# The jars of the differential shard.
jar_steps = [s for s in steps("e2e-shard") if uses_cache(s) and "jars.txt" in str(s["with"].get("key", ""))]
if not jar_steps:
    result("e2e-shard: the sdkjvm jars cache is keyed on test/sdkjvm/jars.txt", "no actions/cache step with hashFiles of test/sdkjvm/jars.txt in its key")
else:
    c = jar_steps[0]["with"]
    key = str(c["key"])
    path = str(c["path"]).replace("~", "/home/runner")
    env = {**jobs["e2e-shard"].get("env", {}), **jar_steps[0].get("env", {})}
    for s in steps("e2e-shard"):
        if "differential/run.sh" in str(s.get("run", "")): env.update(s.get("env", {}))
    want = str(env.get("JELLYSCALE_SDKJVM_CACHE", "/home/runner/.cache/jellyscale/sdkjvm")).replace("~", "/home/runner")
    result("e2e-shard: the jars cache holds the directory decode.sh reads", None if path.rstrip("/") == want.rstrip("/") else f"path {path} is not {want}")
    result("e2e-shard: the jars key is hashFiles of test/sdkjvm/jars.txt", None if re.search(r"hashFiles\(\s*'test/sdkjvm/jars\.txt'\s*\)", key) else f"key {key}")
    result("a push to main saves the jars cache", None if any(push_only(j) and any(uses_cache(s) and str(s["with"].get("key", "")) == key for s in steps(j)) for j in jobs) else "no push-only job with that key")
# Nothing waits for lint-and-unit.
for job in ("integration-shard", "e2e-shard", "roles"):
    need = jobs[job].get("needs", [])
    need = [need] if isinstance(need, str) else need
    result(f"{job} does not wait for lint-and-unit", None if "lint-and-unit" not in need else "needs lint-and-unit")
# The cancelling job.
cancel = [j for j, v in jobs.items() if "lint-and-unit" in ([v["needs"]] if isinstance(v.get("needs"), str) else v.get("needs", [])) and "failure" in str(v.get("if", ""))]
if not cancel:
    result("a job that needs lint-and-unit and runs when it failed cancels the media jobs", "no job needs lint-and-unit with an if that names its failure")
else:
    j = jobs[cancel[0]]; text = yaml.dump(j)
    problems = []
    if "lint-and-unit.result" not in str(j.get("if", "")): problems.append("its if does not read needs.lint-and-unit.result")
    if "code" not in str(j.get("if", "")) + text: problems.append("it does not look at whether the media jobs run (needs.changes.outputs.code)")
    if j.get("permissions", {}).get("actions") != "write": problems.append("no permissions: actions: write")
    if "cancel" not in text: problems.append("it cancels nothing")
    if re.search(r"cancel", text) and not re.search(r"github\.run_id|GITHUB_RUN_ID", text): problems.append("it names no run id: it could cancel another run")
    if "lint-and-unit" not in "".join(str(s.get("run", "")) for s in j.get("steps", [])): problems.append("its log never says lint-and-unit's failure is why")
    result("the job that cancels the media jobs when lint-and-unit fails", "; ".join(problems) or None)
    if "lint-and-unit" in str(jobs["lint-and-unit"].get("continue-on-error", "")): result("lint-and-unit still fails the run", "continue-on-error")
# The summary.
summary = [s for s in steps("integration") if "shard-summary.sh" in str(s.get("run", ""))]
if not summary: result("the integration job's summary comes from test/shard-summary.sh", "no step runs it")
else:
    problems = []
    if summary[0].get("continue-on-error") is not True: problems.append("continue-on-error is not true: the notice could fail the check")
    if "GITHUB_STEP_SUMMARY" not in str(summary[0].get("run", "")): problems.append("it does not write GITHUB_STEP_SUMMARY")
    result("the integration job's summary comes from test/shard-summary.sh and fails nothing", "; ".join(problems) or None)
    # Where the summary reads from: a download of the shards' artifacts that keeps each one apart (merge-multiple would put
    # every shard's tests.json on one path), and the names it is given are the matrix's, rest included.
    run = str(summary[0].get("run", ""))
    m = re.search(r"shard-summary\.sh\s+(\S+)", run)
    sdir = m.group(1) if m else None
    downloads = [s for s in steps("integration") if str(s.get("uses", "")).startswith("actions/download-artifact") and str(s.get("with", {}).get("path", "")).rstrip("/") == str(sdir).rstrip("/")]
    problems = []
    if not downloads: problems.append(f"no download-artifact step with path {sdir!r}, the directory the summary reads")
    else:
        w = downloads[0]["with"]
        if str(w.get("merge-multiple", "false")).lower() == "true": problems.append("that download has merge-multiple: every shard's tests.json lands on one path")
        if "integration-shard" not in str(w.get("pattern", w.get("name", ""))): problems.append("that download does not take the integration-shard-* artifacts")
    names = [e["name"] for e in jobs["integration-shard"]["strategy"]["matrix"]["include"]]
    given = [n for n in names if n in run]
    if len(given) != len(names): problems.append(f"the summary names {given}, the matrix has {names}")
    result("the summary reads the shards' own artifacts, apart, and names every shard of the matrix, rest included", "; ".join(problems) or None)
check_steps = [s for s in steps("integration") if "check-shards.sh integration" in str(s.get("run", ""))]
result("the integration check still runs check-shards.sh and that step does not continue on error", None if check_steps and not check_steps[0].get("continue-on-error") else "missing or continue-on-error")
# A failed shard still leaves its test record, and a push to main still saves the caches.
up = [s for s in steps("integration-shard") if str(s.get("uses", "")).startswith("actions/upload-artifact") and "integration-shard-" in str(s.get("with", {}).get("name", ""))]
result("the integration shard uploads its tests.json pass or fail", None if up and "always()" in str(up[0].get("if", "")) else "no if: always() on the upload")
cj = [j for j in jobs.values() if "gh run cancel" in yaml.dump(j)]
ev = str(cj[0].get("if", "")) if cj else ""
result("the job that cancels the run runs only for merge_group and pull_request", None if "merge_group" in ev and "pull_request" in ev else "its if names no event")
# Both integration.sh paths pass the caller's go test arguments, and neither refuses them.
result("integration.sh passes extra go test arguments in a dealt shard", None if "can't be combined" not in integration_sh else "refused")
# The limits.
m = re.search(r"-timeout (\d+)m", integration_sh)
go_limit = int(m.group(1))
job_limit = jobs["integration-shard"]["timeout-minutes"]
result("go test's -timeout is at least 45m, the shard job's timeout-minutes at least 60 and above it", None if go_limit >= 45 and job_limit >= 60 and job_limit > go_limit else f"go {go_limit}m, job {job_limit}m")
print("\n".join(out))
EOF
)
while IFS=$'\t' read -r name verdict; do [[ -n $name ]] && check "ci.yml: $name" "ok" "$verdict"; done <<<"$ci_results"
check "ci.yml has been read (the python above ran to the end)" "yes" "$([[ $ci_results == *'go test'* ]] && echo yes || echo "no: ${ci_results:0:200}")"

# --- the jars of the differential shard: a restored jar whose sha256 differs from jars.txt still fails, naming the jar.
jv="$tmp/jars"; mkdir -p "$jv/bin" "$jv/cache"
printf '#!/bin/sh\necho "java $*" >>%s/java.log\n' "$jv" >"$jv/bin/java"; chmod +x "$jv/bin/java"
printf '#!/bin/sh\necho "curl $*" >>%s/curl.log\nexit 22\n' "$jv" >"$jv/bin/curl"; chmod +x "$jv/bin/curl"
first_jar=$(grep -v '^#' test/sdkjvm/jars.txt | awk 'NF { print $1; exit }')
jar_name=$(echo "$first_jar" | awk -F: '{ print $2 "-" $3 ".jar" }')
echo "not the pinned jar" >"$jv/cache/$jar_name"
PATH="$jv/bin:$PATH" JELLYSCALE_SDKJVM_CACHE="$jv/cache" bash test/sdkjvm/decode.sh Foo /dev/null >"$jv/out" 2>&1; st=$?
# --fetch only fills the cache: the push job that does it has no JDK to need.
# Its own empty cache and its own curl log: a cold cache makes it ask Maven Central, which the checks below must not count.
mkdir -p "$jv/nojava" "$jv/fetchcache"; for t in sha256sum cut mkdir rm mv; do ln -sf "$(command -v $t)" "$jv/nojava/$t"; done
printf '#!/bin/sh\necho "curl $*" >>%s/fetch-curl.log\nexit 22\n' "$jv" >"$jv/nojava/curl"; chmod +x "$jv/nojava/curl"
PATH="$jv/nojava" JELLYSCALE_SDKJVM_CACHE="$jv/fetchcache" /bin/bash test/sdkjvm/decode.sh --fetch >"$jv/fetch.out" 2>&1
check "decode.sh --fetch does not ask for java" "0" "$(grep -c 'needs java' "$jv/fetch.out")"
check "a restored jar whose sha256 differs from jars.txt fails" "nonzero" "$([[ $st -ne 0 ]] && echo nonzero || echo zero)"
check "... naming the jar" "1" "$(grep -c "$jar_name" "$jv/out" | awk '{print ($1>0)}')"
check "... and never asked Maven Central for it" "0" "$(cat "$jv/curl.log" 2>/dev/null | grep -c 'repo1.maven.org')"

# --- failover.mjs (JS-129): it exits within 5 s of printing its shard line, with the two rolled scenarios run, and a
# replica that never leaves after SIGTERM still fails its scenario by name, not before the bound has passed (the gate's
# 180 s; E2E_SIGTERM_BOUND_S shortens it here so that lint stays quick). Run with the stand-in
# browser that plays (test/e2e/fake/playwright-play), the stand-in replica that leaves 300 ms after SIGTERM, and
# the same with FAKE_REPLICA_IGNORES_SIGTERM=1.
stamped_gate() { # name env...: every line of the gate's output stamped with the time it came, then "end <time>" and "status <n>"
  local name=$1; shift
  ( FAKE_PLAYWRIGHT=playwright-play bash "$repo/test/e2e/fake/run-gate.sh" "$repo" "$tmp/ff-$name" failover "$@" 2>&1 | while IFS= read -r l; do printf '%s %s\n' "$(date +%s.%N)" "$l"; done
    st=${PIPESTATUS[0]}; echo "end $(date +%s.%N)"; echo "status $st" ) >"$tmp/ff-$name.out" 2>&1
}
rolled='^a stream replica rolled'
stamped_gate rolled "E2E_ONLY=$rolled" &
stamped_gate stuck "E2E_ONLY=$rolled.*HLS\$" "FAKE_REPLICA_IGNORES_SIGTERM=1" "E2E_SIGTERM_BOUND_S=3" &
wait
gap() { # name: seconds from the shard line to the end of the process
  awk '/ shard [0-9]+\/[0-9]+: ran / { s = $1 } /^end / { e = $2 } END { printf "%.1f\n", e - s }' "$tmp/ff-$1.out"
}
check "failover: with both rolled scenarios passing, they are two 'ok' lines" "2" "$(grep -c ' ok    a stream replica rolled' "$tmp/ff-rolled.out")"
check "failover: the shard line is its last line, and the gate exits 0" "1 0" "$(grep -v '^end \|^status ' "$tmp/ff-rolled.out" | tail -1 | grep -c ' shard 0/1: ran 2 of 5' | tr -d '\n') $(sed -n 's/^status //p' "$tmp/ff-rolled.out")"
check "failover: it exits within 5 s of printing its shard line when every scenario passed" "yes" "$(awk -v g="$(gap rolled)" 'BEGIN { print (g < 5) ? "yes" : "no: " g " s" }')"
check "failover: a replica that never exits after SIGTERM fails its scenario by name" "1" "$(grep -c ' FAIL  a stream replica rolled.*HLS: the replica that got SIGTERM was still running 3 s later' "$tmp/ff-stuck.out")"
check "failover: ... not before the 3 s bound has passed since the scenario began" "yes" "$(awk '/ run   a stream replica rolled/ { r = $1 } / FAIL  a stream replica rolled/ { f = $1 } END { print (f - r >= 3) ? "yes" : "no: " f - r " s" }' "$tmp/ff-stuck.out")"
check "failover: the bound is 180 s unless E2E_SIGTERM_BOUND_S says otherwise" "1" "$(grep -c 'E2E_SIGTERM_BOUND_S || 180' test/e2e/failover.mjs)"
check "failover: ... and the gate exits non-zero within 5 s of its shard line" "1 yes" "$(sed -n 's/^status //p' "$tmp/ff-stuck.out" | awk '{print ($1 != 0)}') $(awk -v g="$(gap stuck)" 'BEGIN { print (g < 5) ? "yes" : "no: " g " s" }')"
check "failover: ... with the shard line still its last line" "1" "$(grep -v '^end \|^status ' "$tmp/ff-stuck.out" | tail -1 | grep -c ' shard 0/1: ran 1 of 5')"
for name in rolled stuck; do
  alive=0
  for pid in $(cat "$tmp/ff-$name"/state/replica-*.pid 2>/dev/null); do kill -0 "$pid" 2>/dev/null && alive=$((alive + 1)); done
  check "failover ($name): no replica is left running" "0" "$alive"
done

# --- docs/testing.md describes the split as it is.
docs_line() { grep -F -- "$1" docs/testing.md | grep -F -- "$2" | grep -cF -- "$3" | awk '{print ($1>0)}'; }
check "docs/testing.md: the split names test/shard.sh, internal/api, the times file and how to refresh it" "1" "$(docs_line 'test/shard.sh' 'internal/api' 'test/shard-times.sh')"
check "docs/testing.md: ... and the summary's notice" "1" "$(docs_line 'test/shard.sh' 'internal/api' 'refresh test/shard-times.txt')"
check "docs/testing.md: ... and that the times come from CI's runners" "1" "$(grep -F 'test/shard.sh' docs/testing.md | grep -ciE "CI.s runners" | awk '{print ($1>0)}')"
check "docs/testing.md: the .deb cache says what a miss does" "1" "$(docs_line '.deb' 'actions/cache' 'a miss')"
check "docs/testing.md: the jars cache is keyed on test/sdkjvm/jars.txt and says what a miss does" "1" "$(docs_line 'jars.txt' 'actions/cache' 'a miss')"
check "docs/testing.md no longer says the times are for internal/transcode alone" "0" "$(grep -c 'tests of `internal/transcode` dealt to shard i of n' docs/testing.md)"

# --- JS-130: test/integration.sh runs INTEGRATION_PROCS test processes side by side, each with a Postgres of its own.
# Interface the checks use, which the build gives it:
#   INTEGRATION_PROCS=n         n >= 2 processes at once for the shard INTEGRATION_SHARD names (dealt shards and rest); unset or 1 as before
#   INTEGRATION_PROC=k          with INTEGRATION_PROCS=n: only process k (0-based) runs, as one Postgres and one go test process
#   test/shard.sh list|count|args <shard> <k>/<m>   the share of process k of m (same split every time), and
#   test/shard.sh list|count|args <shard> alone     the tests of test/alone.txt that the shard holds; `list|count <shard>` is still the whole shard
#   test/alone.txt              lines "<package> <test> <reason>", '#' lines are comments; SHARD_ALONE names another file (as SHARD_TIMES does)
#   ci.yml                      each matrix entry has `procs`, and the job sets INTEGRATION_PROCS: ${{ matrix.procs }}
# The stand-ins are a docker that starts real listeners (so the port probes work) and logs every call, a go whose
# `tool gotestsum` writes the test record a go test would, from the tests `go test -list` found, an ffmpeg and a
# sleep that does not sleep. They need no Docker and run no test. FAKE_* variables tell them what to do.
cd "$repo" || exit 1
lab="$tmp/js130"; mkdir -p "$lab/bin"
real_go=$(go env GOROOT)/bin/go
for f in /usr/bin/*; do
  case ${f##*/} in docker | ffmpeg | ffprobe | fc-list | go | sleep) ;; *) ln -sf "$f" "$lab/bin/${f##*/}" ;; esac
done
cat >"$lab/bin/docker" <<'SHIM'
#!/bin/bash
d=$FAKE_DIR/state; mkdir -p "$d/c"
echo "docker $*" >>"$FAKE_DIR/docker.log"
case $1 in
  info | version | exec) exit 0 ;;
  run)
    name= args=("$@")
    for ((i = 0; i < ${#args[@]}; i++)); do [[ ${args[i]} == --name ]] && name=${args[i + 1]}; done
    [[ -n $name ]] || exit 125
    (set -C; echo "$*" >"$d/c/$name") 2>/dev/null || { echo "Conflict. The container name \"/$name\" is already in use" >&2; exit 125; }
    setsid python3 -I "$FAKE_DIR/listener.py" "$d/c/$name.port" "$d/c/$name.pid" >/dev/null 2>&1 &
    for _ in $(seq 1 200); do [[ -s $d/c/$name.port ]] && break; /usr/bin/sleep 0.05; done
    echo "$name $(cat "$d/c/$name.port")" >>"$FAKE_DIR/ports.log"
    echo stand-in-container ;;
  port) echo "127.0.0.1:$(cat "$d/c/$2.port" 2>/dev/null)" ;;
  rm)
    shift
    for n in "$@"; do
      [[ $n == -* ]] && continue
      [[ -f $d/c/$n.pid ]] && kill "$(cat "$d/c/$n.pid")" 2>/dev/null
      rm -f "$d/c/$n" "$d/c/$n.port" "$d/c/$n.pid"
    done ;;
esac
exit 0
SHIM
cat >"$lab/listener.py" <<'PY'
import http.server, os, sys
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200); self.send_header("Content-Length", "0"); self.end_headers()
    def log_message(self, *a): pass
s = http.server.ThreadingHTTPServer(("127.0.0.1", 0), H)
open(sys.argv[2], "w").write(str(os.getpid()))
open(sys.argv[1], "w").write(str(s.server_address[1]))
s.serve_forever()
PY
cat >"$lab/bin/ffmpeg" <<'SHIM'
#!/bin/bash
case " $* " in
  *" -encoders "*) printf ' V....D libx264 x\n V....D libx265 x\n V....D libsvtav1 x\n' ;;
  *" -filters "*) printf ' T.. subtitles V->V x\n' ;;
esac
SHIM
printf '#!/bin/bash\nexit 0\n' >"$lab/bin/ffprobe"
printf '#!/bin/bash\necho "/usr/share/fonts/dejavu.ttf: DejaVu Sans"\n' >"$lab/bin/fc-list"
printf '#!/bin/bash\nexec /usr/bin/sleep 0.02\n' >"$lab/bin/sleep"
cat >"$lab/bin/go" <<'SHIM'
#!/bin/bash
case $1 in
  tool) [[ $2 == gotestsum ]] && exec python3 -I "$FAKE_DIR/gotestsum.py" "$@" ;;
  test)
    [[ " $* " == *" -list "* ]] && exec python3 -I "$FAKE_DIR/list.py" "$@"
    [[ " $* " == *" -race "* ]] && { echo "race GOFLAGS=$GOFLAGS $*" >>"$FAKE_DIR/race.log"; exit 0; } ;;
esac
exec "$REAL_GO" "$@"
SHIM
chmod +x "$lab"/bin/{docker,ffmpeg,ffprobe,fc-list,sleep,go}
cat >"$lab/list.py" <<'PY'
import os, sys
tests = {}
for line in open(os.environ["FAKE_DIR"] + "/all.txt"):
    p, t = line.rstrip("\n").split(" ", 1)
    tests.setdefault(p, []).append(t)
args, pkgs, i = sys.argv[2:], [], 0
while i < len(args):
    if args[i] in ("-tags", "-list"): i += 2
    elif args[i].startswith("-"): i += 1
    else: pkgs.append(args[i].removeprefix("./").removeprefix("jellyscale/")); i += 1
for p in (sorted(tests) if pkgs == ["..."] else pkgs):
    if p in tests:
        print("\n".join(tests[p])); print("ok  \tjellyscale/%s\t0.01s" % p)
PY
cat >"$lab/gotestsum.py" <<'PY'
# The stand-in for `go tool gotestsum ... -- <go test arguments>`: writes the --jsonfile a go test run of the tests the
# arguments select would, and records the run. FAKE_WANT_PROCS processes wait for each other before any ends.
import fcntl, json, os, re, sys, time
fake = os.environ["FAKE_DIR"]
argv = sys.argv[3:]
sep = argv.index("--")
gts, gt = argv[:sep], argv[sep + 1:]
jsonfile = gts[gts.index("--jsonfile") + 1]
run, pkgs, i = None, [], 0
while i < len(gt):
    if gt[i] == "-run": run = gt[i + 1]; i += 2
    elif gt[i] in ("-tags", "-p", "-timeout"): i += 2
    elif gt[i].startswith("-"): i += 1
    else: pkgs.append(gt[i].removeprefix("./").removeprefix("jellyscale/")); i += 1
tests = {}
for line in open(fake + "/all.txt"):
    p, t = line.rstrip("\n").split(" ", 1)
    tests.setdefault(p, []).append(t)
mine = [(p, t) for p in pkgs for t in tests.get(p, []) if run is None or re.search(run, t)]
alone = set()
if os.environ.get("FAKE_ALONE") and os.path.exists(os.environ["FAKE_ALONE"]):
    for l in open(os.environ["FAKE_ALONE"]):
        f = l.split(None, 2)
        if len(f) >= 2 and not l.startswith("#"): alone.add((f[0], f[1]))
is_alone = bool(mine) and all(m in alone for m in mine)
key = os.environ.get("JELLYSCALE_TEST_DATABASE_URL", "")
start = time.time()
def locked(name, fn):
    with open(fake + "/" + name, "a+") as f:
        fcntl.flock(f, fcntl.LOCK_EX); f.seek(0); r = fn(f); f.flush(); fcntl.flock(f, fcntl.LOCK_UN); return r  # flushed before the unlock: the next process reads what this one wrote
rank = -1
def note_clash(what): open(fake + "/clash.txt", "a").write(what + "\n")
def midrun(): # a share that registered and has not finished
    return [r for r in os.listdir(fake) if r.startswith("run.") and not os.path.exists(fake + "/done." + r[4:])]
if is_alone:
    # Stay until every share has registered and ended, so a build that starts this process beside the shares meets
    # them whichever starts first, and say so when any share is running or registers meanwhile (the shares end only
    # once all of them are running). A build that runs this process before the shares leaves at the bound.
    open(fake + "/alone-running", "w").write("x")
    until = time.time() + 2.0
    while time.time() < until:
        if midrun(): note_clash("a share was running during the alone phase: %s" % midrun()); break
        if len(open(fake + "/reg.txt").read().split() if os.path.exists(fake + "/reg.txt") else []) >= int(os.environ.get("FAKE_WANT_PROCS", "1")): break
        time.sleep(0.05)
    if os.path.exists(fake + "/alone-running"): os.remove(fake + "/alone-running")
if not is_alone:
    def reg(f):
        keys = f.read().split()
        if key not in keys: f.write(key + "\n"); keys.append(key); return keys.index(key), True
        return keys.index(key), False
    rank, first = locked("reg.txt", reg)
    if first:
        want = int(os.environ.get("FAKE_WANT_PROCS", "1"))
        deadline = time.time() + float(os.environ.get("FAKE_BARRIER_S", "60"))
        while len(open(fake + "/reg.txt").read().split()) < want:
            if time.time() > deadline:
                sys.stderr.write("stand-in: only %d of %d processes were running at once\n" % (len(open(fake + "/reg.txt").read().split()), want))
                open(fake + "/barrier-timeout", "w").write("x"); sys.exit(3)
            time.sleep(0.05)
    if os.path.exists(fake + "/done.%d" % rank): os.remove(fake + "/done.%d" % rank)
    deadline_end = time.time() + float(os.environ.get("FAKE_BARRIER_S", "60"))
    open(fake + "/run.%d" % rank, "w").write("x")
    if os.path.exists(fake + "/alone-running"): note_clash("a share registered while the alone process was running")
    if not (os.environ.get("TMPDIR") and os.path.isdir(os.environ["TMPDIR"]) and os.path.isdir(os.path.dirname(jsonfile) or ".")):
        sys.stderr.write("stand-in: TMPDIR or the record's directory does not exist\n"); sys.exit(4)
    if os.environ.get("FAKE_HOLD"):
        open(fake + "/held.%d" % rank, "w").write("x")
        time.sleep(120)
    # Ranks end in order, so "first" and "last" to end are rank 0 and the last: each waits for the one before it.
    while rank > 0 and not os.path.exists(fake + "/done.%d" % (rank - 1)) and time.time() < deadline_end:
        time.sleep(0.02)
faults = {}
for f in filter(None, os.environ.get("FAKE_FAULT", "").split(",")):
    kind, who = f.split("@")
    faults.setdefault(kind, set()).add(who)
def has(kind):
    if is_alone or kind not in faults: return False
    nproc = int(os.environ.get("FAKE_WANT_PROCS", "1"))
    return str(rank) in faults[kind] or ("first" in faults[kind] and rank == 0) or ("last" in faults[kind] and rank == nproc - 1)
ran, status, events = list(mine), 0, []
pkgfail = set()
def ev(**k): events.append(dict(Time="2026-11-16T00:00:00Z", **k))
if has("drop"): open(fake + "/dropped", "w").write("%s %s" % mine[0]); ran = ran[1:]
if has("extra") and os.path.exists(fake + "/dropped"): ran.append(tuple(open(fake + "/dropped").read().split(" ", 1)))
if has("short0"): ran = ran[:-1]
if has("exit1"): ran, status = [], 1
if has("panic"): ran, status = ran[:len(ran) // 2], 2
for j, (p, t) in enumerate(ran):
    P = "jellyscale/" + p
    ev(Action="run", Package=P, Test=t)
    if j == 0 and has("fail"):
        ev(Action="output", Package=P, Test=t, Output="--- FAIL: %s (1.00s)\n    stand-in: injected failure\n" % t)
        ev(Action="fail", Package=P, Test=t, Elapsed=1.0); pkgfail.add(P); status = 1
        open(fake + "/faulty", "a").write("fail %s %s\n" % (rank, t))
    elif j == 0 and has("skip"):
        ev(Action="skip", Package=P, Test=t, Elapsed=1.0)
        open(fake + "/faulty", "a").write("skip %s %s\n" % (rank, t))
    else:
        ev(Action="pass", Package=P, Test=t, Elapsed=1.0)
for P in sorted({"jellyscale/" + p for p, _ in mine}):
    if has("panic"):
        ev(Action="output", Package=P, Output="panic: stand-in\n"); ev(Action="fail", Package=P, Elapsed=1.0)
    else:
        ev(Action="fail" if P in pkgfail else "pass", Package=P, Elapsed=1.0)
with open(jsonfile, "w") as f:
    for e in events: f.write(json.dumps(e) + "\n")
rec = dict(key=key, rank=rank, alone=is_alone, start=start, end=time.time(), tmpdir=os.environ.get("TMPDIR"), jsonfile=jsonfile,
           goflags=os.environ.get("GOFLAGS", ""), ran=[list(x) for x in ran], share=[list(x) for x in mine], args=gt)
locked("inv.jsonl", lambda f: f.write(json.dumps(rec) + "\n"))
if rank >= 0: open(fake + "/done.%d" % rank, "w").write("x")
sys.exit(status)
PY
printf '%s\n' "$all" >"$lab/all.txt"

# One run of test/integration.sh with the stand-ins: js130_run <name> VAR=value...; output, status and logs in $lab/<name>.
js130_run() {
  local name=$1; shift
  rm -rf "${lab:?}/$name"; mkdir -p "$lab/$name/logs"
  cp "$lab/all.txt" "$lab/$name/all.txt"; for f in listener.py gotestsum.py list.py; do cp "$lab/$f" "$lab/$name/$f"; done
  (cd "$repo" && /usr/bin/env -u INTEGRATION_SHARD -u INTEGRATION_PROCS -u INTEGRATION_PROC -u GITHUB_ACTIONS -u GITHUB_STEP_SUMMARY -u GOFLAGS \
    GIT_DIR=/nonexistent PATH="$lab/bin" REAL_GO="$real_go" FAKE_DIR="$lab/$name" FAKE_ALONE="${SHARD_ALONE:-$repo/test/alone.txt}" FAKE_BARRIER_S=15 \
    JELLYSCALE_TEST_ROOT="$lab/$name/root" JELLYSCALE_LOG_DIR="$lab/$name/logs" "$@" \
    timeout 300 bash test/integration.sh) >"$lab/$name/out" 2>&1
  echo $? >"$lab/$name/status"
}
js130_verify() { # name shard procs [goflags]: what a run that passed left, one "<what>\t<ok|why>" line each
  FAKE_ALONE="${SHARD_ALONE:-$repo/test/alone.txt}" python3 -I - "$lab/$1" "$2" "$3" "${4:-}" "$tmp/js130-want-$(echo "$2" | tr / _)" <<'PY'
import collections, glob, json, os, re, sys
fake, shard, n, goflags, wantfile = sys.argv[1:6]; n = int(n)
want = [tuple(l.split(" ", 1)) for l in open(wantfile).read().splitlines() if l]
alone = set()
if os.environ.get("FAKE_ALONE") and os.path.exists(os.environ["FAKE_ALONE"]):
    for l in open(os.environ["FAKE_ALONE"]):
        f = l.split(None, 2)
        if len(f) >= 2 and not l.startswith("#"): alone.add((f[0], f[1]))
inv = [json.loads(l) for l in open(fake + "/inv.jsonl")] if os.path.exists(fake + "/inv.jsonl") else []
out = []
def res(what, problem=None): out.append("%s\t%s" % (what, problem or "ok"))
main = [i for i in inv if not i["alone"]]; al = [i for i in inv if i["alone"]]
workers = collections.OrderedDict()
for i in main: workers.setdefault(i["key"], []).append(i)
res("%d test processes ran" % n, None if len(workers) == n else "%d ran" % len(workers))
pg = [l for l in open(fake + "/docker.log") if l.startswith("docker run ") and " postgres:" in l]
names = [re.search(r"--name (\S+)", l).group(1) for l in pg]
res("%d Postgres containers were started" % n, None if len(pg) == n else "%d were" % len(pg))
res("... under names that differ", None if len(set(names)) == len(names) else "names repeat: %s" % names)
res("... each on a Docker-assigned port (-p 127.0.0.1::5432)", None if all("-p 127.0.0.1::5432" in l for l in pg) else "a port is fixed or not published")
ports = dict(l.split() for l in open(fake + "/ports.log"))
urlports = [re.search(r"@127\.0\.0\.1:(\d+)/", k).group(1) if re.search(r"@127\.0\.0\.1:(\d+)/", k) else "?" for k in workers]
res("each process has its own database URL, on one of those ports", None if len(set(urlports)) == n and set(urlports) <= {ports[x] for x in names} else "urls on ports %s, containers on %s" % (urlports, [ports[x] for x in names]))
tmps = [v[0]["tmpdir"] for v in workers.values()]
res("each process has its own TMPDIR", None if len(set(tmps)) == len(tmps) and all(tmps) else "TMPDIRs %s" % tmps)
files = [{x["jsonfile"] for x in v} for v in workers.values()]
res("each process has its own test record (--jsonfile)", None if sum(len(f) for f in files) == len(set().union(*files)) else "two processes wrote one file")
c = collections.Counter(tuple(t) for i in inv for t in i["ran"])
w = collections.Counter(want)
res("every test of the shard ran once over the processes and the alone phase", None if c == w else "missing %s; twice or foreign %s" % (sorted((w - c).elements())[:3], sorted((c - w).elements())[:3]))
shares = [collections.Counter(tuple(t) for x in v for t in x["share"]) for v in workers.values()]
res("the shares are disjoint and none holds an alone test", None if all(not (a & b) for k, a in enumerate(shares) for b in shares[k + 1:]) and all(not (set(s) & alone) for s in shares) else "overlap")
res("each process ran its whole share", None if all(collections.Counter(tuple(t) for x in v for t in x["ran"]) == collections.Counter(tuple(t) for x in v for t in x["share"]) for v in workers.values()) else "a process ran other than its share")
firsts = [min(x["start"] for x in v) for v in workers.values()]
ends = [min(x["end"] for x in v) for v in workers.values()]
res("the processes ran at the same time (all started before any ended)", None if workers and max(firsts) < min(ends) else "no common instant")
flags = [i["goflags"] for i in inv] + [l.split()[1] for l in open(fake + "/race.log")] if os.path.exists(fake + "/race.log") else [i["goflags"] for i in inv]
res("every go test process got the caller's GOFLAGS", None if flags and all(f == "GOFLAGS=" + goflags or f == goflags for f in flags) else "flags seen %s" % set(flags))
for a in al:
    clash = [b for b in inv if b is not a and a["start"] < b["end"] and b["start"] < a["end"]]
    marks = open(fake + "/clash.txt").read().splitlines() if os.path.exists(fake + "/clash.txt") else []
    res("an alone test ran with no other test process running", None if not clash and not marks else "%d others overlapped it; %s" % (len(clash), marks[:2]))
    res("... and only alone tests ran in that process", None if all(tuple(t) in alone for t in a["ran"]) else "mixed")
res("alone tests of the shard each ran in the alone phase", None if {tuple(t) for a in al for t in a["ran"]} == {x for x in alone if x in set(want)} else "alone ran %s" % [a["ran"] for a in al])
left = [f for f in os.listdir(fake + "/state/c")] if os.path.isdir(fake + "/state/c") else []
res("no container the run started is left", None if not left else "left: %s" % left)
res("no listener of a removed container is left", None if not [p for p in glob.glob(fake + "/state/c/*.pid")] else "pid files left")
tj = []
for f in glob.glob(fake + "/logs/**/tests.json", recursive=True):
    tj += [json.loads(l) for l in open(f) if l.strip()]
top = collections.Counter((e["Package"].removeprefix("jellyscale/"), e["Test"]) for e in tj if e.get("Action") == "pass" and e.get("Test") and "/" not in e["Test"])
res("the tests.json files hold each test once", None if top == w else "differs: missing %s, repeated or foreign %s" % (sorted((w - top).elements())[:3], sorted((top - w).elements())[:3]))
recs = glob.glob(fake + "/logs/**/shard-integration-*.txt", recursive=True)
sh = shard.replace("/", "of")
res("one shard-integration-%s.txt, counting every test of the shard" % sh, None if len(recs) == 1 and open(recs[0]).read().split() == ["integration", shard, str(len(want))] and recs[0].endswith("shard-integration-%s.txt" % sh) else "records %s: %s" % ([os.path.basename(r) for r in recs], [open(r).read() for r in recs]))
print("\n".join(out))
PY
}
js130_report() { # prefix: "<prefix> <what>" is checked from js130_verify's lines on stdin
  local prefix=$1 what verdict
  while IFS=$'\t' read -r what verdict; do [[ -n $what ]] && check "$prefix $what" ok "$verdict"; done
}
js130_want() { bash test/shard.sh list "$1" >"$tmp/js130-want-$(echo "$1" | tr / _)"; }
cnt() { local c; c=$(grep -c "$@" 2>/dev/null); echo "${c:-0}"; }

dealt_first=$(echo "$dealt_shards" | head -1)
for s in $dealt_first rest; do js130_want "$s"; done
# 1. A passing run, 2 and 4 processes, a dealt shard and rest.
for s in $dealt_first rest; do
  for n in 2 4; do
    tag="$(echo "$s" | tr / _)-$n"
    js130_run "ok-$tag" INTEGRATION_SHARD="$s" INTEGRATION_PROCS=$n FAKE_WANT_PROCS=$n GOFLAGS=-trimpath
    check "INTEGRATION_PROCS=$n, shard $s: the run passes" 0 "$(cat "$lab/ok-$tag/status")"
    js130_verify "ok-$tag" "$s" $n -trimpath 2>&1 | js130_report "INTEGRATION_PROCS=$n, shard $s:"
    check "INTEGRATION_PROCS=$n, shard $s: the report counts every process's tests" "yes" "$(grep -q ": $(wc -l <"$tmp/js130-want-$(echo "$s" | tr / _)" | tr -d ' ') tests;" "$lab/ok-$tag/out" && echo yes || echo no)"
    race=$(wc -l <"$lab/ok-$tag/race.log" 2>/dev/null || echo 0)
    check "INTEGRATION_PROCS=$n, shard $s: the -race TestHarness run is made once in the job that holds cmd/jellyscale, else never" "$([[ $s == rest ]] && echo 1 || echo 0)" "$(echo $race)"
  done
done
# 2. Unset, 1: as today. A bad value is refused, naming it, before anything is started.
for v in "" 1; do
  js130_run "as-today-${v:-unset}" INTEGRATION_SHARD="$dealt_first" ${v:+INTEGRATION_PROCS=$v} FAKE_WANT_PROCS=1
  check "INTEGRATION_PROCS=${v:-unset}: the run passes with one Postgres and one go test process at a time" "0 1 1" "$(cat "$lab/as-today-${v:-unset}/status") $(cnt '^docker run .* postgres:' "$lab/as-today-${v:-unset}/docker.log") $(python3 -I -c 'import json,sys; print(len({json.loads(l)["key"] for l in open(sys.argv[1])}))' "$lab/as-today-${v:-unset}/inv.jsonl")"
done
for v in 0 -1 1.5 two 2x; do
  js130_run "bad-$v" INTEGRATION_SHARD="$dealt_first" INTEGRATION_PROCS="$v" FAKE_WANT_PROCS=2
  check "INTEGRATION_PROCS=$v is refused, naming it, and nothing is started" "nonzero named 0" "$([[ $(cat "$lab/bad-$v/status") -ne 0 ]] && echo nonzero || echo "exit 0") $(grep -qF -- "$v" "$lab/bad-$v/out" && echo named || echo "not named") $(cnt '^docker run' "$lab/bad-$v/docker.log")"
done
# 3. One process's share: INTEGRATION_PROC=k with INTEGRATION_PROCS=n runs process k alone, over the share shard.sh gives it.
for s in $dealt_first rest; do
  js130_run "one-$(echo "$s" | tr / _)" INTEGRATION_SHARD="$s" INTEGRATION_PROCS=4 INTEGRATION_PROC=1 FAKE_WANT_PROCS=1
  od="$lab/one-$(echo "$s" | tr / _)"
  got=$(python3 -I -c 'import json,sys; print("\n".join(sorted(" ".join(t) for l in open(sys.argv[1]) for t in json.loads(l)["ran"])))' "$od/inv.jsonl")
  share1=$(bash test/shard.sh list "$s" 1/4 | LC_ALL=C sort)
  same=no; [[ $got == "$share1" && $(wc -l <<<"$share1") -lt $(bash test/shard.sh count "$s") ]] && same=yes
  check "INTEGRATION_PROC=1 INTEGRATION_PROCS=4, shard $s: one Postgres, and exactly the tests of share 1/4 run" "0 1 yes" "$(cat "$od/status") $(cnt '^docker run .* postgres:' "$od/docker.log") $same"
done
# 4. The shares: shard.sh gives each process a share, the same every time, every test once, none an alone test.
for s in $dealt_first rest; do
  for m in 2 4; do
    union=$(for ((k = 0; k < m; k++)); do bash test/shard.sh list "$s" $k/$m; done | LC_ALL=C sort)
    alone_list=$(bash test/shard.sh list "$s" alone 2>/dev/null | LC_ALL=C sort)
    whole=$(bash test/shard.sh list "$s" | LC_ALL=C sort)
    check "shard.sh: the $m shares of $s and its alone tests are the whole shard, each test once" "$whole" "$(printf '%s\n%s\n' "$union" "$alone_list" | sed '/^$/d' | LC_ALL=C sort)"
    check "shard.sh: the $m shares of $s are the same when dealt again" "$(bash test/shard.sh list "$s" 1/$m)" "$(bash test/shard.sh list "$s" 1/$m)"
    check "shard.sh: count $s 0/$m is the number of lines list gives" "$(bash test/shard.sh list "$s" 0/$m | wc -l | tr -d ' ')" "$(bash test/shard.sh count "$s" 0/$m)"
    check "shard.sh: every share of $s at $m is more than empty" "" "$(for ((k = 0; k < m; k++)); do [[ $(bash test/shard.sh count "$s" $k/$m) -gt 0 ]] || echo "share $k/$m"; done)"
  done
done
fails_with "shard.sh refuses a share past m" bash test/shard.sh list "$dealt_first" 2/2
# The deal inside a job, by time, longest first: on the stand-in module, one share holds the slowest test alone.
printf '# stand-in\ninternal/transcode TestA 50\ninternal/api TestC 40\ninternal/transcode TestB 12\ninternal/transcode TestSame 10\ninternal/api TestSame 9\ninternal/transcode TestNew 8\n' >"$tmp/mod-times-share.txt"
mshare() { (cd "$mod" && SHARD_TIMES="$tmp/mod-times-share.txt" bash test/shard.sh list "$1" "$2" | LC_ALL=C sort); }
check "shard.sh: dealt longest first, the slowest test of shard 0/2 is alone on its share of 2 (TestA 50 s against 10 s and 8 s)" "internal/transcode TestA" "$(mshare 0/2 0/2)"
check "shard.sh: ... and the other share holds the rest" "internal/transcode TestNew
internal/transcode TestSame" "$(mshare 0/2 1/2)"
check "shard.sh: the stand-in module's shares hold each of a shared test name once, in its own package" "internal/api TestSame
internal/transcode TestSame" "$( { for sh in 0/2 1/2; do for k in 0/2 1/2; do mshare $sh $k; done; done; } | grep ' TestSame$' | LC_ALL=C sort)"
# Per-share time guards: each process's share, 1500 s and two thirds of the -timeout, naming the shard and process.
printf '# stand-in\ninternal/transcode TestA 2000\ninternal/api TestC 40\ninternal/transcode TestB 12\ninternal/transcode TestSame 10\ninternal/api TestSame 9\ninternal/transcode TestNew 8\n' >"$tmp/mod-times-cap.txt"
check "the 1500 s guard, applied to a share, names the shard and the process that is over" "shard 0/1 process 0/2: 2000.0s is over 1500s" "$(cd "$mod" && SHARD_TIMES="$tmp/mod-times-cap.txt" bash -c "$(declare -f shard_secs share_job_secs cap_violations shares_of procs_of); shard_procs='0/1 2'; cap_violations '$mod' '$tmp/mod-times-cap.txt' 0/1")"
# A listed alone test counts toward every share of its job: TestB (12 s) alone, shares of 2000 s and 40 s... the 1500 s guard
# and the budget guard both name the process whose share plus the alone seconds is over.
printf '# stand-in\ninternal/transcode TestA 1400\ninternal/api TestC 40\ninternal/transcode TestB 150\ninternal/transcode TestSame 10\ninternal/api TestSame 9\ninternal/transcode TestNew 8\n' >"$tmp/mod-times-alone.txt"
printf '# stand-in\ninternal/transcode TestB because the stand-in says so\n' >"$tmp/mod-alone-guard.txt"
guard_fns="$(declare -f shard_secs share_job_secs cap_violations budget_violations shares_of procs_of)"
check "the 1500 s guard counts the shard's alone tests: share 1400 s plus 150 s alone is over" "shard 0/1 process 0/2: 1550.0s is over 1500s" "$(cd "$mod" && SHARD_ALONE="$tmp/mod-alone-guard.txt" SHARD_TIMES="$tmp/mod-times-alone.txt" bash -c "$guard_fns; shard_procs='0/1 2'; cap_violations '$mod' '$tmp/mod-times-alone.txt' 0/1")"
check "... and without the alone test the same share is within it" "" "$(cd "$mod" && SHARD_ALONE=/nonexistent SHARD_TIMES="$tmp/mod-times-alone.txt" bash -c "$guard_fns; shard_procs='0/1 2'; cap_violations '$mod' '$tmp/mod-times-alone.txt' 0/1")"
check "the two-thirds-of-the--timeout guard counts the shard's alone tests: 1550 s of a 2250 s limit's 1500 s" "shard 0/1 process 0/2: 1550.0s is over two thirds of 2250s" "$(cd "$mod" && SHARD_ALONE="$tmp/mod-alone-guard.txt" SHARD_TIMES="$tmp/mod-times-alone.txt" bash -c "$guard_fns; shard_procs='0/1 2'; budget_violations '$mod' '$tmp/mod-times-alone.txt' 2250 0/1")"
check "the per-share guards use the matrix's processes: every dealt shard and rest runs 2 or more" "" "$(awk '$2 < 2 { print "shard " $1 " has procs " $2 }' <<<"$shard_procs")"

# 5. Faults: the first or the last process to end fails, skips, stops partway, ends early or counts wrong; the run fails,
# and the other processes' results are still there. Dealt shard at 2 processes, rest at 4.
js130_fault() { # name shard procs fault...  (fault is FAKE_FAULT)
  local name=$1 shard=$2 n=$3 fault=$4
  js130_run "$name" INTEGRATION_SHARD="$shard" INTEGRATION_PROCS=$n FAKE_WANT_PROCS=$n FAKE_FAULT="$fault"
  local st; st=$(cat "$lab/$name/status")
  check "$name: the run fails" "nonzero" "$([[ $st -ne 0 ]] && echo nonzero || echo "exit $st")"
  check "$name: no stand-in Postgres is left" "" "$(ls "$lab/$name/state/c" 2>/dev/null)"
  # The tests the other processes ran are in the records.
  python3 -I - "$lab/$name" "$fault" "$n" <<'PY' | js130_report "$name:"
import glob, json, sys
fake, fault, n = sys.argv[1], sys.argv[2], int(sys.argv[3])
bad = {int(w) if w.isdigit() else (0 if w == "first" else n - 1) for w in (f.split("@")[1] for f in fault.split(","))}
inv = [json.loads(l) for l in open(fake + "/inv.jsonl")]
ok = {(e["Package"].removeprefix("jellyscale/"), e["Test"]) for f in glob.glob(fake + "/logs/**/tests.json", recursive=True) for l in open(f) if l.strip() for e in [json.loads(l)] if e.get("Action") == "pass" and e.get("Test")}
miss = [t for i in inv if i["rank"] not in bad and not i["alone"] for t in map(tuple, i["share"]) if t not in ok]
print("the other processes' passing tests are in the test records\t%s" % ("ok" if not miss else "missing %s" % miss[:3]))
PY
  if [[ $fault == fail@* || $fault == skip@* ]]; then
    check "$name: the failing test is named" "yes" "$(t=$(awk '{print $3}' "$lab/$name/faulty" | head -1); [[ -n $t ]] && grep -qF -- "$t" "$lab/$name/out" && echo yes || echo "no: ${t:-none}")"
  fi
}
for s_n in "$dealt_first 2" "rest 4"; do
  read -r fs fn <<<"$s_n"
  for who in first last; do
    for kind in fail skip panic short0 exit1; do
      [[ $fs == rest && $kind != fail && $kind != short0 ]] && continue
      js130_fault "fault-$kind-$who-$(echo "$fs" | tr / _)" "$fs" "$fn" "$kind@$who"
    done
  done
done
js130_fault "fault-both-ends" rest 4 "fail@first,fail@last"
check "fault-both-ends: the failures of both processes are named" "2" "$(awk '{print $3}' "$lab/fault-both-ends/faulty" | while read -r t; do grep -cF -- "$t" "$lab/fault-both-ends/out" | awk '{print ($1>0)}'; done | awk '{ s += $1 } END { print s + 0 }')"
# A test run by the wrong process: the shard's count adds up, one process's does not.
js130_fault "fault-swap-$(echo "$dealt_first" | tr / _)" "$dealt_first" 2 "drop@first,extra@last"

# 6. The processes really run at once: the stand-ins wait for each other, so a run that starts them one after the other fails.
js130_run "serial" INTEGRATION_SHARD="$dealt_first" INTEGRATION_PROCS=2 FAKE_WANT_PROCS=3 FAKE_BARRIER_S=3
check "a run whose processes cannot all be running at once fails (the stand-ins wait for a third)" "nonzero yes" "$([[ $(cat "$lab/serial/status") -ne 0 ]] && echo nonzero || echo "exit 0") $([[ -e $lab/serial/barrier-timeout ]] && echo yes || echo no)"

# 7. alone: a test of test/alone.txt runs once, in the shard's job, with no other test process running, and is counted once.
alone_test=$(bash test/shard.sh list rest | sed -n '1p')
printf '# stand-in\n%s because the stand-in says so\n' "$alone_test" >"$tmp/alone-standin.txt"
export SHARD_ALONE="$tmp/alone-standin.txt"
js130_run "alone-rest" INTEGRATION_SHARD=rest INTEGRATION_PROCS=4 FAKE_WANT_PROCS=4
check "alone: a run of rest with one test listed passes" 0 "$(cat "$lab/alone-rest/status")"
FAKE_ALONE=$SHARD_ALONE js130_verify alone-rest rest 4 | js130_report "alone, rest at 4:"
check "alone: shard.sh lists that one test as alone, and in no share" "1 0" "$(bash test/shard.sh list rest alone | grep -cxF -- "$alone_test") $(for k in 0 1 2 3; do bash test/shard.sh list rest $k/4; done | grep -cxF -- "$alone_test")"
check "alone: the shard's count still counts it once" "$(wc -l <"$tmp/js130-want-rest" | tr -d ' ')" "$(bash test/shard.sh count rest)"
unset SHARD_ALONE
# test/alone.txt itself.
check "test/alone.txt exists" yes "$([[ -f test/alone.txt ]] && echo yes || echo no)"
check "every line of test/alone.txt has a package, a test and a reason" "" "$(grep -v '^#' test/alone.txt 2>/dev/null | grep -v '^[[:space:]]*$' | awk 'NF < 3 { print "no reason: " $0 }')"
check "every test test/alone.txt names is a test of the tier" "" "$(grep -v '^#' test/alone.txt 2>/dev/null | awk 'NF >= 2 { print $1, $2 }' | while read -r p t; do grep -qxF -- "$p $t" <<<"$all" || echo "not a test: $p $t"; done)"
check "no test of test/alone.txt is in a share of any shard of the matrix" "" "$(while read -r sh share; do bash test/shard.sh list "$sh" ${share:+$share}; done < <(for s in $matrix; do shares_of "$s"; done) | grep -vE '^$' | LC_ALL=C sort | LC_ALL=C comm -12 - <(grep -v '^#' test/alone.txt 2>/dev/null | awk 'NF >= 2 { print $1, $2 }' | LC_ALL=C sort))"
check "the scan-cost test is listed in test/alone.txt, or scanTrackCost is at most 12 MB" "yes" "$(grep -q 'TestTheScanReadsNoMoreOfAFilmForItsImageTrackAndNothingThatGrowsWithTheFilm' test/alone.txt 2>/dev/null && echo yes || { grep -qE 'const scanTrackCost = (12|[0-9]) << 20' internal/transcode/image_burn_scan_integration_test.go && echo yes || echo no; })"

# 8. Interrupted: a run stopped by a signal while its processes run leaves no container.
for sig in TERM HUP; do
  nm="interrupt-$sig"; rm -rf "${lab:?}/$nm"; mkdir -p "$lab/$nm/logs"
  cp "$lab/all.txt" "$lab/listener.py" "$lab/gotestsum.py" "$lab/list.py" "$lab/$nm/"
  /usr/bin/env -u INTEGRATION_PROC GIT_DIR=/nonexistent PATH="$lab/bin" REAL_GO="$real_go" FAKE_DIR="$lab/$nm" FAKE_ALONE="$repo/test/alone.txt" \
    JELLYSCALE_TEST_ROOT="$lab/$nm/root" JELLYSCALE_LOG_DIR="$lab/$nm/logs" INTEGRATION_SHARD="$dealt_first" INTEGRATION_PROCS=3 FAKE_WANT_PROCS=3 FAKE_HOLD=1 \
    bash test/integration.sh >"$lab/$nm/out" 2>&1 &
  pid=$!
  for _ in $(seq 1 600); do [[ $(ls "$lab/$nm" | grep -c '^held\.') -ge 3 ]] && break; /usr/bin/sleep 0.1; done
  held=$(ls "$lab/$nm" | grep -c '^held\.')
  kill -$sig "$pid" 2>/dev/null
  for _ in $(seq 1 300); do kill -0 "$pid" 2>/dev/null || break; /usr/bin/sleep 0.1; done
  kill -0 "$pid" 2>/dev/null && { kill -KILL "$pid" 2>/dev/null; echo "run did not stop" >"$lab/$nm/hung"; }
  pkill -f "$lab/$nm/gotestsum.py" 2>/dev/null
  check "interrupt $sig: all 3 processes were running before the signal" 3 "$held"
  check "interrupt $sig: the run stops on the signal" "stopped" "$([[ -e $lab/$nm/hung ]] && echo hung || echo stopped)"
  check "interrupt $sig: no Postgres container is left" "" "$(ls "$lab/$nm/state/c" 2>/dev/null | grep -v '\.' )"
  for pidf in "$lab/$nm"/state/c/*.pid; do [[ -f $pidf ]] && kill "$(cat "$pidf")" 2>/dev/null; done
done

# 9. What CI reads: every shard of the matrix at its processes, the records added up, tests.json read whole.
sd="$tmp/js130-ci"; mkdir -p "$sd/records" "$sd/run1" "$sd/logs-by-shard"
total_all=$(wc -l <<<"$all" | tr -d ' ')
for s in $matrix; do
  n=$(procs_of "$s"); n=${n:-2}; [[ $n -ge 2 ]] || n=2
  js130_want "$s"
  js130_run "ci-$(echo "$s" | tr / _)" INTEGRATION_SHARD="$s" INTEGRATION_PROCS=$n FAKE_WANT_PROCS=$n
  check "matrix shard $s at $n processes passes" 0 "$(cat "$lab/ci-$(echo "$s" | tr / _)/status")"
  js130_verify "ci-$(echo "$s" | tr / _)" "$s" $n | js130_report "matrix shard $s at $n processes:"
  cp "$lab/ci-$(echo "$s" | tr / _)"/logs/shard-integration-*.txt "$sd/records/" 2>/dev/null
  mkdir -p "$sd/run1/integration-shard-${s/\//of}"; cp -r "$lab/ci-$(echo "$s" | tr / _)/logs/." "$sd/run1/integration-shard-${s/\//of}/"
done
check "test/check-shards.sh integration passes on the records of the six runs" 0 "$(bash test/check-shards.sh integration "$sd/records" "$total_all" >/dev/null 2>&1; echo $?)"
first_rec=$(ls "$sd/records" | head -1)
mkdir -p "$sd/short"; cp "$sd/records"/* "$sd/short/"; rm "$sd/short/$first_rec"
check "test/check-shards.sh integration still fails when a shard's tests ran in no shard" nonzero "$(bash test/check-shards.sh integration "$sd/short" "$total_all" >/dev/null 2>&1 && echo zero || echo nonzero)"
mkdir -p "$sd/dup"; cp "$sd/records"/* "$sd/dup/"; awk '{ $3 = $3 + 1; print }' "$sd/records/$first_rec" >"$sd/dup/$first_rec"
check "... and when a test ran in two" nonzero "$(bash test/check-shards.sh integration "$sd/dup" "$total_all" >/dev/null 2>&1 && echo zero || echo nonzero)"
SHARD_TIMES_OUT="$sd/times.txt" bash test/shard-times.sh "$sd/run1" >/dev/null 2>&1
check "test/shard-times.sh reads every tests.json whole: one line for each test of the tier, one second each" "$total_all 0" "$(grep -vc '^#' "$sd/times.txt") $(grep -v '^#' "$sd/times.txt" | awk '$3 != 1' | wc -l | tr -d ' ')"
names_args=(); for s in $matrix; do names_args+=("${s/\//of}"); done
summary=$(bash test/shard-summary.sh "$sd/run1" "${names_args[@]}" 2>&1)
check "test/shard-summary.sh reads each shard's tests.json whole: the seconds of a shard are its count of tests" "" "$(for s in $matrix; do want=$(bash test/shard.sh count "$s"); grep -qxF "integration shard ${s/\//of}: ${want}s of tests" <<<"$summary" || echo "$s: want ${want}s, got: $(grep "shard ${s/\//of}:" <<<"$summary")"; done)"
# One name for each container over all the runs above: unique to the run, not to the process count.
check "container names differ in every run (a name unique to its run)" "" "$(grep -h '^docker run' "$lab"/*/docker.log 2>/dev/null | grep -o -- '--name [^ ]*' | sort | uniq -d | head -3)"
pkill -f "python3 -I $lab/" 2>/dev/null # listeners of a build that left its containers: mine to stop
for pidf in "$lab"/*/state/c/*.pid; do [[ -f $pidf ]] && kill "$(cat "$pidf")" 2>/dev/null; done

# 10. ci.yml and docs/testing.md.
ci130=$(python3 -I - .github/workflows/ci.yml <<'PY'
import sys, yaml
job = yaml.safe_load(open(sys.argv[1]))["jobs"]["integration-shard"]
inc = job["strategy"]["matrix"]["include"]
print("at most five entries\t" + ("ok" if len(inc) <= 5 else "%d entries" % len(inc)))
print("every entry has procs of 2 or more\t" + ("ok" if all(int(e.get("procs", 0)) >= 2 for e in inc) else "an entry has fewer"))
print("the job sets INTEGRATION_PROCS from the matrix\t" + ("ok" if "INTEGRATION_PROCS" in str(job) and "matrix.procs" in str(job) else "it does not"))
print("the job's timeout-minutes is not above 60\t" + ("ok" if int(job["timeout-minutes"]) <= 60 else str(job["timeout-minutes"])))
PY
)
while IFS=$'\t' read -r what verdict; do [[ -n $what ]] && check "ci.yml: $what" ok "$verdict"; done <<<"$ci130"
check "test/integration.sh's go test -timeout is still 45m" "45" "$(sed -n 's/.*go tool gotestsum.* -timeout \([0-9]*\)m.*/\1/p' test/integration.sh | head -1)"
d=docs/testing.md
check "docs/testing.md says the tier is split twice: jobs by test/shard.sh, then processes in a job by INTEGRATION_PROCS" "1" "$(grep -c 'INTEGRATION_PROCS' $d | awk '{print ($1>0)}')"
check "docs/testing.md says what test/alone.txt is for" "1" "$(grep -F 'test/alone.txt' $d | grep -ciE 'alone|no other' | awk '{print ($1>0)}')"
check "docs/testing.md gives one command that runs exactly one process's share" "1" "$(grep -E 'INTEGRATION_SHARD=.*INTEGRATION_PROCS=.*INTEGRATION_PROC=|INTEGRATION_PROC=.*INTEGRATION_PROCS=.*INTEGRATION_SHARD=' $d | grep -c 'test/integration.sh' | awk '{print ($1>0)}')"
check "docs/testing.md says a concurrency claim still needs its own real processes or contention" "1" "$(grep -iE 'concurrency' $d | grep -ciE 'real process|contention' | awk '{print ($1>0)}')"

# 11. Every named mutant of test/mutant.py still applies to the code (its anchor is there), so that a refactor that
# removes one is found here by name, not by whoever next runs test/mutants.sh. No test runs under a mutant.
mutant_names=$(python3 -I test/mutant.py none "$repo" "$tmp" 2>&1 | sed -n 's/^usage: mutant.py <\([^>]*\)>.*$/\1/p' | tr '|' ' ')
check "test/mutant.py names its mutants" "ok" "$([[ -n $mutant_names ]] && echo ok)"
for mutant in $mutant_names; do
  mutant_dir=$(mktemp -d "$tmp/mutant.XXXXXX")
  check "mutant $mutant still applies" "" "$(python3 -I test/mutant.py "$mutant" "$repo" "$mutant_dir" 2>&1)"
done

echo
[[ $fails -eq 0 ]] && echo "scripts: all passed" || echo "scripts: $fails failed"
exit $((fails > 0))
