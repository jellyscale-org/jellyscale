#!/usr/bin/env bash
# Time from starting ffmpeg to each of the first four segments (a chunk) being
# complete, with the app's HLS arguments, for a start (segment 0) and a seek
# (segment 8, 32 s into the clip; times moved with -output_ts_offset, not the
# app's -copyts seam plan, which costs the same), pinned to a set of CPUs that stands for a
# worker of that size. The least of REPS runs, since other work on the machine
# only ever adds time.
#   50-latency.sh [crf]  -> results/latency-crf<crf>.csv
# Needs $S/src/long1080.mkv and $S/src4k/long4k.mkv (60 s; made below).
source "$(dirname "$0")/lib.sh"
crf=${1:-23}
out=$R/latency-crf$crf.csv
tos=$(ls "$S"/dl/tos/*.mov)
[ -s "$S/src4k/long4k.mkv" ] || ffmpeg -nostdin -v error -y -ss 440 -i "$tos" -t 60 -map 0:v:0 -c copy "$S/src4k/long4k.mkv"
[ -s "$S/src/long1080.mkv" ] || ffmpeg -nostdin -v error -y -i "$S/src4k/long4k.mkv" \
  -vf "scale=1920:-2:flags=lanczos,format=yuv420p,setpts=N/FRAME_RATE/TB" -fps_mode passthrough \
  -c:v libx264 -preset fast -crf 12 -g 48 "$S/src/long1080.mkv"

# first_segments <cpus> <source> <preset> <start segment> [extra args] -> "t0 t1 t2 t3" seconds
first_segments() {
  local cpus=$1 src=$2 preset=$3 start=$4; shift 4
  local dir; dir=$(mktemp -d -p "$S/enc" lat.XXXXXX)
  local cap; cap=$(maxrate_of "$src" 0)
  local t0; t0=$(date +%s%N)
  taskset -c "$cpus" ffmpeg -nostdin -v error -y -ss $((start * 4)) -i "$src" -output_ts_offset $((start * 4)) -map 0:v:0 \
    -vf "yadif=mode=send_frame:parity=auto:deint=interlaced,scale=$(size_of "$src"):flags=bicubic,format=yuv420p" \
    -c:v libx264 -preset "$preset" -profile:v high -level 51 -crf "$crf" -maxrate "$cap" -bufsize $((2 * cap)) \
    -x264-params "$X264_PARAMS" -forced-idr 1 -force_key_frames 'expr:gte(t,n_forced*4)' "$@" -an \
    -f hls -hls_time 4 -hls_playlist_type vod -hls_list_size 0 -hls_segment_type fmp4 -hls_flags temp_file \
    -start_number "$start" -hls_fmp4_init_filename init.mp4 \
    -hls_segment_options use_editlist=0:avoid_negative_ts=disabled:movflags=+frag_discont \
    -hls_segment_filename "$dir/%d.m4s" "$dir/index.m3u8" 2>/dev/null &
  local pid=$! n times=""
  for n in 0 1 2 3; do
    until [ -e "$dir/$((start + n)).m4s" ] || ! kill -0 $pid 2>/dev/null; do sleep 0.01; done
    times+=" $(awk -v a="$t0" -v b="$(date +%s%N)" 'BEGIN{printf "%.2f", (b-a)/1e9}')"
  done
  kill -9 $pid 2>/dev/null || true; wait $pid 2>/dev/null || true
  rm -rf "$dir"
  echo $times
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  echo cpus,source,preset,crf,start_segment,seg1_s,seg2_s,seg3_s,seg4_s > "$out"
  # 1, 2 and 3 physical cores of this machine with their hyperthreads (lscpu -e).
  for cpus in ${CPUSETS:-7,15 6-7,14-15 5-7,13-15}; do
    for src in src/long1080 src4k/long4k; do
      for preset in ${PRESETS:-superfast veryfast faster fast medium}; do
        for start in 0 8; do
          best=""
          for _ in $(seq "${REPS:-2}"); do
            got=$(first_segments "$cpus" "$S/$src.mkv" "$preset" "$start")
            if [ -z "$best" ] || awk -v a="${got%% *}" -v b="${best%% *}" 'BEGIN{exit !(a<b)}'; then best=$got; fi
          done
          echo "\"$cpus\",$src,$preset,$crf,$start,${best// /,}" | tee -a "$out"
        done
      done
    done
  done
fi
