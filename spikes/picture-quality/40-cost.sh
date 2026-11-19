#!/usr/bin/env bash
# CPU seconds per second of film, one encode at a time, for each preset, from
# 1080p sources and from 4K ones (to 1080 lines, as the app does), under this
# machine's ffmpeg and under Ubuntu 24.04's ffmpeg 6.1 (test/ffmpeg61.sh's
# image), which CI and some workers run. Other work on the machine inflates
# CPU time a little (shared caches, SMT), so each figure is the least of
# REPS runs.
#   40-cost.sh [crf] [x264-params]   -> results/cost-crf<crf>[-<tag>].csv
source "$(dirname "$0")/lib.sh"
crf=${1:-23} params=${2:-} tag=${3:-}
out=$R/cost-crf$crf${tag:+-$tag}.csv
image=jellyscale-ffmpeg61
docker image inspect $image >/dev/null 2>&1 || (cd "$here/../.." && test/ffmpeg61.sh true)
sources=${SOURCES:-"src/tos_action src/tos_grain src/bbb_flat src/crowd_run
         src4k/tos_action src4k/tos_action_hevc10 src4k/bbb_flat"}
# CPUS pins every run to those CPUs (taskset, and the container's cpuset).
echo ffmpeg,source,preset,crf,cpu_s_per_s,wall_s,speed,kbps > "$out"
for src in $sources; do
  for preset in ${PRESETS:-superfast veryfast faster fast medium slow}; do
    for ff in host 6.1; do
      best=""
      for _ in $(seq "${REPS:-2}"); do
        if [ $ff = host ]; then
          got=$(PIN=${CPUS:+"taskset -c $CPUS"} "$here/cost-one.sh" "$S/$src.mkv" "$preset" "$crf" 0 "$params")
        else
          got=$(docker run --rm ${CPUS:+--cpuset-cpus "$CPUS"} --user "$(id -u):$(id -g)" -e HOME="$S" -e SPIKE_SCRATCH="$S" \
            -v "$S:$S" -v "$here:$here:ro" $image "$here/cost-one.sh" "$S/$src.mkv" "$preset" "$crf" 0 "$params")
        fi
        if [ -z "$best" ] || awk -v a="${got%% *}" -v b="${best%% *}" 'BEGIN{exit !(a<b)}'; then best=$got; fi
      done
      echo "$ff,$src,$preset,$crf,${best// /,}" | tee -a "$out"
    done
  done
done
