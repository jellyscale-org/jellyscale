#!/usr/bin/env bash
# Makes the library test/roles.sh and test/e2e/run.sh share, with no S3:
# <workdir>/media (make.sh's films, and Zeta, which AC3 audio makes transcode),
# and <workdir>/config.json over it, ready for `jellyscale --config`.
# Usage: posix.sh <workdir>
set -uo pipefail
work=$1
repo=$(cd "$(dirname "$0")/../.." && pwd)
mkdir -p "$work"
work=$(cd "$work" && pwd)

bash "$repo/test/fixtures/make.sh" "$work/media" || exit 1
rm -rf "$work/assets" "$work/scratch" "$work/segments"

# A film Chrome can't direct-play: AC3 audio forces a transcode, and a second
# track gives the audio menu something to switch to.
zeta="$work/media/transcode/Zeta Film (2023)/Zeta Film (2023).mkv"
if [[ ! -f $zeta ]]; then
  mkdir -p "$(dirname "$zeta")"
  ffmpeg -nostdin -v error -y \
    -f lavfi -i "testsrc2=size=1280x720:rate=24:duration=180" \
    -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=180" \
    -f lavfi -i "sine=frequency=880:sample_rate=48000:duration=180" \
    -map 0:v -map 1:a -map 2:a -c:v libx264 -preset ultrafast -crf 26 -g 48 -c:a:0 ac3 -c:a:1 aac \
    -metadata:s:a:0 language=eng -metadata:s:a:1 language=spa "$zeta" || exit 1
fi

# Segments go to a directory here, and to S3 in run.sh's config-s3.json, so the
# transcode store is exercised on both. Idle is short so closing a tab shows.
cat >"$work/config.json" <<E2
{"name": "e2e", "adminUser": "admin", "webDir": "$repo/web-dist", "scanInterval": "1h",
 "transcodeStore": "file://$work/segments", "transcodeScratch": "$work/scratch", "transcodeIdle": "10s", "transcodeLead": "20s", "libraries": [
  {"name": "Movies", "kind": "movies", "media": "file://$work/media/movies", "assets": "file://$work/assets/movies"},
  {"name": "Shows", "kind": "tvshows", "media": "file://$work/media/shows", "assets": "file://$work/assets/shows"},
  {"name": "Transcode", "kind": "movies", "media": "file://$work/media/transcode", "assets": "file://$work/assets/transcode"}
]}
E2
