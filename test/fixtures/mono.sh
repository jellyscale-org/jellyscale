#!/usr/bin/env bash
# Makes "Mono Film", whose only audio is English: <dir>/languages/Mono Film/Mono Film.mkv. Streams: 0 video; audio 1 eng.
# For the scenario of a viewer whose audio language is German and who has no German track to be given. Usage: mono.sh <dir>
set -euo pipefail
dir="$1/languages/Mono Film"
mkdir -p "$dir"
ffmpeg -nostdin -v error -y \
  -f lavfi -i "testsrc2=size=640x360:rate=24:duration=20" \
  -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=20" \
  -map 0:v -map 1:a \
  -c:v libx264 -preset ultrafast -g 48 -c:a aac -metadata:s:a:0 language=eng -disposition:a:0 default \
  "$dir/Mono Film.mkv"
