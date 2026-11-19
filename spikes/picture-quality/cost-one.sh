#!/usr/bin/env bash
# cost-one.sh <source> <preset> <crf> <cap bps|0> [x264-params]: the app's
# video encode of the source to at most 1080 lines (cap 0 is Auto), not kept. Prints
#   cpu_s_per_s wall_s speed kbps
# CPU is user plus system of the ffmpeg process, per second of film. Runs
# under any ffmpeg (40-cost.sh runs it in the ffmpeg 6.1 container too).
source "$(dirname "$0")/lib.sh"
src=$1 preset=$2 crf=$3 cap=$4
[ -n "${5:-}" ] && PARAMS=$5
out=$(mktemp -p "$S/enc" cost.XXXXXX.mp4)
x264_encode "$src" "$out" "$preset" "$crf" "$cap"
d=$(seconds_of "$src")
awk -v d="$d" -v k="$(kbps "$out")" '{printf "%.2f %.1f %.2f %s\n", ($1+$2)/d, $3, d/$3, k}' "$out.time"
rm -f "$out" "$out.time" "$out.time.err"
