#!/usr/bin/env bash
# test/js171-before-js152.sh <workdir> runs the two seek checks JS-171 shortened (the lost-head check and the store check) on
# the product code of b920b19, the build before JS-152, with this checkout's test files. The lost-head check must fail there
# at each of its seeks for the reason it names: the seek got no segment, and the failed job's recorded reason says ffmpeg made
# no packet. It exits 0 when it does, and 1 when it passes there (it would not have caught JS-152) or fails for another reason.
# The store check is reported: it does not fail on that build in its full length either (see below).
#
# Run it with the host's ffmpeg (9.0.2 where this was written). Under CI's ffmpeg 6.1 (JS171_RUNNER=$PWD/test/ffmpeg61.sh) the lost-head
# check passes on b920b19, as JS-152's own TestASeekIntoAFilmWhoseAudioHeadIsLostGetsItsSegmentWithSound does there (observed
# 2026-11-22): the damaged head costs the probe its packet on ffmpeg 9.0.2 and not on 6.1, so what fails before JS-152 fails on
# the newer ffmpeg only.
#
#   JELLYSCALE_TEST_DATABASE_URL=... test/js171-before-js152.sh ~/.cache/jellyscale/agents/js171-before
set -euo pipefail
cd "$(dirname "$0")/.."
rev=b920b19
work=$(realpath -m "${1:?a workdir}")
rm -rf "$work" && mkdir -p "$work"
trap 'rm -rf "$work"' EXIT
git archive "$rev" | tar -x -C "$work"
# The product code is the old build's; the checks are this checkout's.
cp internal/transcode/*_test.go "$work/internal/transcode/"
cp internal/testenv/*.go "$work/internal/testenv/"
mkdir -p "$work/test" && cp -r test/fixtures "$work/test/"
out=$work/out.txt
status=0
(cd "$work" && ${JS171_RUNNER:-} go test -tags integration -count=1 -v ./internal/transcode \
  -run '^(TestSeveralSeeksInOnePlayOfAFilmWhoseAudioHeadIsLostEachGetTheirSegment|TestASeekPastHalfIntoAFilmReadFromAStoreWithNoPathGetsItsSegment)$' >"$out" 2>&1) || status=$?
fail() { echo "FAIL: $1" >&2; tail -40 "$out" >&2; exit 1; }
[ "$status" -ne 0 ] || fail "the lost-head check passed on the build before JS-152"
grep -q 'build failed' "$out" && fail "the checks do not build against the product code of $rev"
# The lost-head check fails at each of its seeks, which are its subtests: as many as it declares, and at least three (a short
# one, one past half and the last segment).
declared=$(grep -c -- '=== RUN   TestSeveralSeeksInOnePlayOfAFilmWhoseAudioHeadIsLostEachGetTheirSegment/segment_' "$out" || true)
heads=$(grep -c -- '--- FAIL: TestSeveralSeeksInOnePlayOfAFilmWhoseAudioHeadIsLostEachGetTheirSegment/segment_' "$out" || true)
[ "$declared" -ge 3 ] || fail "the lost-head check ran $declared seeks, want at least 3"
[ "$heads" -eq "$declared" ] || fail "the lost-head check failed at $heads of its $declared seeks"
grep -q 'got no segment' "$out" || fail "no seek got no segment"
grep -q 'made no packet' "$out" || fail "no failed job's recorded reason says ffmpeg made no packet"
# The store check is reported, not demanded: it passes on this build even at its full length of 229 segments (observed on
# b920b19 with the checks of HEAD, 119 s), so there is no failure for a shortened one to keep. What it must not do is fail on its
# ceiling of segments or on a timeout.
! grep -q 'the run from the film.s start makes' "$out" || fail "a check failed on its ceiling of segments, not on the seek"
! grep -q 'timed out' "$out" || fail "a check timed out instead of failing on its seek"
grep -q -- '--- PASS: TestASeekPastHalfIntoAFilmReadFromAStoreWithNoPathGetsItsSegment' "$out" \
  && echo "the store check passes on $rev, as its full-length form does: it is not one that JS-152's bug fails"
echo "the lost-head check fails on $rev for the reason it names, at each of its $declared seeks"
