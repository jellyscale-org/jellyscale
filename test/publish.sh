#!/usr/bin/env bash
# Pushes the server image and the fixtures image test/image.sh tested, and nothing else.
# Usage: publish.sh <repository> <server image> <fixtures image>, with GITHUB_SHA, GITHUB_REF_TYPE and GITHUB_REF_NAME as
# GitHub sets them. A push to main gives main-<12 hex> and main-<12 hex>-fixtures; a tag vX.Y.Z (exactly three numbers) gives
# those two as well as X.Y.Z and X.Y.Z-fixtures. Any other ref publishes nothing.
#
# No published tag ever names another image, so before it pushes anything it asks the registry for every tag it would write:
# a version that exists fails the run naming it; a main tag that exists keeps its image (a rerun, or a tag on a commit
# that was published already). A registry that cannot be asked fails the run: only a 404 says a tag is absent.
# A bearer token is fetched with REGISTRY_USER and REGISTRY_TOKEN when the registry asks for one (ghcr.io does).
set -uo pipefail
[[ $# -eq 3 ]] || { echo "usage: publish.sh <repository> <server image> <fixtures image>" >&2; exit 2; }
repo=$1 server=$2 fixtures=$3
sha=${GITHUB_SHA:-} type=${GITHUB_REF_TYPE:-} ref=${GITHUB_REF_NAME:-}
[[ $sha =~ ^[0-9a-f]{40}$ ]] || { echo "publish.sh: GITHUB_SHA is '$sha', want 40 hex digits" >&2; exit 1; }
main=main-${sha:0:12}
versions=()
case $type in
branch) [[ $ref == main ]] || { echo "publish.sh: the branch '$ref' publishes nothing, only main does" >&2; exit 1; } ;;
tag)
  [[ $ref =~ ^v([0-9]+\.[0-9]+\.[0-9]+)$ ]] || { echo "publish.sh: the tag '$ref' is not vX.Y.Z and publishes nothing" >&2; exit 1; }
  versions=("${BASH_REMATCH[1]}" "${BASH_REMATCH[1]}-fixtures")
  ;;
*) echo "publish.sh: GITHUB_REF_TYPE is '$type', want branch or tag" >&2; exit 1 ;;
esac
mains=("$main" "$main-fixtures")

host=${repo%%/*} path=${repo#*/}
scheme=https
[[ $host == 127.* || $host == localhost* ]] && scheme=http
tmp=$(mktemp -d)
tagged=()
cleanup() { rm -rf "$tmp"; ((${#tagged[@]})) && docker rmi "${tagged[@]}" >/dev/null 2>&1; return 0; }
trap cleanup EXIT

curl_status() { # url [curl args]: the HTTP status, with the body in $tmp/body; 000 when nothing answered
  local url=$1; shift
  curl -sS --max-time 30 -o "$tmp/body" -w '%{http_code}' "$@" "$url" 2>"$tmp/curl.err" || echo 000
}
auth=()
status=$(curl_status "$scheme://$host/v2/" -D "$tmp/headers")
if [[ $status == 401 ]] && grep -qi '^www-authenticate: *bearer' "$tmp/headers" && [[ -n ${REGISTRY_USER:-} && -n ${REGISTRY_TOKEN:-} ]]; then
  challenge=$(grep -i '^www-authenticate' "$tmp/headers" | tr -d '\r')
  realm=$(sed -n 's/.*realm="\([^"]*\)".*/\1/p' <<<"$challenge")
  service=$(sed -n 's/.*service="\([^"]*\)".*/\1/p' <<<"$challenge")
  status=$(curl_status "$realm?service=$service&scope=repository:$path:pull,push" -u "$REGISTRY_USER:$REGISTRY_TOKEN")
  token=$(jq -r '.token // empty' "$tmp/body" 2>/dev/null)
  [[ $status == 200 && -n $token ]] || { echo "publish.sh: $host gave no token (HTTP $status), so it cannot be asked about tags: nothing was pushed" >&2; exit 1; }
  auth=(-H "Authorization: Bearer $token")
  status=$(curl_status "$scheme://$host/v2/" "${auth[@]}")
fi
[[ $status == 200 ]] || { echo "publish.sh: $scheme://$host/v2/ answered $status, so the registry cannot be asked about tags: nothing was pushed" >&2; exit 1; }

# exists <tag>: 0 when the registry has it, 1 when it says it has not (404); anything else ends the run.
exists() {
  local code
  code=$(curl_status "$scheme://$host/v2/$path/manifests/$1" "${auth[@]}" \
    -H 'Accept: application/vnd.docker.distribution.manifest.v2+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.oci.image.index.v1+json')
  case $code in
  200) return 0 ;;
  404) return 1 ;;
  *) echo "publish.sh: asking $host for the tag $1 gave $code, so the registry cannot be asked: nothing was pushed" >&2; exit 1 ;;
  esac
}

push=() # tag, then the local image
for tag in "${versions[@]}"; do
  if exists "$tag"; then echo "publish.sh: the version tag $tag exists already and never names another image: nothing was pushed (a mistake gets the next version)" >&2; exit 1; fi
done
for tag in "${versions[@]}" "${mains[@]}"; do
  if [[ $tag == *-fixtures ]]; then image=$fixtures; else image=$server; fi
  if exists "$tag"; then echo "publish.sh: $tag exists already and keeps its image"; else push+=("$tag" "$image"); fi
done
for ((i = 0; i < ${#push[@]}; i += 2)); do
  docker tag "${push[i + 1]}" "$repo:${push[i]}" && tagged+=("$repo:${push[i]}") || exit 1
done
for ((i = 0; i < ${#push[@]}; i += 2)); do
  docker push "$repo:${push[i]}" || { echo "publish.sh: pushing $repo:${push[i]} failed" >&2; exit 1; }
done
