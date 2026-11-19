#!/usr/bin/env bash
# JS-134 observation: what ffprobe says of an HDR10 copy (HEVC and AV1), and of the fallback JS-088/JS-133 make of it
# today, for the init segment alone and for the frames of a segment with its init in front. Results are in
# hdr-fallback-12.4.md. Usage: hdr-fallback-observe.sh <empty workdir>   (needs ffmpeg with libx265 and libsvtav1)
# The fallback's ffmpeg arguments are those of recodeArgs and fragmentCommand/AV1SegmentCommand in
# internal/transcode/ffmpeg.go, and the tone map is playback.ToneMapFilter; api/transcode.go's goOn gives Recoded the
# SOURCE's tone map, so today's fallback of an HDR copy is tone mapped. FALLBACK=plain makes the fallback without it
# (the picture of Spec.ToneMap empty), 10bit is the candidate of JS-134.
set -euo pipefail
work=$(realpath -m "${1:?workdir}"); mkdir -p "$work"; cd "$work"
mode=${FALLBACK:-tonemapped}
TONE="zscale=tin=smpte2084:min=bt2020nc:pin=bt2020:rin=tv:t=linear:m=bt2020nc:p=bt2020:npl=100,format=gbrpf32le,zscale=p=bt709,tonemap=tonemap=hable:desat=0,zscale=t=bt709:m=bt709:r=tv,format=yuv420p"
PIC="yadif=mode=send_frame:parity=auto:deint=interlaced,scale=1920:1080:flags=bicubic"
[ "$mode" = tonemapped ] && PIC="$PIC,$TONE"
FMT=yuv420p; TAGS=
# FALLBACK=10bit: the picture left as it is at 10 bits. No colour flag: the decoded frames carry the copy's tags and both
# encoders write them (tags derived from the transfer would be wrong for a PQ film with P3 primaries or full range).
[ "$mode" = 10bit ] && FMT=yuv420p10le
PIC="$PIC,format=$FMT"
SRC="-f lavfi -i testsrc2=size=1920x1080:rate=24:duration=12 -pix_fmt yuv420p10le"
FRAG="-an -f hls -hls_time 0.001 -hls_list_size 0 -hls_segment_type fmp4 -hls_flags temp_file -hls_fmp4_init_filename init.mp4 -hls_segment_options use_editlist=0:avoid_negative_ts=disabled:movflags=+frag_discont+skip_sidx"
mkdir -p hevc av1
ffmpeg -nostdin -v error -y $SRC -c:v libx265 -preset veryfast -tag:v hvc1 -x265-params "colorprim=bt2020:colormatrix=bt2020nc:transfer=smpte2084:hdr10=1:master-display=G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,1):max-cll=1000,400:keyint=48:log-level=error" hevc/source.mp4
ffmpeg -nostdin -v error -y $SRC -c:v libsvtav1 -preset 12 -g 48 -svtav1-params "color-primaries=9:transfer-characteristics=16:matrix-coefficients=9:enable-hdr=1:mastering-display=G(0.265,0.690)B(0.150,0.060)R(0.680,0.320)WP(0.3127,0.3290)L(1000,0.0001):content-light=1000,400" av1/source.mp4 2>/dev/null
# The copy: CopyCommand's picture, the source's packets cut at a keyframe (the first 4 s here).
for c in hevc av1; do
  tag=; [ $c = hevc ] && tag="-tag:v hvc1"
  ffmpeg -nostdin -v error -y -i $c/source.mp4 -t 4 -map 0:v -c:v copy $tag $FRAG -hls_segment_filename "$c/copy%d.m4s" $c/copy.m3u8
  mv $c/init.mp4 $c/copy-init.mp4
done
# The fallback, one segment of 4 s, as the worker makes it.
ffmpeg -nostdin -v error -y -i hevc/source.mp4 -t 4 -map 0:v -vf "$PIC" -c:v libx265 -preset veryfast -tag:v hvc1 -crf 26 \
  -x265-params "bframes=2:b-adapt=0:open-gop=0:scenecut=0:keyint=100000:vbv-maxrate=6220:vbv-bufsize=12441:log-level=error" -forced-idr 1 $TAGS -force_key_frames 0 $FRAG -hls_segment_filename "hevc/fb%d.m4s" hevc/fb.m3u8
mv hevc/init.mp4 hevc/fb-init.mp4
ffmpeg -nostdin -v error -y -i av1/source.mp4 -t 4 -map 0:v -vf "$PIC" -c:v libsvtav1 -preset 10 -crf 32 -g 100000 $TAGS $FRAG -hls_segment_filename "av1/fb%d.m4s" av1/fb.m3u8 2>/dev/null
mv av1/init.mp4 av1/fb-init.mp4
probe() { # <label> <init> <segment>
  echo "== $1"
  echo "-- init alone"; ffprobe -v error -select_streams v:0 -show_entries stream=codec_name,profile,pix_fmt,bits_per_raw_sample,color_range,color_space,color_transfer,color_primaries -of default=nw=1 "$2"
  cat "$2" "$3" > "$work/_j.mp4"
  echo "-- first frame, init in front"; ffprobe -v error -select_streams v:0 -read_intervals %+#1 -show_frames -show_entries frame=pix_fmt,color_range,color_space,color_transfer,color_primaries:frame_side_data=side_data_type,max_content,max_average,max_luminance -of default=nw=1 "$work/_j.mp4"
  echo "-- distinct pix_fmt/transfer over all frames"; ffprobe -v error -select_streams v:0 -show_entries frame=pix_fmt,color_transfer,color_primaries,color_space,color_range -of csv=p=0 "$work/_j.mp4" | sort | uniq -c
}
for c in hevc av1; do probe "$c copy" $c/copy-init.mp4 $c/copy0.m4s; probe "$c fallback ($mode)" $c/fb-init.mp4 $c/fb0.m4s; done
rm -f _j.mp4
