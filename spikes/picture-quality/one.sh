#!/usr/bin/env bash
# one.sh <set> <clip> <codec> <preset> <crf> <cap Mbps|0> [variant [x264-params [ffmpeg args...]]]
# One encode of a 1080p clip, measured (the cap as the app would apply it,
# see lib.sh; RATE=none or raw changes that): a line of CSV in $S/res/<set>/, which
# the matrix scripts gather. Skips a line it already has. The encode is
# deleted once it is measured.
#   clip,codec,preset,crf,cap_mbps,variant,kbps,vmaf,vmaf_p5,vmaf_min,psnr_y,ssim,cpu_s_per_s,wall_s
source "$(dirname "$0")/lib.sh"
set_=$1 clip=$2 codec=$3 preset=$4 crf=$5 cap=$6 variant=${7:-app} params=${8:-}
shift $(( $# < 8 ? $# : 8 ))
key=$clip-$codec-$preset-$crf-$cap-$variant
mkdir -p "$S/res/$set_" "$S/enc/$set_"
line=$S/res/$set_/$key.csv out=$S/enc/$set_/$key.mp4 src=$S/src/$clip.mkv
[ -s "$line" ] && exit 0
bps=$((cap * 1000000))
case $codec in
x264)
  PARAMS=${params:-$X264_PARAMS} x264_encode "$src" "$out" "$preset" "$crf" "$bps" "$@" ;;
x265) # the HEVC fallback's arguments (recodeArgs), preset and CRF apart
  vbv=""
  if [ "${RATE:-app}" != none ]; then
    bps=$(maxrate_of "$src" "$bps"); vbv=":vbv-maxrate=$((bps / 1000)):vbv-bufsize=$((2 * bps / 1000))"
  fi
  timed "$out.time" ffmpeg -nostdin -v error -y -i "$src" -map 0:v:0 -vf format=yuv420p \
    -c:v libx265 -preset "$preset" -tag:v hvc1 -crf "$crf" \
    -x265-params "bframes=2:b-adapt=0:open-gop=0:scenecut=0:keyint=100000$vbv:log-level=error" \
    -forced-idr 1 -force_key_frames 'expr:gte(t,n_forced*4)' "$@" -an "$out" ;;
svtav1) # the AV1 fallback's, but one run with a keyframe every 4 s of film
  fps=$(ffprobe -v error -select_streams v:0 -show_entries stream=r_frame_rate -of csv=p=0 "$src")
  mbr=""
  if [ "${RATE:-app}" != none ]; then bps=$(maxrate_of "$src" "$bps"); mbr=":mbr=$((bps / 1000))"; fi
  timed "$out.time" ffmpeg -nostdin -v error -y -i "$src" -map 0:v:0 -vf format=yuv420p \
    -c:v libsvtav1 -preset "$preset" -crf "$crf" -g $((4 * fps)) -svtav1-params "scd=0$mbr" "$@" -an "$out" ;;
esac
d=$(seconds_of "$src")
echo "$clip,$codec,$preset,$crf,$cap,$variant,$(kbps "$out"),$(vmaf "$out" "$src" | tr ' ' ','),$(cpu_per_s "$out.time" "$d"),$(awk '{print $3}' "$out.time")" > "$line.tmp"
mv "$line.tmp" "$line"
rm -f "$out" "$out.time" "$out.time.err"
