#!/usr/bin/env bash
# Shows that the tests of the burn-in kit, the slow-database tests, the libraries test and the stall test still fail on
# the defect each exists to catch, now that they run at the films' own size, on a shorter film, side by side, with a
# forced order, and so on. Every line must say "ok".
#
#   test/mutants.sh                          # every mutant
#   test/mutants.sh cue-whole-film-read ...  # these (by name)
#   test/mutants.sh --list                   # each mutant and what it must do
#
# Each mutant is one small change to the product (test/mutant.py, applied through go's -overlay, so the tree is never
# changed), run with the tests named for it. An expectation is a regex of a subtest's name (the part after
# "--- FAIL: " or "--- PASS: ") and, after "=", how many lines match: exactly that many, or at least one without "=".
# "-" means no test of the run may fail. A run's output must hold the text of the test's own assertion where one is
# named, and not hold the text of a harness guard where one is named. A mutant that no longer applies exits 2.
# `publish-404-is-unaskable` changes test/publish.sh, which is no Go, in a copy of the tree. Needs what the tests need:
# JELLYSCALE_TEST_DATABASE_URL (a Postgres on a Docker-assigned port, not one that another run shares: some tests end
# backends chosen from pg_locks, which is cluster-wide), ffmpeg, and Docker for the registry test. MUTANTS_RUNNER is a
# prefix command, for example test/ffmpeg61.sh.
set -uo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
runner=(${MUTANTS_RUNNER:-})

T=./internal/transcode
LIB=TestAProcessWithoutTheWorkerRoleWritesNothingToTheLibraries
STALL=TestAWorkerStoppedPastItsLeaseAndTheStallLimitEndsItsJobCancelled

# name@package@-run regex@must fail (space separated)@must pass (space separated)@text the output must hold (ERE)@text it must not hold (ERE)
mutants=(
  "cue-begins-after-the-cue@$T@^(TestAMidFilmRunDrawsTheCueAFilmFromZeroShowsAtItsCut|TestAMidFilmRunDrawsAPGSPictureBuiltByTwoPackets|TestAMidFilmRunOfAnMPEG2FilmWithVobSubDrawsTheCueShowing|TestAfterTheFilmIsReplacedAMidFilmRunDrawsByTheNewFilesCues|TestASeekIntoABurntImageCueShowsItFromTheFirstFrame|TestAFailoverInsideABurntImageCueShowsItFromTheFirstFrame|TestABurntImageSubtitleGetsHelpers|TestAFilmScannedByThePreviousReleaseMeetsTheCriteriaByTheNextScan)\$@TestAMidFilmRunDrawsTheCueAFilmFromZeroShowsAtItsCut TestAMidFilmRunDrawsAPGSPictureBuiltByTwoPackets TestAMidFilmRunOfAnMPEG2FilmWithVobSubDrawsTheCueShowing TestAfterTheFilmIsReplacedAMidFilmRunDrawsByTheNewFilesCues TestASeekIntoABurntImageCueShowsItFromTheFirstFrame TestAFailoverInsideABurntImageCueShowsItFromTheFirstFrame TestABurntImageSubtitleGetsHelpers TestAFilmScannedByThePreviousReleaseMeetsTheCriteriaByTheNextScan/draws@@"
  "cue-reads-only-the-later-packet@$T@^TestAMidFilmRunDrawsAPGSPictureBuiltByTwoPackets\$@TestAMidFilmRunDrawsAPGSPictureBuiltByTwoPackets@@"
  "cue-opens-at-the-films-start@$T@^(TestARunInsideACueReadsOnlyTheFilmFromTheCuesFirstPacket|TestAfterTheFilmIsReplacedAMidFilmRunReadsByTheNewFilesCues|TestAFilmScannedByThePreviousReleaseMeetsTheCriteriaByTheNextScan)\$@TestARunInsideACueReadsOnlyTheFilmFromTheCuesFirstPacket TestAfterTheFilmIsReplacedAMidFilmRunReadsByTheNewFilesCues TestAFilmScannedByThePreviousReleaseMeetsTheCriteriaByTheNextScan/reads@@"
  "cue-nothing-showing-opens-early@$T@^TestARunWhereNoCueIsShowingReadsNoMoreThanOneWithoutTheTrack\$@TestARunWhereNoCueIsShowingReadsNoMoreThanOneWithoutTheTrack@@"
  "cue-lookback-too-short@$T@^TestAMidFilmRunOfAFilmWithNoCueTimesDrawsACueWithinTheLookBackAndMissesAnOlderOne\$@TestAMidFilmRunOfAFilmWithNoCueTimesDrawsACueWithinTheLookBackAndMissesAnOlderOne@@"
  "cue-lookback-too-long@$T@^TestAMidFilmRunOfAFilmWithNoCueTimesDrawsACueWithinTheLookBackAndMissesAnOlderOne\$@TestAMidFilmRunOfAFilmWithNoCueTimesDrawsACueWithinTheLookBackAndMissesAnOlderOne@@"
  "cue-whole-film-read@$T@^(TestAMidFilmRunOfAFilmWithNoCueTimesReadsAtMostTheLookBackMore|TestTheFirstMidFilmRunReadsTheSameInAFilmFourTimesAsLong|TestARunFromZeroWithABurntImageTrackReadsNoMoreThanOneWithout|TestARunWhoseImageTrackIsAFileBesideTheFilmReadsNoMoreThanOneWithout|TestTwoMidFilmRunsAtOnceOnTwoWorkersEachStayWithinOneRunsBound)\$@TestAMidFilmRunOfAFilmWithNoCueTimesReadsAtMostTheLookBackMore TestTheFirstMidFilmRunReadsTheSameInAFilmFourTimesAsLong TestARunFromZeroWithABurntImageTrackReadsNoMoreThanOneWithout TestARunWhoseImageTrackIsAFileBesideTheFilmReadsNoMoreThanOneWithout TestTwoMidFilmRunsAtOnceOnTwoWorkersEachStayWithinOneRunsBound@@"
  "cue-failed-read-is-nothing-showing@$T@^TestAFailedReadOfTheCueTimesNeverLeavesASegmentWithoutTheCue\$@TestAFailedReadOfTheCueTimesNeverLeavesASegmentWithoutTheCue@@"
  "cue-read-outlives-its-job@$T@^TestEveryReadOfAMidFilmRunEndsWithItsJob\$@TestEveryReadOfAMidFilmRunEndsWithItsJob@@"
  "cue-runs-one-at-a-time@$T@^TestTwoMidFilmRunsAtOnceOnTwoWorkersEachStayWithinOneRunsBound\$@TestTwoMidFilmRunsAtOnceOnTwoWorkersEachStayWithinOneRunsBound@@"
  "scan-reads-the-whole-film@$T@^TestTheNextScanOfAFilmTheLastReleaseScannedReadsNothingThatGrowsWithTheFilm\$@TestTheNextScanOfAFilmTheLastReleaseScannedReadsNothingThatGrowsWithTheFilm@@"
  "keeper-connect-bound@$T@^TestASlowDatabaseSetBeforeTheClaimCostsNoJobItsLeaseOrItsWorker\$@TestASlowDatabaseSetBeforeTheClaimCostsNoJobItsLeaseOrItsWorker@@jobs kept ending without the segment|timed out waiting for the segment|ended (cancelled|failed) through a slow database|lease was found run out|jobs for the spec, want 1@harness error"
  "keeper-renewal-two-round-trips@$T@^TestASlowDatabaseCostsNoJobItsLeaseOrItsWorker\$@TestASlowDatabaseCostsNoJobItsLeaseOrItsWorker@@ended (cancelled|failed) through a slow database, want done|lease was found run out|jobs for the spec, want 1@harness error"
  "libraries-synced-by-every-role@./cmd/jellyscale@^$LIB\$@$LIB/api/\S+=6 $LIB/stream/\S+=6 $LIB/api,stream/\S+=6@@"
  "libraries-synced-by-the-api-role@./cmd/jellyscale@^$LIB\$@$LIB/api/\S+=6 $LIB/api,stream/\S+=6@$LIB/stream/\S+=6@"
  "stall-keeper-cut-ends-failed@$T@^$STALL\$@$STALL/\S*heartbeat_first\S*=2@$STALL/\S*poll_first\S*=2@@"
  "stall-upload-ends-failed@$T@^$STALL\$@$STALL/\S*poll_first\S*=2@$STALL/\S*heartbeat_first\S*=2@@"
  "stall-poll-yields-to-keeper@$T@^$STALL\$@-@$STALL/\S*poll_first\S*=2 $STALL/\S*heartbeat_first\S*=2@@harness error"
  "stall-keeper-cut-late@$T@^$STALL\$@-@$STALL/\S*poll_first\S*=2 $STALL/\S*heartbeat_first\S*=2@@harness error"
)

if [[ ${1:-} == --list ]]; then
  for entry in "${mutants[@]}"; do
    IFS='@' read -r name pkg run fails passes text nottext <<<"$entry"
    echo "$name ($pkg): must fail: ${fails// /, }; must pass: ${passes// /, }"
  done
  echo "publish-404-is-unaskable (./deploy/image): must fail TestARegistryThatAnswersNotFoundHasTheTagAbsentAndIsWrittenTo once for each of MANIFEST_UNKNOWN and NAME_UNKNOWN"
  exit 0
fi
if [[ -z ${JELLYSCALE_TEST_DATABASE_URL:-} ]]; then
  echo "test/mutants.sh needs JELLYSCALE_TEST_DATABASE_URL (a disposable Postgres on a Docker-assigned port)" >&2
  exit 1
fi
want=("$@")
status=0

wanted() {
  [[ ${#want[@]} -eq 0 ]] && return 0
  local each
  for each in "${want[@]}"; do [[ $each == "$1" ]] && return 0; done
  return 1
}

# expect <log> <FAIL|PASS> <pattern[=N]>...: each pattern is matched against the subtest names of that outcome.
expect() {
  local log=$1 outcome=$2 item pattern n got
  shift 2
  for item in "$@"; do
    pattern=${item%%=*} n=""
    [[ $item == *=* ]] && n=${item##*=}
    got=$(grep -cE -- "--- $outcome: ($pattern)( |\$)" "$log")
    if [[ -n $n && $got -ne $n ]] || [[ -z $n && $got -lt 1 ]]; then
      echo "   NOT AS EXPECTED: $got subtests matching $pattern $(echo "$outcome" | tr A-Z a-z)ed, want ${n:-at least one}   (log $log)"
      status=1
      return 1
    fi
    echo "   ok: $got $pattern $(echo "$outcome" | tr A-Z a-z)ed"
  done
}

for entry in "${mutants[@]}"; do
  IFS='@' read -r name pkg run fails passes text nottext <<<"$entry"
  wanted "$name" || continue
  log=$(mktemp "${TMPDIR:-/tmp}/mutant.XXXXXX")
  echo "== $name"
  "$repo/test/mutant.sh" "$name" "${runner[@]}" go test -tags integration -p 1 -count=1 -v -timeout 40m -run "$run" "$pkg" >"$log" 2>&1
  if grep -q 'the mutant no longer applies' "$log"; then
    cat "$log"
    exit 2
  fi
  good=1
  if [[ $fails == "-" ]]; then
    if grep -q -- '--- FAIL: ' "$log"; then
      echo "   NOT AS EXPECTED: a test failed, none may   (log $log)"
      status=1
      good=0
    else
      echo "   ok: no test failed"
    fi
  else
    # shellcheck disable=SC2086
    expect "$log" FAIL $fails || good=0
  fi
  # shellcheck disable=SC2086
  [[ -z $passes ]] || expect "$log" PASS $passes || good=0
  if [[ -n $text ]] && ! grep -qE -- "$text" "$log"; then
    echo "   NOT AS EXPECTED: the output does not hold the test's own assertion ($text)   (log $log)"
    status=1
    good=0
  fi
  if [[ -n $nottext ]] && grep -qE -- "$nottext" "$log"; then
    echo "   NOT AS EXPECTED: the output holds '$nottext', a guard of the harness and not the test's assertion   (log $log)"
    status=1
    good=0
  fi
  [[ $good -eq 1 ]] && rm -f "$log"
done

# The one mutant of a script: a publish.sh that reads a 404 as a registry it cannot ask, so pushes nothing. The test runs
# test/publish.sh from the tree it is in, so it runs in a copy.
if wanted publish-404-is-unaskable; then
  echo "== publish-404-is-unaskable"
  copy=$(mktemp -d "${TMPDIR:-/tmp}/publish.XXXXXX")
  (cd "$repo" && tar --exclude=.git --exclude=web-dist --exclude=.results -c .) | tar -x -C "$copy"
  if ! grep -q '^  404) return 1 ;;$' "$copy/test/publish.sh"; then
    echo "the mutant no longer applies: test/publish.sh has no '404) return 1 ;;'" >&2
    rm -rf "$copy"
    exit 2
  fi
  sed -i 's|^  404) return 1 ;;$|  404) echo "publish.sh: 404 cannot be asked: nothing was pushed" >\&2; exit 1 ;;|' "$copy/test/publish.sh"
  log=$(mktemp "${TMPDIR:-/tmp}/mutant.XXXXXX")
  (cd "$copy" && "${runner[@]}" go test -tags integration -p 1 -count=1 -v -timeout 10m -run '^TestARegistryThatAnswersNotFoundHasTheTagAbsentAndIsWrittenTo$' ./deploy/image) >"$log" 2>&1
  good=1
  for code in MANIFEST_UNKNOWN NAME_UNKNOWN; do
    if grep -q "a registry answering 404 $code counted as unaskable" "$log"; then
      echo "   ok: failed for $code"
    else
      echo "   NOT AS EXPECTED: no failure naming $code   (log $log)"
      status=1
      good=0
    fi
  done
  [[ $good -eq 1 ]] && rm -f "$log"
  rm -rf "$copy"
fi
exit $status
