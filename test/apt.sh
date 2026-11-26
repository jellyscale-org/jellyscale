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
# A runner image can carry a newer exact-version dependency than the mirror's
# package. --allow-downgrades alone lets apt accept the older version but does
# not make it choose one (merge-group run 45736550858; PR 285's first fix). The
# cached .debs may already have installed an older Mesa driver, whose exact
# mesa-libgallium must come down to the same version. Pin both to the installed
# driver (or the mirror's candidate when none is installed).
packages=("$@")
driver_version=$(dpkg-query -W -f='${Version}' mesa-va-drivers 2>/dev/null || true)
if [[ -z $driver_version ]]; then
  driver_version=$(apt-cache policy mesa-va-drivers 2>/dev/null | sed -n 's/^  Candidate: //p' | head -1)
fi
if [[ -n $driver_version ]]; then
  packages+=("mesa-va-drivers=$driver_version" "mesa-libgallium=$driver_version")
fi
sudo apt-get -o Acquire::Retries=3 install -y --reinstall --allow-downgrades "${packages[@]}"
if [[ -n $debs ]]; then
  mkdir -p "$debs"
  cp "$archives"/*.deb "$debs"/
fi
