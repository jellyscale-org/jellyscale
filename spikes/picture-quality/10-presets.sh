#!/usr/bin/env bash
# x264 presets at the caps clients send, with the app's other arguments
# (CRF 23, bufsize twice the cap). Cap 0 is jellyfin-web's Auto, which the app
# holds to 6 bits per pixel a second: 12.44 Mbps at 1920x1080 and 9.88 at
# 1920x858, so a 12 or a 20 Mbps cap is the same command as Auto (or within
# 4% of it) and is not run apart. Then the same
# at CRF 20 and 18, which is what a cap is for when CRF 23 leaves it unused.
# JOBS encodes run side by side (on CPUS, if set), so the CPU column here is
# under contention: the cost table comes from 40-cost.sh.
source "$(dirname "$0")/lib.sh"
for clip in $CLIPS; do
  for preset in superfast veryfast faster fast medium slow; do
    for cap in 0 4 8; do echo "presets $clip x264 $preset 23 $cap"; done
  done
done > "$S/logs/presets.jobs"
for clip in ${CRF_CLIPS:-tos_dark tos_action tos_grain sintel_motion bbb_flat}; do
  for preset in veryfast fast medium; do
    for crf in 20 18; do for cap in 0 4; do echo "presets $clip x264 $preset $crf $cap"; done; done
  done
done >> "$S/logs/presets.jobs"
xargs -P "${JOBS:-3}" -L 1 ${CPUS:+taskset -c $CPUS} "$here/one.sh" < "$S/logs/presets.jobs"
# What a cap above the app's limit would buy: 20 Mbps passed as it is.
for clip in tos_action tos_grain crowd_run; do
  for crf in 23 20 18; do echo "presets $clip x264 fast $crf 20 raw20"; done
done | RATE=raw xargs -P "${JOBS:-3}" -L 1 ${CPUS:+taskset -c $CPUS} "$here/one.sh"
"$here/gather.sh" presets
