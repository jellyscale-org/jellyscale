#!/usr/bin/env bash
# The films whose codec is the recipe's own, shared by test/fixtures/e2e.sh and test/fixtures/make.sh (the client
# library). Source it: keyframes, hdr and codec_film are set or defined, and nothing is made until codec_film is called.

# Films whose video Chrome plays and whose audio it doesn't, with keyframes at
# the irregular times of the captured fixture (compat/captures/transcode-12.4.md),
# so a copied stream's playlist is no fixed grid.
keyframes=0,3,7,13,18,26,31,39,44,52,57,65,70,78,83

# HDR: 10-bit HEVC in PQ and HLG, and H.264 in PQ, which a copy would send to the
# client as washed-out SDR. A tone-mapping transcode makes them SDR BT.709.
hdr="-pix_fmt yuv420p10le -x265-params keyint=2000:log-level=error:colorprim=bt2020:colormatrix=bt2020nc"

codec_film() { # root name container video_encoder_args audio_codec: <root>/transcode/<name>/<name>.<container>
  local file="$1/transcode/$2/$2.$3"
  [[ -f $file ]] && return
  mkdir -p "$(dirname "$file")"
  # shellcheck disable=SC2086
  ffmpeg -nostdin -v error -y \
    -f lavfi -i "testsrc2=size=640x360:rate=24000/1001:duration=90" \
    -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=90" \
    -map 0:v -map 1:a $4 -force_key_frames "$keyframes" -c:a "$5" -metadata:s:a:0 language=eng "$file" 2>/dev/null || { echo "could not make $file" >&2; exit 1; }
}
