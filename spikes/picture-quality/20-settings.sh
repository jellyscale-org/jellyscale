#!/usr/bin/env bash
# What x264's other settings do beyond the preset, each changed alone.
source "$(dirname "$0")/lib.sh"
base=$X264_PARAMS
variants=(
  "aq2|$base:aq-mode=2"
  "aq3|$base:aq-mode=3"
  "psy0|$base:psy-rd=0,0"
  "la40|$base:rc-lookahead=40"
  "la60|$base:rc-lookahead=60"
  "ref3|$base:ref=3"
  "ref5|$base:ref=5"
  "bframes3|bframes=3:b-adapt=0:open-gop=0:scenecut=0"
  "badapt2|bframes=3:b-adapt=2:open-gop=0:scenecut=0"
  "trellis2|$base:trellis=2"
  "subme7|$base:subme=7"
)
# CRF 18 under a 4 Mbps cap, where the cap binds and every variant spends
# about the same bits, so the scores alone compare them.
for clip in ${SETTING_CLIPS:-tos_dark tos_grain sintel_motion}; do
  for cond in "fast 18 4"; do
    echo "settings $clip x264 $cond"
    for v in "${variants[@]}"; do echo "settings $clip x264 $cond ${v%%|*} ${v#*|}"; done
    echo "settings $clip x264 $cond tune-film $base -tune film"
    echo "settings $clip x264 $cond tune-grain $base -tune grain"
  done
done > "$S/logs/settings.jobs"
# Which of faster's settings carries the step from veryfast to faster, and
# whether fast's can be had on faster for less: each alone, CRF 23 at Auto.
steps=(
  "veryfast|subme4|$base:subme=4"
  "veryfast|la20|$base:rc-lookahead=20"
  "veryfast|ref2|$base:ref=2"
  "veryfast|mixed-refs|$base:ref=2:mixed-refs=1"
  "veryfast|subme4-la20|$base:subme=4:rc-lookahead=20"
  "veryfast|trellis1|$base:trellis=1"
  "faster|subme6|$base:subme=6"
  "faster|la30|$base:rc-lookahead=30"
  "faster|ref1|$base:ref=1"
  "faster|ref3|$base:ref=3"
)
for clip in ${STEP_CLIPS:-tos_dark tos_action tos_grain bbb_flat}; do
  for cap in 0; do
    echo "settings $clip x264 veryfast 23 $cap"; echo "settings $clip x264 faster 23 $cap"
    for v in "${steps[@]}"; do IFS='|' read -r preset name params <<< "$v"; echo "settings $clip x264 $preset 23 $cap $name $params"; done
  done
done >> "$S/logs/settings.jobs"
xargs -P "${JOBS:-3}" -L 1 ${CPUS:+taskset -c $CPUS} "$here/one.sh" < "$S/logs/settings.jobs"
"$here/gather.sh" settings
