#!/usr/bin/env bash
# Prints the key under which the e2e fixtures are cached: a sha256 over the
# recipes (test/fixtures/*.sh, the PGS, VobSub and Dolby Vision generators and the package the first imports),
# the first line of `ffmpeg -version` and the serif font make.sh attaches. CI's actions/cache key and test/fixtures/cached.sh both use
# it, so they can't diverge. Editing a recipe, or another ffmpeg, changes it.
set -euo pipefail
cd "$(dirname "$0")"
{
  sha256sum *.sh pgs/*.go vobsub/*.go dovi/*.go ../../internal/testenv/*.go
  sha256sum "$(fc-match -f '%{file}' serif)"
  ffmpeg -version 2>&1 | sed -n 1p
} | sha256sum | cut -d' ' -f1
