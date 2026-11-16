#!/usr/bin/env bash
# Makes the library of the fixtures image (docs/client-testing.md) under <dir>: make.sh's 21 films and 3 episodes, and
# Babel Film and Kino Film. Needs ffmpeg, Go (the PGS recipe is a Go program) and a serif font (make.sh's font films).
# Exits non-zero, saying what failed, unless every film is there. Usage: client.sh <dir>
set -uo pipefail
dir=${1:?usage: client.sh <dir>}
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
for tool in ffmpeg go fc-match; do
  command -v "$tool" >/dev/null || { echo "client.sh needs ffmpeg, Go and a serif font (fontconfig's fc-match): $tool is not on PATH" >&2; exit 1; }
done
for recipe in make babel kino; do
  bash "$here/$recipe.sh" "$dir" || { echo "client.sh: $recipe.sh failed, so films are missing" >&2; exit 1; }
done
# Whatever a recipe's ffmpeg said, a film that is not there, or has nothing in it, fails the run.
films=0
while IFS= read -r -d '' film; do
  [[ -s $film ]] || { echo "client.sh: film missing or empty: $film" >&2; exit 1; }
  films=$((films + 1))
done < <(find "$dir" -type f \( -name '*.mkv' -o -name '*.mp4' \) -print0)
[[ $films -eq 26 ]] || { echo "client.sh: $films films and episodes under $dir, want 26 (23 films, 3 episodes): a film is missing" >&2; exit 1; }
