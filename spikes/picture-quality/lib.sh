# Shared by the scripts here. Source it; it defines where things live and the
# two measurements every script makes: an encode with its CPU time, and VMAF.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
S=${SPIKE_SCRATCH:-$HOME/.cache/jellyscale/spike-picture}   # downloads, clips, encodes
R=$here/results                                             # the CSVs the report quotes
mkdir -p "$S"/{dl,src,src4k,enc,logs} "$R"

# The 1080p clips every picture measurement uses (made by 00-clips.sh).
CLIPS=${CLIPS:-"tos_dark tos_action tos_grain sintel_dark sintel_motion bbb_flat crowd_run"}

# What jellyscale passes x264 today (internal/transcode/ffmpeg.go), preset and
# rate apart.
X264_PARAMS="bframes=2:b-adapt=0:open-gop=0:scenecut=0"

# The output size the app chooses for a cap of 2 Mbps or more
# (playback.outputSize): at most 1080 lines, the width following the shape.
size_of() {
  ffprobe -v error -select_streams v:0 -show_entries stream=width,height -of csv=p=0 "$1" |
    awk -F, '{h=$2-$2%2; if(h>1080)h=1080; w=int($1*h/$2); w-=w%2; printf "%d:%d", w, h}'
}

# The app's -maxrate for a client's cap in bits per second (0 is jellyfin-web's
# Auto, which sends 2147483647): never over 6 bits per pixel a second
# (playback.NewSpec), so Auto at 1920x1080 is 12.44 Mbps.
maxrate_of() { # <source> <cap>
  size_of "$1" | awk -F: -v cap="$2" '{b=$1*$2*6; print (cap>0 && cap<b) ? cap : b}'
}

# timed <label-file> <command...>: runs it, writes "user sys wall" seconds.
timed() {
  local out=$1; shift
  local TIMEFORMAT='%U %S %R'
  { time "$@" 2>"$out.err"; } 2>"$out" || { cat "$out.err" >&2; return 1; }
}

# x264_encode <src> <out> <preset> <crf> <cap bps|0> [extra ffmpeg args...]
# The app's video arguments. RATE=none drops -maxrate (plain CRF, which the
# app never runs); RATE=raw passes the cap as given, past the app's limit.
x264_encode() {
  local src=$1 out=$2 preset=$3 crf=$4 cap=$5; shift 5
  local rate=()
  case ${RATE:-app} in
  app) cap=$(maxrate_of "$src" "$cap"); rate=(-maxrate "$cap" -bufsize $((2 * cap))) ;;
  raw) rate=(-maxrate "$cap" -bufsize $((2 * cap))) ;;
  esac
  timed "$out.time" ${PIN:-} ffmpeg -nostdin -v error -y -copyts -ss 0 -i "$src" -map 0:v:0 \
    -vf "yadif=mode=send_frame:parity=auto:deint=interlaced,scale=$(size_of "$src"):flags=bicubic,format=yuv420p" \
    -c:v libx264 -preset "$preset" -profile:v high -level 51 \
    -crf "$crf" "${rate[@]}" -x264-params "${PARAMS:-$X264_PARAMS}" \
    -forced-idr 1 -force_key_frames 'expr:gte(t,n_forced*4)' "$@" -an "$out"
}

# vmaf <distorted> <reference> -> "vmaf_mean vmaf_p5 vmaf_min psnr_y ssim"
# The reference is scaled as the app scales (bicubic) when the sizes differ.
# Every fifth frame is scored (5 shares no factor with the 3-frame pattern of
# bframes=2 or SVT-AV1's 16-frame one), which made the matrix affordable on a
# machine other work was using; VMAF_SUBSAMPLE=1 scores them all.
vmaf() {
  local dist=$1 ref=$2 log
  log=$(mktemp -p "$S/logs" vmaf.XXXXXX.json)
  local size; size=$(ffprobe -v error -select_streams v:0 -show_entries stream=width,height -of csv=s=x:p=0 "$dist")
  # Frames are paired by number (-r on both inputs): paired by time, Matroska's
  # millisecond times put every third frame of a 24 fps source against its
  # neighbour, and a clip in motion scored 68 where it scores 95.5.
  local fps; fps=$(ffprobe -v error -select_streams v:0 -show_entries stream=r_frame_rate -of csv=p=0 "$ref")
  nice ffmpeg -nostdin -v error -r "$fps" -i "$dist" -r "$fps" -i "$ref" -lavfi \
    "[0:v]format=yuv420p[d];[1:v]scale=${size/x/:}:flags=bicubic,format=yuv420p[r];[d][r]libvmaf=n_threads=${VMAF_THREADS:-4}:n_subsample=${VMAF_SUBSAMPLE:-5}:feature=name=psnr|name=float_ssim:log_fmt=json:log_path=$log" \
    -f null - >&2
  jq -r '([.frames[].metrics.vmaf] | sort) as $v
         | [ (.pooled_metrics.vmaf.mean), $v[($v|length)*0.05|floor], $v[0],
             (.pooled_metrics.psnr_y.mean), (.pooled_metrics.float_ssim.mean) ]
         | map(.*10000|round/10000) | @tsv' "$log" | tr '\t' ' '
  rm -f "$log"
}

kbps() { # video bitrate of a file over its duration
  ffprobe -v error -show_entries format=duration,size -of csv=p=0 "$1" | awk -F, '{printf "%.0f", $2*8/$1/1000}'
}
seconds_of() { ffprobe -v error -show_entries format=duration -of csv=p=0 "$1"; }
cpu_per_s() { # <time-file> <film seconds>: CPU seconds (user+sys) per second of film
  awk -v d="$2" '{printf "%.2f", ($1+$2)/d}' "$1"
}
