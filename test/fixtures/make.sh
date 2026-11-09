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

# Films whose subtitles are images, which ffmpeg can't write from text: a PGS
# stream (test/fixtures/pgs) is muxed as it is, and made into VobSub and DVB.
# The film is black, with a yellow bar on the 1920x1080 canvas of a 640x360
# picture: near the top from 20 to 26 seconds, near the bottom from 60 to 66 and
# in the middle from 100 to 106, so where the bar is on screen says which cue it
# is. Keyframes are two seconds apart.
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
(cd "$here" && go build -o "$tmp/pgs" ./pgs) || exit 1
"$tmp/pgs" "$tmp/image.sup" 1920 1080 20,26,660,100,600,120 60,66,660,860,600,120 100,106,660,480,600,120 || exit 1
imagefilm() { # name audio_codec subtitle_codec
  local video="$root/transcode/$1/$1.mkv"
  mkdir -p "$(dirname "$video")"
  [[ -f $video ]] || ffmpeg -nostdin -v error -y -f lavfi -i "color=c=black:size=640x360:rate=24:duration=120" \
    -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=120" -fix_sub_duration -i "$tmp/image.sup" \
    -map 0:v -map 1:a -map 2 -c:v libx264 -preset ultrafast -g 48 -c:a "$2" -c:s "$3" \
    -metadata:s:a:0 language=eng -metadata:s:s:0 language=eng -disposition:a:0 default -disposition:s:0 0 -default_mode passthrough "$video"
}
imagefilm "Pgs Film (2070)" aac copy
imagefilm "Vobsub Film (2071)" aac dvdsub
# AC3 makes Chrome transcode it whatever the subtitle.
imagefilm "Pgs Transcode Film (2072)" ac3 copy
imagefilm "Dvb Film (2073)" aac dvbsub

# Films whose image subtitles are files beside them, with the cues of the films
# above: "Pgs Film"'s as a .sup, and as a .sub and .idx pair on a 720x480 canvas
# (the bar in the same place of the picture). Stream 2 is the file, after the
# film's video and audio. The orphan film has a .sub and an .idx that belong to
# no pair, which are not tracks.
(cd "$here" && go build -o "$tmp/vobsub" ./vobsub) || exit 1
"$tmp/vobsub" "$tmp/image" 720 480 20,26,247,44,225,53 60,66,247,382,225,53 100,106,247,213,225,53 || exit 1
imagebeside() { # name audio_codec kind (sup, vobsub or none)
  local dir="$root/transcode/$1" video="$root/transcode/$1/$1.mkv"
  mkdir -p "$dir"
  [[ -f $video ]] || ffmpeg -nostdin -v error -y -f lavfi -i "color=c=black:size=640x360:rate=24:duration=120" \
    -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=120" -map 0:v -map 1:a -c:v libx264 -preset ultrafast -g 48 -c:a "$2" \
    -metadata:s:a:0 language=eng "$video"
  case $3 in
    sup) cp "$tmp/image.sup" "$dir/$1.en.sup" ;;
    vobsub) cp "$tmp/image.sub" "$dir/$1.en.sub"; cp "$tmp/image.idx" "$dir/$1.en.idx" ;;
    none) cp "$tmp/image.sub" "$dir/$1.en.sub"; cp "$tmp/image.idx" "$dir/$1.fr.idx" ;;
  esac
}
imagebeside "Sup Beside Film (2120)" aac sup
# AC3 makes Chrome transcode it whatever the subtitle.
imagebeside "Sup Beside Transcode Film (2121)" ac3 sup
imagebeside "Idx Beside Film (2122)" aac vobsub
imagebeside "Orphan Beside Film (2127)" aac none

# Films whose subtitles are files beside them, named as Jellyfin reads them:
# an English SubRip and a Japanese ASS file, which come out as streams 2 and 3.
# Direct plays with AAC, transcodes with AC3, and the black one burn-in shows
# against, as the films above.
beside() { # name audio_codec picture srt ass
  local dir="$root/transcode/$1" video="$root/transcode/$1/$1.mkv"
  mkdir -p "$dir"
  [[ -f $video ]] || ffmpeg -nostdin -v error -y -f lavfi -i "$3" -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=120" \
    -map 0:v -map 1:a -c:v libx264 -preset ultrafast -crf 30 -g 48 -c:a "$2" -metadata:s:a:0 language=eng "$video"
  cp "$4" "$dir/$1.en.srt"
  cp "$5" "$dir/$1.ja.ass"
}
beside "Direct Sub Film (2060)" aac "$testsrc" "$tmp/lines.srt" "$tmp/lines.ass"
beside "Transcode Sub Film (2061)" ac3 "$testsrc" "$tmp/lines.srt" "$tmp/lines.ass"
beside "Burn Sub Film (2062)" aac "color=c=black:size=640x360:rate=24:duration=120" "$tmp/place.srt" "$tmp/place.ass"

# Two films with the same ASS track in a font that jellyfin-web's renderer
# doesn't have (the system's serif), one with the font attached and one without.
font=$(fc-match -f '%{file}' serif)
family=$(fc-match -f '%{family[0]}' serif)
{
  echo "${ass_head//Arial/$family}"
  printf 'Dialogue: 0,0:00:01.00,0:00:24.00,Low,,0,0,0,,Wide Font Test MWmw\n'
} >"$tmp/font.ass"
fontfilm() { # name [output options]
  local video="$root/transcode/$1/$1.mkv"
  mkdir -p "$(dirname "$video")"
  [[ -f $video ]] || ffmpeg -nostdin -v error -y -f lavfi -i "color=c=black:size=640x360:rate=24:duration=30" \
    -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=30" -i "$tmp/font.ass" \
    -map 0:v -map 1:a -map 2 -c:v libx264 -preset ultrafast -g 48 -c:a aac -c:s ass -metadata:s:s:0 language=jpn "${@:2}" "$video"
}
fontfilm "Font Film (2063)" -attach "$font" -metadata:s:t:0 mimetype=font/ttf
fontfilm "Plain Film (2064)"
# The same film twice more: one whose fonts job the e2e worker's ffmpeg fails
# for good (test/e2e/run.sh), and one whose font the gate takes from the store.
fontfilm "Cracked Fonts Film (2068)" -attach "$font" -metadata:s:t:0 mimetype=font/ttf
fontfilm "Vanishing Fonts Film (2069)" -attach "$font" -metadata:s:t:0 mimetype=font/ttf
