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
shard_secs() { # repo-dir times-file shard
  local dir=$1 times=$2 shard=$3
  SHARD_TIMES=$times bash "$dir/test/shard.sh" list "$shard" | awk -v times="$times" '
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
budget_violations() { # repo-dir times-file limit-seconds shard...: "shard <s>: <secs>s" for each dealt shard over two thirds of the limit
  local dir=$1 times=$2 limit=$3 s secs; shift 3
  for s in "$@"; do
    [[ $s == rest ]] && continue
    secs=$(shard_secs "$dir" "$times" "$s")
    awk -v s="$secs" -v l="$limit" 'BEGIN { exit !(s > l * 2 / 3) }' && echo "shard $s: ${secs}s is over two thirds of ${limit}s"
  done
}
check "no dealt shard's seconds in the times file pass two thirds of go test's -timeout" "" "$(budget_violations "$repo" test/shard-times.txt $((limit_minutes * 60)) $matrix)"
# And none passes 1500 s (25 min): the queue run's 27-minute limit on a shard job less about 2 minutes of job overhead.
cap_violations() { # repo-dir times-file shard...: "shard <s>: <secs>s" for each dealt shard over 1500 s
  local dir=$1 times=$2 s secs; shift 2
  for s in "$@"; do
    [[ $s == rest ]] && continue
    secs=$(shard_secs "$dir" "$times" "$s")
    awk -v s="$secs" 'BEGIN { exit !(s > 1500) }' && echo "shard $s: ${secs}s is over 1500s"
  done
}
check "no dealt shard's seconds in the times file pass 1500 s (the matrix is big enough)" "" "$(cap_violations "$repo" test/shard-times.txt $matrix)"

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
print("five dealt shards plus rest" if len(jobs["integration-shard"]["strategy"]["matrix"]["include"]) == 6 else "matrix changed")
print("cancel-media-jobs kept" if "cancel-media-jobs" in jobs else "cancel-media-jobs gone")
print("integration says why the shards did not run" if "did not run: $WHY" in str(jobs["integration"]["steps"]) else "integration lost its reason")
print("e2e says why the shards did not run" if "did not run: $WHY" in str(jobs["e2e"]["steps"]) else "e2e lost its reason")
EOF
)
check "ci.yml: lint-and-unit runs on every change" "lint-and-unit has no needs or if" "$(sed -n 1p <<<"$js132")"
for j in integration-total integration-shard roles e2e-shard; do
  check "ci.yml: $j is gated on changes as before" "$j still waits for changes and runs only for code" "$(grep "^$j " <<<"$js132")"
done
check "ci.yml: the dealt shards and cancel-media-jobs stay" "five dealt shards plus rest cancel-media-jobs kept" "$(grep -E '^(five dealt|cancel-media)' <<<"$js132" | paste -sd' ')"
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
archives=${APT_ARCHIVES:-/nonexistent}; args=(); download_only=; reinstall=
while [[ $# -gt 0 ]]; do
  case $1 in
    -o) [[ $2 == Dir::Cache::archives=* ]] && archives=${2#*=}; shift 2 ;;
    -o*) [[ $1 == -oDir::Cache::archives=* ]] && archives=${1#*=}; shift ;;
    --download-only | -d) download_only=1; shift ;;
    --reinstall) reinstall=1; shift ;;
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
    if "restore-keys" in c: problems.append("restore-keys would restore another key's files")
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
result("integration.sh passes extra go test arguments in a dealt shard", None if "can't be combined" not in integration_sh and integration_sh.count('"${run[@]}" "$@"') == 1 else "refused or not passed")
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

echo
[[ $fails -eq 0 ]] && echo "scripts: all passed" || echo "scripts: $fails failed"
exit $((fails > 0))
