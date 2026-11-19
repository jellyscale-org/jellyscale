#!/usr/bin/env bash
# gather.sh <set>: the set's lines as results/<set>.csv, sorted.
source "$(dirname "$0")/lib.sh"
{ echo clip,codec,preset,crf,cap_mbps,variant,kbps,vmaf,vmaf_p5,vmaf_min,psnr_y,ssim,cpu_s_per_s,wall_s
  cat "$S/res/$1"/*.csv | sort -t, -k1,1 -k2,2 -k6,6 -k5,5n -k4,4n -k3,3; } > "$R/$1.csv"
wc -l "$R/$1.csv"
