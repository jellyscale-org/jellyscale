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

# --- shard.sh: every test of the tier lands in one shard, and only one.
cd "$repo" || exit 1
all=$(go test -tags integration -list '.*' ./... | grep -E '^(Test|Example|Fuzz)' | sort)
dealt=$(for i in 0 1 2 3; do bash test/shard.sh args "$i/4" | sed -n 2p | sed 's/^\^(//; s/)\$$//' | tr '|' '\n'; done | sort)
rest_packages=$(bash test/shard.sh args rest)
rest=$(go test -tags integration -list '.*' $rest_packages | grep -E '^(Test|Example|Fuzz)' | sort)
check "the transcode shards and rest hold every test once" "$all" "$(printf '%s\n%s\n' "$dealt" "$rest" | sort)"
check "no transcode test is dealt twice" "" "$(echo "$dealt" | uniq -d)"
check "rest leaves out internal/transcode" "0" "$(echo "$rest_packages" | grep -c '/internal/transcode$')"
counts=$(( $(bash test/shard.sh count 0/4) + $(bash test/shard.sh count 1/4) + $(bash test/shard.sh count 2/4) + $(bash test/shard.sh count 3/4) + $(bash test/shard.sh count rest) ))
check "the shards' counts add up to the tier" "$(echo "$all" | wc -l | tr -d ' ')" "$counts"
fails_with "a shard past n is refused" bash test/shard.sh count 3/3

# --- shard.mjs: each scenario runs once, a worker-stopper last on the last shard.
for i in 0 1 2; do E2E_SHARD=$i/3 node test/e2e/shard_test.mjs >"$tmp/shard$i" 2>&1; done
check "shard 0 runs every third scenario" "run   s1 ran s1 run   s4 ran s4 run   s7 ran s7 shard 0/3: ran 3 of 11" "$(tr '\n' ' ' <"$tmp/shard0" | sed 's/ $//')"
check "shard 2 runs its share, then the worker-stoppers in call order" "run   s3 ran s3 run   s6 ran s6 run   s9 ran s9 run   kill A ran kill A run   kill B ran kill B shard 2/3: ran 5 of 11" "$(tr '\n' ' ' <"$tmp/shard2" | sed 's/ $//')"
check "every scenario ran once over the three shards" "$(printf 'kill A\nkill B\ns1\ns2\ns3\ns4\ns5\ns6\ns7\ns8\ns9\n')" "$(cat "$tmp"/shard? | sed -n 's/^ran //p' | sort)"
check "E2E_ONLY filters after the count" "run   s4 ran s4 shard 0/3: ran 1 of 11" "$(E2E_SHARD=0/3 E2E_ONLY='^s4$' node test/e2e/shard_test.mjs | tr '\n' ' ' | sed 's/ $//')"
fails_with "a shard past n is refused by the gate" env E2E_SHARD=3/3 node test/e2e/shard_test.mjs

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

# --- changes.sh: docs-only means Markdown in the root or under docs/, or tasks/prd.json.
g="$tmp/git"
git init -q "$g" && cd "$g" || exit 1
git config user.email t@t && git config user.name t
mkdir -p docs .claude/agents compat src tasks && echo 1 >tasks/prd.json && echo 1 >tasks/other.json && echo 1 >README.md && echo 1 >docs/a.md && echo 1 >docs/a.txt && echo 1 >src/a.go && echo 1 >.claude/agents/qa.md && echo 1 >compat/x.md
git add -A && git commit -qm base
changed() { git add -A && git commit -qm change && bash "$repo/test/changes.sh" HEAD~1 HEAD; }
echo 2 >>README.md; echo 2 >>docs/a.md
check "a root .md and docs/*.md are docs-only" docs-only "$(changed)"
echo 2 >>tasks/prd.json; echo 2 >>docs/a.md
check "tasks/prd.json with docs is docs-only" docs-only "$(changed)"
echo 2 >>tasks/other.json
check "another file under tasks/ is code" code "$(changed)"
echo 2 >>docs/a.txt
check "a file under docs/ that is not Markdown is code" code "$(changed)"
echo 2 >>README.md; echo 2 >>src/a.go
check "a Go file with a doc is code" code "$(changed)"
echo 2 >>.claude/agents/qa.md
check ".claude is code even for a .md" code "$(changed)"
echo 2 >>compat/x.md
check "compat is code even for a .md" code "$(changed)"
git mv src/a.go docs/b.md
check "code renamed into docs/ is code" code "$(changed)"
git mv README.md moved.txt
check "a renamed .md is code" code "$(changed)"
check "an empty diff is code" code "$(bash "$repo/test/changes.sh" HEAD HEAD)"
check "an unreadable diff is code" code "$(bash "$repo/test/changes.sh" nosuchbase...HEAD)"
cd "$repo" || exit 1

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
check "the integration shards still tile the tier with the stress files in" "$all" "$(printf '%s\n%s\n' "$dealt" "$rest" | sort)"
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

echo
[[ $fails -eq 0 ]] && echo "scripts: all passed" || echo "scripts: $fails failed"
exit $((fails > 0))
