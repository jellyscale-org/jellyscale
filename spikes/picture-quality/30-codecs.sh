#!/usr/bin/env bash
# Rate against quality, with no cap at all (RATE=none), for x264, x265 and SVT-AV1 at several CRFs,
# to compare what a codec gains with what a preset gains. Keyframes every 4 s
# in all three; x265 and SVT-AV1 with the arguments of the app's fallbacks.
source "$(dirname "$0")/lib.sh"
for clip in ${CODEC_CLIPS:-tos_action tos_grain bbb_flat}; do
  for preset in veryfast fast; do
    for crf in 17 20 23 26 29; do echo "codecs $clip x264 $preset $crf 0"; done
  done
  for preset in veryfast fast; do
    for crf in 20 24 28 32; do echo "codecs $clip x265 $preset $crf 0"; done
  done
  for preset in 10 8; do
    for crf in 26 34 42 50; do echo "codecs $clip svtav1 $preset $crf 0"; done
  done
done | RATE=none xargs -P "${JOBS:-3}" -L 1 ${CPUS:+taskset -c $CPUS} "$here/one.sh"
"$here/gather.sh" codecs
