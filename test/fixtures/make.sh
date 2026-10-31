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

# Films with text subtitles, for the Transcode library. Each has a SubRip track
# (eng, default) and an ASS track (jpn) whose cues come every ten seconds, so a
# cue is always near, and the ASS style has a colour the picture doesn't.
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
{
  for n in $(seq 0 11); do
    printf '%d\n00:%02d:%02d,000 --> 00:%02d:%02d,000\nEnglish line %d\n\n' $((n + 1)) $((n * 10 / 60)) $((n * 10 % 60 + 1)) $((n * 10 / 60)) $((n * 10 % 60 + 6)) "$n"
  done
} >"$tmp/lines.srt"
ass_head='[Script Info]
ScriptType: v4.00+
PlayResX: 1280
PlayResY: 720

[V4+ Styles]
Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding
Style: Loud,Arial,64,&H0000FF00,&H000000FF,&H00000000,&H00000000,1,0,0,0,100,100,0,0,1,3,0,2,10,10,40,1
Style: High,Arial,64,&H00FFFFFF,&H000000FF,&H00000000,&H00000000,1,0,0,0,100,100,0,0,1,3,0,8,10,10,40,1
Style: Low,Arial,64,&H00FFFFFF,&H000000FF,&H00000000,&H00000000,1,0,0,0,100,100,0,0,1,3,0,2,10,10,40,1
Style: Mid,Arial,64,&H00FFFFFF,&H000000FF,&H00000000,&H00000000,1,0,0,0,100,100,0,0,1,3,0,5,10,10,40,1

[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text'
{
  echo "$ass_head"
  for n in $(seq 0 11); do
    printf 'Dialogue: 0,0:%02d:%02d.00,0:%02d:%02d.00,Loud,,0,0,0,,ASS line %d\n' $((n * 10 / 60)) $((n * 10 % 60 + 1)) $((n * 10 / 60)) $((n * 10 % 60 + 6)) "$n"
  done
} >"$tmp/lines.ass"
# On a black picture, so the burnt-in text is the only bright thing in a frame:
# high from 20 to 40 seconds, low from 60 to 80, middle from 100 to 120.
{
  echo "$ass_head"
  printf 'Dialogue: 0,0:00:20.00,0:00:40.00,High,,0,0,0,,HIGH\nDialogue: 0,0:01:00.00,0:01:20.00,Low,,0,0,0,,LOW\nDialogue: 0,0:01:40.00,0:02:00.00,Mid,,0,0,0,,MID\n'
} >"$tmp/place.ass"
printf '1\n00:01:00,000 --> 00:01:20,000\nLOW\n\n' >"$tmp/place.srt"

subtitled() { # output audio_codec picture srt ass
  mkdir -p "$(dirname "$1")"
  [[ -f $1 ]] || ffmpeg -nostdin -v error -y \
    -f lavfi -i "$3" -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=120" -i "$4" -i "$5" \
    -map 0:v -map 1:a -map 2 -map 3 -c:v libx264 -preset ultrafast -crf 30 -g 48 -c:a "$2" -c:s:0 srt -c:s:1 ass \
    -metadata:s:a:0 language=eng -metadata:s:s:0 language=eng -metadata:s:s:1 language=jpn -disposition:s:0 default "$1"
}
testsrc="testsrc2=size=640x360:rate=24:duration=120"
subtitled "$root/transcode/Sigma Film (2034)/Sigma Film (2034).mkv" aac "$testsrc" "$tmp/lines.srt" "$tmp/lines.ass"
subtitled "$root/transcode/Tau Film (2035)/Tau Film (2035).mkv" ac3 "$testsrc" "$tmp/lines.srt" "$tmp/lines.ass"
subtitled "$root/transcode/Phi Film (2037)/Phi Film (2037).mkv" aac "color=c=black:size=640x360:rate=24:duration=120" "$tmp/place.srt" "$tmp/place.ass"

# A film whose only subtitle track is an image one. The probe reads only the
# codec's name, so the track is an ASS one renamed to PGS in the file.
upsilon="$root/transcode/Upsilon Film (2036)/Upsilon Film (2036).mkv"
if [[ ! -f $upsilon ]]; then
  mkdir -p "$(dirname "$upsilon")"
  ffmpeg -nostdin -v error -y -f lavfi -i "testsrc2=size=640x360:rate=24:duration=20" \
    -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=20" -i "$tmp/lines.ass" \
    -map 0:v -map 1:a -map 2 -c:v libx264 -preset ultrafast -c:a aac -c:s ass "$upsilon"
  LC_ALL=C sed -i 's/S_TEXT\/ASS/S_HDMV\/PGS/' "$upsilon"
fi
