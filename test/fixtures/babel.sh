#!/usr/bin/env bash
# Makes "Babel Film", the film whose tracks are in several languages, for the tests of track-language choices and for
# the fixtures image (docs/client-testing.md lists its tracks): <dir>/languages/Babel Film/Babel Film.mkv and, beside it,
# Babel Film.de.srt and Babel Film.es.srt. Streams: 0 video; audio 1 eng (the default), 2 deu, 3 fra, 4 with no language;
# subtitles 5 eng, 6 eng forced, 7 eng for the deaf and hard of hearing, 8 deu, 9 ger (not deu) forced, 10 fra, 11 with
# no language, 12 jpn as PGS. Each text track and each file has one cue that says which it is, so a client that fetches
# a track can tell which one it got. Usage: babel.sh <dir>
set -euo pipefail
dir="$1/languages/Babel Film"
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$dir"

cue() { printf '1\n00:00:01,000 --> 00:00:05,000\n%s\n\n' "$2" >"$1"; }
for n in 5 6 7 8 9 10 11; do cue "$tmp/s$n.srt" "Babel track $n"; done
cue "$dir/Babel Film.de.srt" "Babel file de"
cue "$dir/Babel Film.es.srt" "Babel file es"

(cd "$here" && go build -o "$tmp/pgs" ./pgs)
"$tmp/pgs" "$tmp/image.sup" 1920 1080 2,6,660,100,600,120

ffmpeg -nostdin -v error -y \
  -f lavfi -i "testsrc2=size=640x360:rate=24:duration=20" \
  -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=20" \
  -f lavfi -i "sine=frequency=550:sample_rate=48000:duration=20" \
  -f lavfi -i "sine=frequency=660:sample_rate=48000:duration=20" \
  -f lavfi -i "sine=frequency=770:sample_rate=48000:duration=20" \
  -i "$tmp/s5.srt" -i "$tmp/s6.srt" -i "$tmp/s7.srt" -i "$tmp/s8.srt" -i "$tmp/s9.srt" -i "$tmp/s10.srt" -i "$tmp/s11.srt" \
  -fix_sub_duration -i "$tmp/image.sup" \
  -map 0:v -map 1:a -map 2:a -map 3:a -map 4:a -map 5 -map 6 -map 7 -map 8 -map 9 -map 10 -map 11 -map 12 \
  -c:v libx264 -preset ultrafast -g 48 -c:a aac -c:s srt -c:s:7 copy -default_mode passthrough \
  -metadata:s:a:0 language=eng -metadata:s:a:1 language=deu -metadata:s:a:2 language=fra \
  -metadata:s:s:0 language=eng -metadata:s:s:1 language=eng -metadata:s:s:2 language=eng -metadata:s:s:3 language=deu \
  -metadata:s:s:4 language=ger -metadata:s:s:5 language=fra -metadata:s:s:7 language=jpn \
  -disposition:a:0 default -disposition:a:1 0 -disposition:a:2 0 -disposition:a:3 0 \
  -disposition:s:1 forced -disposition:s:2 hearing_impaired -disposition:s:4 forced \
  "$dir/Babel Film.mkv"
