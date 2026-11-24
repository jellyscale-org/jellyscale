#!/usr/bin/env bash
# Makes "Trio Film", whose first subtitle, flagged-default subtitle and English subtitle are three different tracks, for the
# scenarios of the default subtitle a viewer's mode and language choose (JS-041): <dir>/languages/Trio Film/Trio Film.mkv.
# Streams: 0 video; audio 1 eng; text subtitles 2 fra (the first), 3 deu (flagged default), 4 eng; no file beside it. Each
# track has one cue, from 1 to 5 seconds, that says which it is ("Trio track 2"), so what is on screen names the track.
# Usage: trio.sh <dir>
set -euo pipefail
dir="$1/languages/Trio Film"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$dir"
for n in 2 3 4; do printf '1\n00:00:01,000 --> 00:00:05,000\nTrio track %d\n\n' "$n" >"$tmp/s$n.srt"; done
ffmpeg -nostdin -v error -y \
  -f lavfi -i "testsrc2=size=640x360:rate=24:duration=20" \
  -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=20" \
  -i "$tmp/s2.srt" -i "$tmp/s3.srt" -i "$tmp/s4.srt" \
  -map 0:v -map 1:a -map 2 -map 3 -map 4 \
  -c:v libx264 -preset ultrafast -g 48 -c:a aac -c:s srt -default_mode passthrough \
  -metadata:s:a:0 language=eng \
  -metadata:s:s:0 language=fra -metadata:s:s:1 language=deu -metadata:s:s:2 language=eng \
  -disposition:s:0 0 -disposition:s:1 default -disposition:s:2 0 \
  "$dir/Trio Film.mkv"
