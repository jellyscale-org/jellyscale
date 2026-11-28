#!/usr/bin/env bash
# Generate a short, invented test film as fMP4 HLS with relative playlist URLs.
set -euo pipefail
out=$1
mkdir -p "$out"
ffmpeg -y -hide_banner -loglevel error \
  -f lavfi -i 'testsrc2=size=1280x720:rate=25' \
  -f lavfi -i 'sine=frequency=440:sample_rate=48000' \
  -t 180 -c:v libx264 -preset veryfast -pix_fmt yuv420p \
  -g 100 -keyint_min 100 -sc_threshold 0 -b:v 1500k \
  -c:a aac -b:a 128k \
  -f hls -hls_time 4 -hls_segment_type fmp4 -hls_playlist_type vod \
  -hls_segment_filename "$out/seg%03d.m4s" "$out/main.m3u8"
cat > "$out/master.m3u8" <<'EOF'
#EXTM3U
#EXT-X-VERSION:7
#EXT-X-INDEPENDENT-SEGMENTS
#EXT-X-STREAM-INF:BANDWIDTH=1700000,CODECS="avc1.4d401f,mp4a.40.2",RESOLUTION=1280x720,FRAME-RATE=25
main.m3u8
EOF
