#!/usr/bin/env bash
# test/apt.sh <packages...> installs packages on a CI runner. The runners'
# Azure mirror of Ubuntu stalled for hours on 2026-11-11 (every package timed
# out at 20 s and every job hit its time limit), so the main archive is used,
# with retries.
set -euo pipefail
for f in /etc/apt/sources.list.d/ubuntu.sources /etc/apt/sources.list; do
  if [[ -f $f ]]; then sudo sed -i 's|http://azure.archive.ubuntu.com|http://archive.ubuntu.com|g' "$f"; fi
done
sudo apt-get -o Acquire::Retries=3 update
sudo apt-get -o Acquire::Retries=3 install -y "$@"
