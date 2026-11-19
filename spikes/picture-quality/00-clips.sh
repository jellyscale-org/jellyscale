#!/usr/bin/env bash
# Downloads the open films and cuts the clips the other scripts measure.
# Sources and licences are in README.md. About 14 GB of downloads.
source "$(dirname "$0")/lib.sh"
# film <dir> <zip URL>: the film in the zip, unpacked once and the zip removed.
film() {
  mkdir -p "$S/dl/$1"
  ls "$S/dl/$1" | grep -q -E '\.(mov|mkv|mp4)$' && return
  curl -sSL -o "$S/dl/$1/film.zip" "$2" && unzip -q -o -j "$S/dl/$1/film.zip" -d "$S/dl/$1" && rm "$S/dl/$1/film.zip"
}
film tos https://download.blender.org/demo/movies/ToS/tearsofsteel_4k.mov.zip
film sintel https://download.blender.org/durian/movies/Sintel.2010.4k.mkv.zip
film bbb https://download.blender.org/demo/movies/BBB/bbb_sunflower_2160p_30fps_normal.mp4.zip
mkdir -p "$S/dl/xiph"
[ -s "$S/dl/xiph/crowd_run_1080p50.y4m" ] || curl -sSL -o "$S/dl/xiph/crowd_run_1080p50.y4m" https://media.xiph.org/video/derf/y4m/crowd_run_1080p50.y4m
# Where each clip starts in its film, in seconds.
: "${TOS_DARK:=100}" "${TOS_ACTION:=462}" "${SINTEL_DARK:=570}" "${SINTEL_MOTION:=250}" "${BBB_FLAT:=200}"
tos=$(ls "$S"/dl/tos/*.mov) sintel=$(ls "$S"/dl/sintel/*.mkv) bbb=$(ls "$S"/dl/bbb/*.mp4)

# cut <name> <master> <start> <seconds> [filter]: a 4K clip as the master has
# it (stream copy from the keyframe before the start), and the 1080p source
# made from it: scaled once with lanczos and coded near lossless (x264 slow,
# CRF 10), which stands for a Blu-ray remux. Every picture is measured against
# the 1080p source, as the server would see it.
cut() {
  local name=$1 master=$2 start=$3 len=$4 filter=${5:-null}
  [ -s "$S/src4k/$name.mkv" ] || ffmpeg -nostdin -v error -y -ss "$start" -i "$master" -t "$len" -map 0:v:0 -c copy "$S/src4k/$name.mkv"
  [ -s "$S/src/$name.mkv" ] || ffmpeg -nostdin -v error -y -i "$S/src4k/$name.mkv" \
    -vf "scale=1920:-2:flags=lanczos,$filter,format=yuv420p,setpts=N/FRAME_RATE/TB" -fps_mode passthrough \
    -c:v libx264 -preset slow -crf 10 -an "$S/src/$name.mkv"
}
cut tos_dark      "$tos"    "$TOS_DARK"    20
cut tos_action    "$tos"    "$TOS_ACTION"  20
# Film grain: none of the open films has any, so this is the dark clip with
# temporal noise added (synthetic; strength 10 of 100, luma and chroma).
cut tos_grain     "$tos"    "$TOS_DARK"    20 "noise=alls=10:allf=t"
cut sintel_dark   "$sintel" "$SINTEL_DARK" 20
cut sintel_motion "$sintel" "$SINTEL_MOTION" 20
cut bbb_flat      "$bbb"    "$BBB_FLAT"    20
# Camera noise, a crowd and a pan at 50 fps: 10 s is all there is of it.
[ -s "$S/src/crowd_run.mkv" ] || ffmpeg -nostdin -v error -y -i "$S/dl/xiph/crowd_run_1080p50.y4m" \
  -vf format=yuv420p -c:v libx264 -preset slow -crf 10 "$S/src/crowd_run.mkv"
# A UHD film is usually 10-bit HEVC, which costs more to decode than the
# masters' H.264: one 4K clip in that form, for the cost table.
[ -s "$S/src4k/tos_action_hevc10.mkv" ] || ffmpeg -nostdin -v error -y -i "$S/src4k/tos_action.mkv" \
  -c:v libx265 -preset fast -crf 16 -pix_fmt yuv420p10le -x265-params log-level=error "$S/src4k/tos_action_hevc10.mkv"
for f in "$S"/src/*.mkv "$S"/src4k/*.mkv; do
  echo "$(basename "$f") $(ffprobe -v error -select_streams v:0 -show_entries stream=codec_name,width,height,r_frame_rate,pix_fmt -of csv=p=0 "$f") $(seconds_of "$f")s $(kbps "$f")kbps"
done | tee "$R/clips.txt"
