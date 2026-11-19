#!/usr/bin/env bash
# Can segments made at different presets share one stream? A player gets one
# init segment (the parameter sets) for a spec, so segments of another preset
# must decode under it.
#  1. The parameter sets (avcC) each preset writes, with the app's arguments.
#  2. A stream of preset A's init and first segment followed by preset B's
#     later segments, decoded by ffmpeg: errors, frames, and VMAF against the
#     source over the spliced part.
source "$(dirname "$0")/lib.sh"
src=$S/src/long1080.mkv
work=$S/enc/mixed; rm -rf "$work"; mkdir -p "$work"
run() { # <preset> [x264 params]: 16 s as HLS fMP4 in $work/<preset>
  local preset=$1 params=${2:-$X264_PARAMS} cap; cap=$(maxrate_of "$src" 0)
  mkdir -p "$work/$preset"
  taskset -c "${CPUS:-0-15}" ffmpeg -nostdin -v error -y -i "$src" -t 16 -map 0:v:0 \
    -vf "scale=$(size_of "$src"):flags=bicubic,format=yuv420p" \
    -c:v libx264 -preset "$preset" -profile:v high -level 51 -crf 23 -maxrate "$cap" -bufsize $((2 * cap)) \
    -x264-params "$params" -forced-idr 1 -force_key_frames 'expr:gte(t,n_forced*4)' -an \
    -f hls -hls_time 4 -hls_playlist_type vod -hls_list_size 0 -hls_segment_type fmp4 \
    -hls_fmp4_init_filename init.mp4 -hls_segment_filename "$work/$preset/%d.m4s" "$work/$preset/index.m3u8"
}
avcc() { # the SPS and PPS in an init segment, as hex
  ffprobe -v error -select_streams v:0 -show_entries stream=extradata -show_data -of default=nw=1 "$1" 2>/dev/null |
    grep -E '^[0-9a-f]{8}:' | cut -c11-50 | tr -d ' \n'
}
{
echo "## parameter sets by preset (app arguments)"
for p in superfast veryfast faster fast medium slow; do run $p; echo "$p $(avcc "$work/$p/init.mp4")"; done
echo "## with ref=3 fixed on every preset"
for p in superfast veryfast faster fast medium slow; do
  mv "$work/$p" "$work/$p.app"; run $p "$X264_PARAMS:ref=3"; echo "$p $(avcc "$work/$p/init.mp4")"; mv "$work/$p" "$work/$p.ref3"; mv "$work/$p.app" "$work/$p"
done
echo "## with the parameter sets repeated in every keyframe (repeat-headers=1)"
for p in veryfast medium; do
  mv "$work/$p" "$work/$p.app"; run $p "$X264_PARAMS:repeat-headers=1"; mv "$work/$p" "$work/$p.inband"; mv "$work/$p.app" "$work/$p"
done
echo "## splices: init and segment 0 of A, segments 1 to 3 of B"
ffmpeg -nostdin -v error -y -ss 4 -i "$src" -t 12 -c:v libx264 -qp 0 -preset ultrafast "$work/ref.mkv"
for pair in veryfast:veryfast veryfast:faster veryfast:fast veryfast:medium veryfast:slow superfast:veryfast medium:veryfast slow:veryfast \
    veryfast.ref3:faster.ref3 fast.ref3:medium.ref3 fast.ref3:slow.ref3 veryfast.ref3:fast.ref3 veryfast.ref3:slow.ref3 \
    veryfast.inband:medium.inband medium.inband:veryfast.inband; do
  a=${pair%:*} b=${pair#*:}
  cat "$work/$a/init.mp4" "$work/$a/0.m4s" "$work/$b"/{1,2,3}.m4s > "$work/splice.mp4"
  errors=$(ffmpeg -nostdin -v error -i "$work/splice.mp4" -f null - 2>&1 | wc -l)
  frames=$(ffprobe -v error -count_frames -select_streams v:0 -show_entries stream=nb_read_frames -of csv=p=0 "$work/splice.mp4" 2>/dev/null || true)
  ffmpeg -nostdin -v error -y -ss 4 -i "$work/splice.mp4" -c:v libx264 -qp 0 -preset ultrafast "$work/tail.mkv" 2>/dev/null || true
  echo "$a then $b: decode error lines $errors, frames $frames, VMAF of B's part (mean, 5th percentile, least) $(VMAF_SUBSAMPLE=1 vmaf "$work/tail.mkv" "$work/ref.mkv" 2>/dev/null | cut -d' ' -f1-3 || true)"
done
} | tee "$R/mixed.txt"
[ -n "${KEEP:-}" ] || rm -rf "$work"   # KEEP=1 leaves the segments for 61-mixed-chrome.mjs
