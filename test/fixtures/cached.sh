#!/usr/bin/env bash
# Puts the e2e films (test/fixtures/e2e.sh) in <workdir>/media from a cache,
# making them first if the cache has none for the recipes and ffmpeg at hand.
# Usage: cached.sh <workdir>
# E2E_FIXTURE_CACHE is the cache directory, default ~/.cache/jellyscale/e2e-fixtures.
# An entry is <cache>/<key>/media, key from key.sh. It is made in a temp dir and
# renamed, so a killed run leaves no half entry, and it is never changed after.
# Only the films are cached; a run's hard links, copies and everything else stay in its workdir.
set -euo pipefail
work=$1
here=$(cd "$(dirname "$0")" && pwd)
cache=${E2E_FIXTURE_CACHE:-$HOME/.cache/jellyscale/e2e-fixtures}
key=$(bash "$here/key.sh")
mkdir -p "$cache" "$work"
# Old keys, and the half-copied entries of runs killed during the copy below.
# Never the current key: CI restores an entry with its first save's mtime, and
# saves no newer one while the key matches.
find "$cache" -mindepth 1 -maxdepth 1 ! -name "$key" -mtime +14 -exec rm -rf {} + 2>/dev/null || true
find "$cache" -mindepth 1 -maxdepth 1 -name '.tmp.*' -mmin +240 -exec rm -rf {} + 2>/dev/null || true

if [[ ! -d $cache/$key/media ]]; then
  echo "fixtures: making $key" >&2
  # The films are made under TMPDIR, which the caller's trap removes, so a
  # killed run leaves its ffmpeg output there and not in the cache; only the
  # copy (a moment's work) is made in the cache, to be renamed whole.
  gen=$(mktemp -d)
  tmp=
  trap 'rm -rf "$gen" "$tmp"' EXIT
  bash "$here/e2e.sh" "$gen" >&2
  tmp=$(mktemp -d "$cache/.tmp.XXXXXX")
  cp -a --reflink=auto "$gen/." "$tmp"
  # Another run may have filled the entry meanwhile; its is as good.
  mv -T "$tmp" "$cache/$key" 2>/dev/null || [[ -d $cache/$key/media ]]
else
  echo "fixtures: cached $key" >&2
fi
touch "$cache/$key"
rm -rf "$work/media"
cp -a --reflink=auto "$cache/$key/media" "$work/media"
