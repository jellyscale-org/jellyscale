#!/usr/bin/env bash
# Makes "Kino Film", whose audio is German first and English second: <dir>/languages/Kino Film/Kino Film.mkv.
# Streams: 0 video; audio 1 deu (the default), 2 eng; text subtitles 3 deu, 4 eng, 5 fra; no file beside it.
# Usage: kino.sh <dir>
set -euo pipefail
dir="$1/languages/Kino Film"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$dir"
for n in 3 4 5; do printf '1\n00:00:01,000 --> 00:00:05,000\nKino track %d\n\n' "$n" >"$tmp/s$n.srt"; done
ffmpeg -nostdin -v error -y \
  -f lavfi -i "testsrc2=size=640x360:rate=24:duration=20" \
  -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=20" \
  -f lavfi -i "sine=frequency=550:sample_rate=48000:duration=20" \
  -i "$tmp/s3.srt" -i "$tmp/s4.srt" -i "$tmp/s5.srt" \
  -map 0:v -map 1:a -map 2:a -map 3 -map 4 -map 5 \
  -c:v libx264 -preset ultrafast -g 48 -c:a aac -c:s srt -default_mode passthrough \
  -metadata:s:a:0 language=deu -metadata:s:a:1 language=eng \
  -metadata:s:s:0 language=deu -metadata:s:s:1 language=eng -metadata:s:s:2 language=fra \
  -disposition:a:0 default -disposition:a:1 0 \
  "$dir/Kino Film.mkv"
