#!/usr/bin/env bash
# Generates a small synthetic library. Usage: make.sh <dir>
set -euo pipefail
root=$1

clip() { # output seconds
  mkdir -p "$(dirname "$1")"
  [[ -f $1 ]] || ffmpeg -nostdin -v error -y \
    -f lavfi -i "testsrc2=size=1280x720:rate=24000/1001:duration=$2" \
    -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=$2" \
    -c:v libx264 -preset ultrafast -c:a aac -metadata:s:a:0 language=eng "$1"
}

clip "$root/movies/Alpha Film (2021)/Alpha Film (2021).mkv" 30
clip "$root/movies/The Beta Film (2019)/The Beta Film (2019).mp4" 30
clip "$root/movies/Gamma.Film.2015.1080p.mkv" 20
clip "$root/shows/Delta Show (2020)/Season 01/Delta Show S01E01 Pilot.mkv" 15
clip "$root/shows/Delta Show (2020)/Season 01/Delta Show S01E02 Second.mkv" 15
clip "$root/shows/Delta Show (2020)/Season 02/Delta Show S02E01 Return.mkv" 15
