#!/usr/bin/env bash
# JS-134: makes the three streams hdr-fallback-spike.mjs plays, in <dir>/{A,B,C}, for codec hevc or av1:
#   A the copy: 10-bit PQ (BT.2020, tagged), a keyframe every 4 s, as hls fmp4 (init.mp4, 0.m4s ...)
#   B today's fallback: the same picture tone mapped (playback.ToneMapFilter) to 8-bit BT.709, the copy's codec
#   C the candidate: the same picture at 10 bits, no tone mapping and no colour flag (the frames' tags reach the encoder)
# The audio (AAC, copied) is in all three: a player that has audio takes its time from the audio track's clock, and without it a
# fallback whose video timescale differs from the copy's (12288 against the 16000 of a copy from Matroska) stalls the player.
# TIMESCALE (default 12288) is the video track timescale of all three: with the copy at 16000 (what ffmpeg gives a copy from
# Matroska) and the fallback at 12288 the player placed the fallback's segments 4.64 s late and stalled at the change, with
# audio as without (see hdr-fallback-12.4.md); the e2e gate is where the real stack's timescales meet the player.
# Usage: hdr-fallback-streams.sh <hevc|av1> <empty dir>
set -euo pipefail
codec=${1:?hevc or av1}; dir=${2:?dir}; mkdir -p "$dir"; cd "$dir"
TAGS="-color_primaries bt2020 -color_trc smpte2084 -colorspace bt2020nc -color_range tv"
TONE="zscale=tin=smpte2084:min=bt2020nc:pin=bt2020:rin=tv:t=linear:m=bt2020nc:p=bt2020:npl=100,format=gbrpf32le,zscale=p=bt709,tonemap=tonemap=hable:desat=0,zscale=t=bt709:m=bt709:r=tv,format=yuv420p"
HLS="-f hls -hls_time 4 -hls_list_size 0 -hls_segment_type fmp4 -hls_fmp4_init_filename init.mp4 -hls_segment_options video_track_timescale=${TIMESCALE:-12288}"
case $codec in
  hevc) enc() { echo "-c:v libx265 -preset veryfast -tag:v hvc1 -x265-params keyint=96:min-keyint=96:scenecut=0:open-gop=0:bframes=2:b-adapt=0:log-level=error -force_key_frames expr:gte(t,n_forced*4)"; } ;;
  av1)  enc() { echo "-c:v libsvtav1 -preset 10 -g 96 -force_key_frames expr:gte(t,n_forced*4)"; } ;;
esac
# (A lavfi source's frames carry no tags and -color_trc alone did not reach libx265 in ffmpeg 9: setparams does.)
# The source picture as a PQ stream of the codec (the mezzanine B and C are made from).
ffmpeg -nostdin -v error -y -f lavfi -i "testsrc2=size=640x360:rate=24:duration=48" -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=48" -map 0:v -map 1:a -c:a aac -b:a 64k -vf "setparams=color_primaries=bt2020:color_trc=smpte2084:colorspace=bt2020nc:range=tv,format=yuv420p10le" $TAGS $(enc) src.mkv 2>/dev/null
mkdir -p A B C
ffmpeg -nostdin -v error -y -i src.mkv -map 0:v -map 0:a -c copy $([ $codec = hevc ] && echo "-tag:v hvc1") $HLS -hls_segment_filename 'A/%d.m4s' A/index.m3u8
ffmpeg -nostdin -v error -y -i src.mkv -map 0:v -map 0:a -c:a copy -vf "$TONE" $(enc) $HLS -hls_segment_filename 'B/%d.m4s' B/index.m3u8 2>/dev/null
ffmpeg -nostdin -v error -y -i src.mkv -map 0:v -map 0:a -c:a copy -vf format=yuv420p10le $(enc) $HLS -hls_segment_filename 'C/%d.m4s' C/index.m3u8 2>/dev/null
ls A | wc -l
