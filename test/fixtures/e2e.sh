#!/usr/bin/env bash
# Makes every film the e2e gates play, under <dir>/media: posix.sh's (make.sh's
# and Zeta), then the rest. It makes only files that ffmpeg and the recipes
# below produce. What a run does with them (hard links, a copy it cuts short) is
# test/e2e/run.sh's, so that the result can be cached whole: test/fixtures/cached.sh.
# Usage: e2e.sh <dir>
set -uo pipefail
work=$1
repo=$(cd "$(dirname "$0")/../.." && pwd)
mkdir -p "$work"
work=$(cd "$work" && pwd)

bash "$repo/test/fixtures/posix.sh" "$work" || exit 1
mkdir -p "$work/media/sources"
epsilon="$work/media/sources/Epsilon Film (2022).mkv"
[[ -f $epsilon ]] || ffmpeg -nostdin -v error -y \
  -f lavfi -i "testsrc2=size=1280x720:rate=24000/1001:duration=30" \
  -f lavfi -i "sine=frequency=660:sample_rate=48000:duration=30" \
  -c:v libx264 -preset ultrafast -c:a aac -metadata:s:a:0 language=eng "$epsilon" || exit 1

# Plays as it is, with a second audio track: switching to it asks for a transcode.
nu="$work/media/transcode/Nu Film (2030)/Nu Film (2030).mkv"
if [[ ! -f $nu ]]; then
  mkdir -p "$(dirname "$nu")"
  ffmpeg -nostdin -v error -y \
    -f lavfi -i "testsrc2=size=1280x720:rate=24:duration=60" \
    -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=60" \
    -f lavfi -i "sine=frequency=880:sample_rate=48000:duration=60" \
    -map 0:v -map 1:a -map 2:a -c:v libx264 -preset ultrafast -crf 26 -g 48 -c:a aac \
    -metadata:s:a:0 language=eng -metadata:s:a:1 language=spa "$nu" || exit 1
fi

# Plays as it is, and is big enough that Chrome has not buffered all of it when
# a scenario seeks to its last fifth: 120 seconds at a constant 4 Mbit/s, 60 MB
# (the outage scenario of JS-029 seeks while Postgres is away and needs the browser to ask for a range).
outage="$work/media/transcode/Outage Direct Film (2113)/Outage Direct Film (2113).mkv"
if [[ ! -f $outage ]]; then
  mkdir -p "$(dirname "$outage")"
  ffmpeg -nostdin -v error -y \
    -f lavfi -i "testsrc2=size=1280x720:rate=24:duration=120" \
    -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=120" \
    -c:v libx264 -preset ultrafast -b:v 4M -minrate 4M -maxrate 4M -bufsize 4M -x264-params nal-hrd=cbr:force-cfr=1 -g 48 \
    -c:a aac "$outage" || exit 1
fi

# Films whose video Chrome plays and whose audio it doesn't, with keyframes at
# the irregular times of the captured fixture (compat/captures/transcode-12.4.md),
# so a copied stream's playlist is no fixed grid: Matroska and MP4 (moov at the
# end, with an edit list, as ffmpeg writes it), and a Matroska with no Cues,
# which has no index to cut on and is re-encoded.
keyframes=0,3,7,13,18,26,31,39,44,52,57,65,70,78,83
rho="$work/media/transcode/Rho Film (2041)/Rho Film (2041).mkv"
if [[ ! -f $rho ]]; then
  mkdir -p "$(dirname "$rho")"
  ffmpeg -nostdin -v error -y \
    -f lavfi -i "testsrc2=size=640x360:rate=24000/1001:duration=90" \
    -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=90" \
    -f lavfi -i "sine=frequency=880:sample_rate=48000:duration=90" \
    -map 0:v -map 1:a -map 2:a -c:v libx264 -preset veryfast -x264-params bframes=2:scenecut=0 -force_key_frames "$keyframes" \
    -c:a:0 ac3 -c:a:1 aac -metadata:s:a:0 language=eng -metadata:s:a:1 language=spa "$rho" || exit 1
fi
qoppa="$work/media/transcode/Qoppa Film (2042)/Qoppa Film (2042).mp4"
[[ -f $qoppa ]] || { mkdir -p "$(dirname "$qoppa")" && ffmpeg -nostdin -v error -y -i "$rho" -map 0 -c copy "$qoppa" || exit 1; }
# AV1 and HEVC with the captured keyframes, which the encoders keep to when
# their own interval is longer than the film (-g 2000, keyint=2000). Chrome plays
# AV1 in software, so Sampi and San are copied and Heth plays as it is; it plays
# HEVC only with a hardware decoder, which CI doesn't have (Yot, Lamed).
# x265 and SVT-AV1 print their configuration on stderr whatever -v says.
codec_film() { # file container video_encoder_args audio_codec
  local file="$work/media/transcode/$1/$1.$2"
  [[ -f $file ]] && return
  mkdir -p "$(dirname "$file")"
  # shellcheck disable=SC2086
  ffmpeg -nostdin -v error -y \
    -f lavfi -i "testsrc2=size=640x360:rate=24000/1001:duration=90" \
    -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=90" \
    -map 0:v -map 1:a $3 -force_key_frames "$keyframes" -c:a "$4" -metadata:s:a:0 language=eng "$file" 2>/dev/null || { echo "could not make $file" >&2; exit 1; }
}
# SVT-AV1 in ffmpeg 6.1 (Ubuntu 24.04, which CI runs) ignores -force_key_frames and
# keeps the first frame's keyframe alone, which is a film with no index to cut a
# copy on. So the video is encoded in pieces that each start at a keyframe time
# and joined: any ffmpeg then gives the same keyframes.
av1_video="$work/media/sources/av1-video.mkv"
if [[ ! -f $av1_video ]]; then
  parts=$(mktemp -d)
  from=0
  : >"$parts/list"
  after_first=${keyframes#0,}
  for t in ${after_first//,/ } 90; do # The film ends at 90 s, the last piece's end.
    to=$(( (t * 24000 + 1000) / 1001 )) # The first frame at or after t seconds.
    ffmpeg -nostdin -v error -y -f lavfi -i "testsrc2=size=640x360:rate=24000/1001:duration=90" \
      -vf "trim=start_frame=$from:end_frame=$to,setpts=PTS-STARTPTS" -c:v libsvtav1 -preset 10 -g 2000 "$parts/$from.mkv" 2>/dev/null || { echo "could not encode AV1" >&2; exit 1; }
    echo "file '$parts/$from.mkv'" >>"$parts/list"
    from=$to
  done
  ffmpeg -nostdin -v error -y -f concat -safe 0 -i "$parts/list" -c copy "$av1_video" || exit 1
  rm -rf "$parts"
fi
av1_film() { # file container audio_codec
  local file="$work/media/transcode/$1/$1.$2"
  [[ -f $file ]] && return
  mkdir -p "$(dirname "$file")"
  ffmpeg -nostdin -v error -y -i "$av1_video" -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=90" \
    -map 0:v -map 1:a -c:v copy -c:a "$3" -metadata:s:a:0 language=eng "$file" || { echo "could not make $file" >&2; exit 1; }
}
hevc="-c:v libx265 -preset veryfast -x265-params keyint=2000:log-level=error"
av1_film "Sampi Film (2080)" mkv ac3
av1_film "San Film (2081)" mp4 ac3
av1_film "Heth Film (2082)" mp4 aac
codec_film "Yot Film (2083)" mp4 "$hevc" aac
codec_film "Lamed Film (2084)" mkv "$hevc" ac3
# HDR: 10-bit HEVC in PQ (Resh) and HLG (Shin), and H.264 in PQ (Tav), which a
# copy would send to the client as washed-out SDR. All are re-encoded to SDR
# BT.709 by a tone-mapping transcode, whatever Chrome plays.
hdr="-pix_fmt yuv420p10le -x265-params keyint=2000:log-level=error:colorprim=bt2020:colormatrix=bt2020nc"
codec_film "Resh Film (2085)" mkv "-c:v libx265 -preset veryfast $hdr:transfer=smpte2084" ac3
codec_film "Shin Film (2086)" mp4 "-c:v libx265 -preset veryfast $hdr:transfer=arib-std-b67" aac
codec_film "Tav Film (2087)" mkv "-pix_fmt yuv420p10le -c:v libx264 -preset veryfast -x264-params colorprim=bt2020:transfer=smpte2084:colormatrix=bt2020nc" aac
# ffmpeg writes the Cues last, so cutting the file where they start leaves a
# film with its duration and no Cues; the SeekHead still points to them.
nocues="$work/media/transcode/Digamma Film (2045)/Digamma Film (2045).mkv"
if [[ ! -f $nocues ]]; then
  mkdir -p "$(dirname "$nocues")"
  python3 -I -c 'import sys; d = open(sys.argv[1], "rb").read(); i = d.rfind(bytes.fromhex("1c53bb6b")); assert i > len(d) * 0.9; open(sys.argv[2], "wb").write(d[:i])' "$rho" "$nocues" || exit 1
fi
# H.264 that has to be re-encoded, because it is interlaced, with the same
# structure as Zeta (two audio tracks, a keyframe every two seconds): what the
# scenarios that follow Zeta, Theta and Mu run on, to show re-encoding works.
for interlaced in "Zeta Interlaced (2050)" "Theta Interlaced (2051)" "Mu Interlaced (2052)"; do
  file="$work/media/transcode/$interlaced/$interlaced.mkv"
  [[ -f $file ]] && continue
  mkdir -p "$(dirname "$file")"
  ffmpeg -nostdin -v error -y \
    -f lavfi -i "testsrc2=size=640x360:rate=25:duration=180" \
    -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=180" \
    -f lavfi -i "sine=frequency=880:sample_rate=48000:duration=180" \
    -map 0:v -map 1:a -map 2:a -vf interlace=scan=tff -c:v libx264 -preset ultrafast -flags +ilme+ildct -x264-params tff=1 -g 50 \
    -c:a:0 ac3 -c:a:1 aac -metadata:s:a:0 language=eng -metadata:s:a:1 language=spa "$file" || exit 1
done
# Interlaced H.264 whose audio Chrome plays: the video is re-encoded and the audio
# copied. Two AAC tracks (440 and 880 Hz) to switch between; the MP4 has the
# bitrate of its tracks, which the bitrate cap scenario counts the copy by.
for copied in "Audio Copy Film (2090)" "Audio Cap Film (2091)"; do
  ext=mkv; [[ $copied == "Audio Cap"* ]] && ext=mp4
  file="$work/media/transcode/$copied/$copied.$ext"
  [[ -f $file ]] && continue
  mkdir -p "$(dirname "$file")"
  ffmpeg -nostdin -v error -y \
    -f lavfi -i "testsrc2=size=640x360:rate=25:duration=120" \
    -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=120" \
    -f lavfi -i "sine=frequency=880:sample_rate=48000:duration=120" \
    -map 0:v -map 1:a -map 2:a -vf interlace=scan=tff -c:v libx264 -preset ultrafast -flags +ilme+ildct -x264-params tff=1 -g 50 \
    -c:a aac -b:a 128k -metadata:s:a:0 language=eng -metadata:s:a:1 language=spa "$file" || exit 1
done
# Interlaced H.264 with Opus or FLAC audio, both listed in Chrome's profile: the
# video is re-encoded and the audio copied.
for codec in opus flac; do
  name="Audio ${codec^} Film ($([[ $codec == opus ]] && echo 2092 || echo 2093))"
  file="$work/media/transcode/$name/$name.mkv"
  [[ -f $file ]] && continue
  mkdir -p "$(dirname "$file")"
  encoder=$([[ $codec == opus ]] && echo libopus || echo flac)
  ffmpeg -nostdin -v error -y \
    -f lavfi -i "testsrc2=size=640x360:rate=25:duration=120" \
    -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=120" \
    -map 0:v -map 1:a -vf interlace=scan=tff -c:v libx264 -preset ultrafast -flags +ilme+ildct -x264-params tff=1 -g 50 \
    -c:a "$encoder" -ac 2 "$file" || exit 1
done
# Films with chapters (Opening at 0 s, Middle Part at 20 s, and one untitled at
# 45.5 s, which Jellyfin names "Chapter 3"). Chrome plays the first two as they are.
# The other two are transcoded, the audio of one needs it (AC3, so its video is
# copied) and the other's video does (interlaced), and their streams start 2 s
# late: chapters are on the file's clock, which a transcode keeps (-copyts), so a
# chapter seek must land where the file says, not 2 s early or late.
chapters="$work/media/sources/chapters.txt"
cat >"$chapters" <<'E3'
;FFMETADATA1
[CHAPTER]
TIMEBASE=1/1000
START=0
END=20000
title=Opening
[CHAPTER]
TIMEBASE=1/1000
START=20000
END=45500
title=Middle Part
[CHAPTER]
TIMEBASE=1/1000
START=45500
END=90000
E3
chaptered() { # name container offset_seconds video_args audio_codec
  local file="$work/media/transcode/$1/$1.$2"
  [[ -f $file ]] && return
  mkdir -p "$(dirname "$file")"
  # shellcheck disable=SC2086
  ffmpeg -nostdin -v error -y \
    -itsoffset "$3" -f lavfi -i "testsrc2=size=640x360:rate=24:duration=90" \
    -itsoffset "$3" -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=90" \
    -i "$chapters" -map 0:v -map 1:a -map_metadata 2 -map_chapters 2 $4 -c:a "$5" -metadata:s:a:0 language=eng "$file" || { echo "could not make $file" >&2; exit 1; }
}
chaptered "Chapters Direct (2095)" mkv 0 "-c:v libx264 -preset ultrafast -g 48" aac
chaptered "Chapters Mp4 (2096)" mp4 0 "-c:v libx264 -preset ultrafast -g 48" aac
chaptered "Chapters Copy (2097)" mkv 2 "-c:v libx264 -preset ultrafast -g 48" ac3
chaptered "Chapters Encode (2098)" mkv 2 "-vf interlace=scan=tff -c:v libx264 -preset ultrafast -flags +ilme+ildct -x264-params tff=1 -g 48" aac
# A 1080p film for measuring what a copy saves: noisy, so the encode has work to do.
big="$work/media/transcode/Stigma Film (2047)/Stigma Film (2047).mkv"
if [[ ! -f $big ]]; then
  mkdir -p "$(dirname "$big")"
  ffmpeg -nostdin -v error -y \
    -f lavfi -i "testsrc2=size=1920x1080:rate=24:duration=60" \
    -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=60" \
    -vf "noise=alls=20:allf=t" -c:v libx264 -preset veryfast -b:v 8M -maxrate 8M -bufsize 16M -g 48 -c:a ac3 "$big" || exit 1
fi


# Dolby Vision and HDR10+ labels, under media/hdr: the films the labels are read
# from (the HDR library of the differential tier, and the checks of JS-023).
# test/fixtures/dovi makes them (testenv.HDRFilm): an HDR10, HLG or SDR base
# layer, or AV1, with the Dolby Vision configuration box a server reads the
# profile and the base layer's compatibility id from, and the HDR10+ SEI x265
# writes. There are no RPUs, because no tool here makes them. Each is 20 s.
dovi="$work/media/sources/dovi"
(cd "$repo/test/fixtures" && go build -o "$dovi" ./dovi) || exit 1
hdr_film() { # name base [hdr10plus] [dv=profile.bl_compatibility_id[.el]]
  local name=$1 file="$work/media/hdr/$1/$1.mp4"
  [[ -f $file ]] && return
  mkdir -p "$(dirname "$file")"
  "$dovi" "$file" 20 "${@:2}" || { echo "could not make $file" >&2; exit 1; }
}
# AV1 in HDR10 with AC3 audio in Matroska, in the Transcode library: CI's Chrome
# plays AV1 but not AC3, so the video is copied with its PQ range and only the
# audio is re-encoded (JS-073; with HEVC, which CI's Chrome does not play, this
# copy is checked only with E2E_HEVC=1). 40 s, a keyframe every 2 s, so a seek
# lands between keyframes of a copy.
av1_hdr="$work/media/transcode/Kaf Film (2088)/Kaf Film (2088).mkv"
if [[ ! -f $av1_hdr ]]; then
  mkdir -p "$(dirname "$av1_hdr")" "$work/media/sources"
  "$dovi" "$work/media/sources/av1-hdr10.mp4" 40 av1 || { echo "could not make $av1_hdr" >&2; exit 1; }
  ffmpeg -nostdin -v error -y -i "$work/media/sources/av1-hdr10.mp4" -map 0:v -map 0:a -c:v copy -c:a ac3 -metadata:s:a:0 language=eng "$av1_hdr" || { echo "could not make $av1_hdr" >&2; exit 1; }
fi
hdr_film "Plain Sdr Film (1990)" sdr
hdr_film "Plain Pq Film (1991)" pq
hdr_film "Plain Hlg Film (1992)" hlg
hdr_film "Plain Plus Film (1993)" pq hdr10plus
hdr_film "Dolby Five Film (1994)" pq dv=5.0
hdr_film "Dolby Seven Film (1995)" pq dv=7.6.el
hdr_film "Dolby Eight One Film (1996)" pq dv=8.1
hdr_film "Dolby Eight Two Film (1997)" sdr dv=8.2
hdr_film "Dolby Eight Four Film (1998)" hlg dv=8.4
hdr_film "Dolby Eight Zero Film (1999)" pq dv=8.0
hdr_film "Dolby Eight Three Film (2000)" pq dv=8.3
hdr_film "Dolby Ten Film (2001)" av1 dv=10.1
hdr_film "Dolby Eight One Plus Film (2002)" pq hdr10plus dv=8.1
hdr_film "Dolby Seven Plus Film (2003)" pq hdr10plus dv=7.6.el
hdr_film "Dolby Five Plus Film (2004)" pq hdr10plus dv=5.0
