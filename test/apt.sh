#!/usr/bin/env bash
# test/apt.sh <packages...> installs packages on a CI runner. The runners'
# Azure mirror of Ubuntu stalled for hours on 2026-11-11 (every package timed
# out at 20 s and every job hit its time limit), and a mirror step took 7.5 to
# 13.8 runner-minutes a run, so CI keeps the .deb files in a cache
# (actions/cache, keyed on the runner image version and the package list) in
# the directory APT_DEBS names.
#   A hit (a file for every package in APT_DEBS): installs those files with dpkg,
#   with no apt-get update and no download. If that install fails (a dependency the
#   cached files lack: run 44012242557), it installs as on a miss instead of
#   failing the job.
#   A miss: installs from the main archive, with retries, and leaves the .deb of
#   every package it installed, dependencies included, in APT_DEBS, for the cache
#   to save. APT_ARCHIVES is where apt leaves its downloads (the default is right
#   on a runner).
# Without APT_DEBS it only installs.
set -euo pipefail
debs=${APT_DEBS:-}
archives=${APT_ARCHIVES:-/var/cache/apt/archives}

if [[ -n $debs ]]; then
  # Every package of the list must have its file, or the cache is of another list.
  files=("$(realpath -m "$debs")"/*.deb)
  for p in "$@"; do compgen -G "$debs/${p}_*.deb" >/dev/null || files=(); done
  if [[ ${#files[@]} -gt 0 ]]; then
    echo "installing ${#files[@]} cached .deb files, no mirror"
    # dpkg installs exactly these files and never asks a mirror. apt-get install --no-download, which this replaced,
    # still found archives to fetch with every file of the cache given (queue runs of #184 and #185), so it fell back.
    if sudo dpkg -i "${files[@]}"; then exit 0; fi
    echo "the cached .deb files are not enough; installing from the mirror"
  fi
fi

for f in /etc/apt/sources.list.d/ubuntu.sources /etc/apt/sources.list; do
  if [[ -f $f ]]; then sudo sed -i 's|http://azure.archive.ubuntu.com|http://archive.ubuntu.com|g' "$f"; fi
done
# So that the archive holds only what this install downloads.
[[ -n $debs ]] && sudo apt-get clean
sudo apt-get -o Acquire::Retries=3 update
# --reinstall: a package the runner image already has (fontconfig, fonts-dejavu-core) is
# otherwise never downloaded, so it would leave no .deb and every later run would miss.
# A cached .deb can carry an older Mesa VA driver whose exact mesa-libgallium
# the runner no longer has and the mirror no longer offers (merge-group run
# 45736550858; PR 285). dpkg leaves it half-configured and apt refuses to resolve
# around it. CI encodes in software, so the optional VA packages are removed; the
# required packages install without recommendations, which also keeps the
# mirror download inside the job's 5-minute bound.
sudo dpkg --purge --force-all mesa-va-drivers va-driver-all >/dev/null 2>&1 || true
# Keep the mirror install inside the job's five-minute bound: required packages only.
sudo apt-get -o Acquire::Retries=3 install -y --reinstall --no-install-recommends "$@"
if [[ -n $debs ]]; then
  mkdir -p "$debs"
  cp "$archives"/*.deb "$debs"/
fi
