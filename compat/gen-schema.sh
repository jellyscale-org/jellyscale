#!/usr/bin/env bash
# Makes compat/schema-12.4.json from the OpenAPI document of the Jellyfin container
# the differential tier starts (jellyfin/jellyfin:12.4), and records the image and
# the server version in it. The differential tier makes the same file from the
# container it has started and fails when it differs from the checked-in one.
# Usage: compat/gen-schema.sh     (needs Docker; the container is removed again)
set -uo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
image=jellyfin/jellyfin:12.4
name=jellyscale-genschema-$$
trap 'docker rm -fv "$name" >/dev/null 2>&1; rm -f "$doc"' EXIT
doc=$(mktemp)
docker run -d --rm --name "$name" -p 127.0.0.1::8096 "$image" >/dev/null || exit 1
port=$(docker port "$name" 8096/tcp | head -1 | sed 's/.*://')
# The server answers before its document is ready, so wait for the document itself.
for _ in $(seq 1 120); do curl -fs "http://127.0.0.1:$port/api-docs/openapi.json" -o "$doc" && break; sleep 1; done
[[ -s $doc ]] || { echo "FAIL  $image gave no OpenAPI document" >&2; exit 1; }
(cd "$repo" && go run ./compat/conform/cmd/conform gen -image "$image" <"$doc" >"$repo/compat/schema-12.4.json.new") || exit 1
mv "$repo/compat/schema-12.4.json.new" "$repo/compat/schema-12.4.json"
echo "wrote compat/schema-12.4.json from $image"
