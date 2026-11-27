#!/usr/bin/env bash
# Tests the fixtures image (docs/client-testing.md) the way a client uses it, and the server image beside it.
#   test/image.sh          makes the films with test/fixtures/client.sh, builds both targets of the Dockerfile from this
#                          checkout and tests them. With JELLYSCALE_BUILT_AS=<name> the two images are named <name>:server
#                          and <name>:fixtures and are kept, for test/publish.sh to push those very images; otherwise
#                          they are removed at exit.
#   test/image.sh <image>  pulls that fixtures image and tests it; builds nothing and makes no film.
# What is expected of the films is test/image-expect.tsv, which docs/client-testing.md repeats. Needs docker with the
# compose plugin and a daemon, curl and jq; building also needs ffmpeg, Go, git and a serif font.
# Every check that fails is printed; the exit status is non-zero if any did. A tool that is missing fails the run.
# The run's containers and network belong to the compose project "jsimg-<pid>-<n>", printed first. A run that was
# killed leaves them behind, and the next run removes those of a project whose pid is gone, and no one else's.
# Temp files go under JELLYSCALE_TEST_ROOT (test/lib.sh), in a dir named image.<pid>.XXXXXX.
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
expect=$repo/test/image-expect.tsv
request=$repo/test/fixtures/client-playbackinfo-request.json
pulled=${1:-}

need() { command -v "$1" >/dev/null || { echo "test/image.sh needs $1 on PATH: $2" >&2; exit 1; }; }
need docker "it builds, pulls and starts the images"
docker compose version >/dev/null 2>&1 || { echo "test/image.sh needs the docker compose plugin: docker compose version fails" >&2; exit 1; }
docker info >/dev/null 2>&1 || { echo "test/image.sh needs a docker daemon that answers: docker info fails" >&2; exit 1; }
need curl "it asks the server"
need jq "it reads the server's answers"
if [[ -z $pulled ]]; then
  need ffmpeg "it makes the films (test/fixtures/client.sh needs ffmpeg, Go and a serif font)"
  need go "it makes the films (test/fixtures/client.sh needs ffmpeg, Go and a serif font)"
  need git "it labels the build with the commit"
fi

# --- cleanup: what this run made, and what a killed run left.
project=jsimg-$$-$RANDOM
run_label=jellyscale.image-run
built_repo=jellyscale-image-test
fail_count=0
keep_images=${JELLYSCALE_BUILT_AS:-}
server_img= fixtures_img= compose_started=

remove_project() { # name [compose]: its containers (compose's and docker run's), network, anonymous volumes and built images; compose runs down only for a project that was started
  [[ ${2:-} == compose ]] && docker compose -p "$1" down -v --remove-orphans -t 2 >/dev/null 2>&1
  docker ps -aq --filter "label=$run_label=$1" | xargs -r docker rm -fv >/dev/null 2>&1
  docker ps -aq --filter "label=com.docker.compose.project=$1" | xargs -r docker rm -fv >/dev/null 2>&1
  docker network ls -q --filter "label=com.docker.compose.project=$1" | xargs -r docker network rm >/dev/null 2>&1
  docker image rm "$built_repo:$1-server" "$built_repo:$1-fixtures" >/dev/null 2>&1
  return 0
}
sweep() { # the projects of runs that are gone
  local p
  for p in $({ docker ps -a --format '{{.Label "com.docker.compose.project"}}'; docker ps -a --format "{{.Label \"$run_label\"}}"
    docker network ls --format '{{.Label "com.docker.compose.project"}}'; docker image ls "$built_repo" --format '{{.Tag}}' | sed 's/-\(server\|fixtures\)$//'; } 2>/dev/null | sort -u); do
    [[ $p =~ ^jsimg-([0-9]+)- ]] || continue
    kill -0 "${BASH_REMATCH[1]}" 2>/dev/null && continue
    remove_project "$p" compose
  done
}
cleanup() {
  trap '' INT TERM
  remove_project "$project" ${compose_started:+compose}
  rm -rf "$tmp_base"
}
make_work image || exit 1
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
echo "compose project: $project"
sweep

fail() { echo "FAIL: $*" >&2; fail_count=$((fail_count + 1)); }
die() { echo "FAIL: $*" >&2; exit 1; }
tsv() { awk -F'\t' -v k="$1" '$1 == k {print $2}' "$expect"; }
copy_of() { awk -F'\t' -v f="$1" '$1 == "twin" && $2 == f {print $3}' "$expect"; } # the S3 copy of a film, if it has one
run_in() { # image command...: a throwaway container of the image, with the entrypoint replaced
  local img=$1; shift
  docker run --rm --label "$run_label=$project" --entrypoint "$1" "$img" "${@:2}"
}

# --- the images
if [[ -n $pulled ]]; then
  echo "pulling $pulled"
  docker pull "$pulled" || die "docker pull $pulled failed: the image cannot be fetched"
  fixtures_img=$pulled
else
  rev=$(git -C "$repo" rev-parse HEAD) || die "git rev-parse HEAD failed"
  if [[ ${GITHUB_REF_TYPE:-} == tag && ${GITHUB_REF_NAME:-} =~ ^v([0-9]+\.[0-9]+\.[0-9]+)$ ]]; then version=${BASH_REMATCH[1]}; else version=main-${rev:0:12}; fi
  # The build context is a copy of what the Dockerfile reads, with the films beside it as media/.
  ctx=$work/context
  mkdir -p "$ctx/deploy"
  (cd "$repo" && cp -a Dockerfile go.mod go.sum cmd internal "$ctx/" && cp -a deploy/fixtures "$ctx/deploy/") || die "copying the build context failed"
  echo "making the films"
  bash "$repo/test/fixtures/client.sh" "$ctx/media" || die "test/fixtures/client.sh failed"
  if [[ -n $keep_images ]]; then server_img=$keep_images:server fixtures_img=$keep_images:fixtures
  else server_img=$built_repo:$project-server fixtures_img=$built_repo:$project-fixtures; fi
  for target in server fixtures; do
    img=$server_img; [[ $target == fixtures ]] && img=$fixtures_img
    docker build --target "$target" -t "$img" --build-arg "REVISION=$rev" --build-arg "VERSION=$version" "$ctx" || die "docker build of the $target target failed"
  done
fi

inspect() { docker image inspect -f "$2" "$1"; }
images=("$fixtures_img")
[[ -n $server_img ]] && images+=("$server_img")

# Labels: all three on each image; in a build, the same on both, and the revision is this checkout's.
label() { inspect "$1" "{{index .Config.Labels \"org.opencontainers.image.$2\"}}"; }
for img in "${images[@]}"; do
  [[ $(label "$img" revision) =~ ^[0-9a-f]{40}$ ]] || fail "$img: org.opencontainers.image.revision is '$(label "$img" revision)', want a 40-digit commit"
  [[ $(label "$img" source) == https://github.com/jellyscale-org/jellyscale ]] || fail "$img: org.opencontainers.image.source is '$(label "$img" source)'"
  [[ -n $(label "$img" version) ]] || fail "$img: org.opencontainers.image.version is missing or empty"
done
if [[ -n $server_img ]]; then
  for key in revision source version; do
    [[ $(label "$server_img" $key) == "$(label "$fixtures_img" $key)" ]] || fail "the two images differ in org.opencontainers.image.$key: '$(label "$server_img" $key)' and '$(label "$fixtures_img" $key)'"
  done
  [[ $(label "$fixtures_img" revision) == "$rev" ]] || fail "the revision label is '$(label "$fixtures_img" revision)', not HEAD $rev"
  [[ $(label "$fixtures_img" version) == "$version" ]] || fail "the version label is '$(label "$fixtures_img" version)', want $version"
fi

# User, entrypoint, command, and no password anywhere an image keeps one.
for img in "${images[@]}"; do
  [[ $(inspect "$img" '{{.Config.User}}') == 1000:1000 ]] || fail "$img: user is '$(inspect "$img" '{{.Config.User}}')', want 1000:1000"
  [[ $(inspect "$img" '{{json .Config.Entrypoint}}') == '["jellyscale"]' ]] || fail "$img: entrypoint is $(inspect "$img" '{{json .Config.Entrypoint}}'), want [\"jellyscale\"]"
  if inspect "$img" '{{json .Config}} {{json .Config.Env}}' | grep -qi password; then fail "$img: its config, environment or labels name a password"; fi
  if docker history --no-trunc --format '{{.CreatedBy}}' "$img" | grep -qi password; then fail "$img: its build history names a password"; fi
  if [[ -n $(run_in "$img" find / -xdev \( -iname '*jellyfin-web*' -o -name web-dist \) 2>/dev/null) ]]; then fail "$img: holds jellyfin-web"; fi
done
[[ $(inspect "$fixtures_img" '{{json .Config.Cmd}}') == '["--roles=all","--config=/fixtures/config.json"]' ]] || fail "the fixtures image's command is $(inspect "$fixtures_img" '{{json .Config.Cmd}}')"
config=$(run_in "$fixtures_img" cat /fixtures/config.json) || fail "the fixtures image has no /fixtures/config.json"
jq -e '.adminUser == "test" and (has("adminPassword") | not) and (has("webDir") | not)' <<<"$config" >/dev/null || fail "the fixtures config has no adminUser test, or has an adminPassword or a webDir"
if [[ -n $server_img ]]; then
  [[ $(inspect "$server_img" '{{json .Config.Cmd}}') == null ]] || fail "the server image has a default command: $(inspect "$server_img" '{{json .Config.Cmd}}')"
  run_in "$server_img" ls /fixtures >/dev/null 2>&1 && fail "the server image holds /fixtures"
  run_in "$server_img" ls /etc/jellyscale >/dev/null 2>&1 && fail "the server image holds a config"
  # The fixtures image is the server image plus layers: the same binary, ffmpeg and user.
  [[ $(inspect "$fixtures_img" '{{json .RootFS.Layers}}') == "$(inspect "$server_img" '{{json .RootFS.Layers}}' | sed 's/\]$//')"* ]] || fail "the fixtures image's layers do not begin with the server image's"
fi

# The films in the image are the films client.sh made: same paths, same sizes, under the size limit.
in_image=$(run_in "$fixtures_img" sh -c 'cd /fixtures/media && find . -type f -exec stat -c "%s %n" {} +' | sed 's|^\([0-9]*\) \./|\1 |' | LC_ALL=C sort)
if [[ -n $server_img ]]; then
  made=$(cd "$ctx/media" && find . -type f -printf '%s %P\n' | LC_ALL=C sort)
  [[ $in_image == "$made" ]] || fail "/fixtures/media differs from the films client.sh made: $(diff <(echo "$made") <(echo "$in_image") | head -5)"
fi
size=$(awk '{s += $1} END {print s + 0}' <<<"$in_image")
[[ $size -gt 0 && $size -lt 300000000 ]] || fail "/fixtures/media is $size bytes, want more than 0 and under 300,000,000"
[[ $(grep -cE '\.(mkv|mp4)$' <<<"$in_image") -eq $(($(tsv films) + $(tsv episodes))) ]] || fail "/fixtures/media holds $(grep -cE '\.(mkv|mp4)$' <<<"$in_image") videos, want $(($(tsv films) + $(tsv episodes)))"

# --- start it as a client would: the compose file alone, JELLYSCALE_IMAGE, the port read from compose.
mkdir -p "$work/compose"
cp "$repo/$(tsv compose)" "$work/compose/compose.yaml" || die "no compose file $(tsv compose)"
compose() { compose_started=1; JELLYSCALE_IMAGE=$fixtures_img docker compose -p "$project" -f "$work/compose/compose.yaml" "$@"; }
compose up -d --wait --wait-timeout 240 || { compose logs --tail 40 >&2; die "docker compose up -d --wait did not end with a healthy server"; }
hostport=$(compose port jellyscale 8096) || die "docker compose port jellyscale 8096 failed"
base=http://$hostport
[[ $hostport == 127.0.0.1:* ]] || fail "the server's port is published on $hostport, want 127.0.0.1"
[[ $(curl -s -m 10 -o /dev/null -w '%{http_code}' "$base/readyz") == 200 ]] || fail "the first GET /readyz after up --wait did not answer 200"
[[ $(curl -s -m 10 -o /dev/null -w '%{http_code}' "$base/web/") == 404 ]] || fail "GET /web/ did not answer 404"

user=$(tsv user)
password=$(sed -n 's/.*JELLYSCALE_ADMIN_PASSWORD: *//p' "$work/compose/compose.yaml")
client='MediaBrowser Client="image.sh", Device="image.sh", DeviceId="image-sh", Version="1"'
signin=$(jq -n --arg u "$user" --arg p "$password" '{Username: $u, Pw: $p}' | curl -s -m 30 -H "X-Emby-Authorization: $client" -H 'Content-Type: application/json' -d @- "$base/Users/AuthenticateByName")
token=$(jq -r '.AccessToken // empty' <<<"$signin")
userid=$(jq -r '.User.Id // empty' <<<"$signin")
[[ -n $token && -n $userid ]] || die "signing in as $user failed: $signin"
[[ $(jq -r '.User.Policy.IsAdministrator' <<<"$signin") == true ]] || fail "$user is not an administrator"
auth="Authorization: $client, Token=\"$token\""
api() { curl -s -m 30 -H "$auth" "$@"; }
items() { api "$base/Items?Recursive=true&IncludeItemTypes=$1&Limit=500" | jq -r '.Items[] | [.Name, .Id] | @tsv'; }

# --- the scan: /readyz answers before it is done, so wait, up to 60 seconds, for the documented counts.
want_films=$(( $(tsv films) + $(awk -F'\t' '$1 == "twin"' "$expect" | grep -c .) )) want_episodes=$(tsv episodes)
deadline=$((SECONDS + 60))
while :; do
  movies=$(items Movie) episodes=$(items Episode)
  n_films=$(grep -c . <<<"$movies") n_episodes=$(grep -c . <<<"$episodes")
  [[ $n_films -eq $want_films && $n_episodes -eq $want_episodes ]] && break
  if ((SECONDS >= deadline)); then die "60 seconds after up --wait: $n_films films and $n_episodes episodes, want $want_films and $want_episodes"; fi
  sleep 1
done
named() { awk -F'\t' -v n="$2" '$1 == n {c++} END {print c + 0}' <<<"$1"; }
while IFS= read -r name; do
  [[ $(named "$movies" "$name") -eq 1 ]] || fail "film '$name' finds $(named "$movies" "$name") items, want exactly one"
done < <(tsv film)
while IFS= read -r name; do
  [[ $(named "$movies" "$name") -eq 1 ]] || fail "S3 copy '$name' finds $(named "$movies" "$name") items, want exactly one"
done < <(awk -F'\t' '$1 == "twin" {print $3}' "$expect")
while IFS= read -r name; do
  [[ $(named "$episodes" "$name") -eq 1 ]] || fail "episode '$name' finds $(named "$episodes" "$name") items, want exactly one"
done < <(tsv episode)
views=$(api "$base/UserViews?userId=$userid" | jq -r '.Items[] | "\(.Name)\t\(.CollectionType)"' | LC_ALL=C sort)
[[ $views == "$(awk -F'\t' '$1 == "library" || $1 == "s3library" {print $2 "\t" $3}' "$expect" | LC_ALL=C sort)" ]] || fail "the libraries are $(tr '\n\t' ';,' <<<"$views"), not the documented ones"

# --- PlaybackInfo, for the request of test/fixtures/client-playbackinfo-request.json, of every film.
jq -e .DeviceProfile "$request" >/dev/null || die "$request has no DeviceProfile"
playback() { api -H 'Content-Type: application/json' -d @"${2:-$request}" "$base/Items/$1/PlaybackInfo?UserId=$userid&StartTimeTicks=0&IsPlayback=false&AutoOpenLiveStream=false"; }
declare -A film_id source_id transcode_url
# Each film, and each S3 copy of one, which must list the film's tracks and play as the film does.
while IFS=$'\t' read -r name orig; do
  id=$(awk -F'\t' -v n="$name" '$1 == n {print $2}' <<<"$movies")
  [[ -n $id ]] || continue
  orig=${orig:-$name}
  info=$(playback "$id")
  film_id[$name]=$id
  source_id[$name]=$(jq -r '.MediaSources[0].Id' <<<"$info")
  transcode_url[$name]=$(jq -r '.MediaSources[0].TranscodingUrl // empty' <<<"$info")
  got=$(jq -r '.MediaSources[0].MediaStreams | to_entries[] | .value as $s | [$s.Index, $s.Type, (if ($s.Language // "") == "" then "none" else $s.Language end), $s.Codec,
      ([(if $s.Type == "Audio" and $s.IsDefault then "default" else empty end), (if $s.IsForced then "forced" else empty end), (if $s.IsExternal then "external" else empty end)] | if length == 0 then "-" else join(",") end)] | @tsv' <<<"$info")
  want=$(awk -F'\t' -v f="$orig" '$1 == "track" && $2 == f {print $3 "\t" $4 "\t" $5 "\t" $6 "\t" $7}' "$expect")
  [[ $got == "$want" ]] || fail "$name: PlaybackInfo lists $(grep -c . <<<"$got") streams that differ from the documented $(grep -c . <<<"$want"): $(diff <(echo "$want") <(echo "$got") | head -6 | tr '\n' ';')"
  plays=$(awk -F'\t' -v f="$orig" '$1 == "plays" && $2 == f {print $3}' "$expect")
  if [[ $plays == transcode && -z ${transcode_url[$name]} ]]; then fail "$name: documented to transcode, but PlaybackInfo has no TranscodingUrl"; fi
  if [[ $plays == direct && -n ${transcode_url[$name]} ]]; then fail "$name: documented to play as it is, but PlaybackInfo has a TranscodingUrl"; fi
  video=$(awk -F'\t' -v f="$orig" '$1 == "plays" && $2 == f {print $4}' "$expect")
  if [[ $video == copy && ${transcode_url[$name]} != *VideoCodec=copy* ]]; then fail "$name: documented to copy the video, but its TranscodingUrl does not say VideoCodec=copy"; fi
done < <({ tsv film | sed 's/$/\t/'; awk -F'\t' '$1 == "twin" {print $3 "\t" $2}' "$expect"; })

# Direct play: a byte range of a film that plays as it is, of the film and of its S3 copy.
byte_range() { # name
  local name=$1 code
  code=$(curl -s -m 60 -D "$work/range.headers" -o "$work/range.body" -w '%{http_code}' -H "$auth" -H 'Range: bytes=1000-1999' \
    "$base/Videos/${film_id[$name]}/stream?static=true&mediaSourceId=${source_id[$name]}&api_key=$token")
  grep -qi '^content-range: bytes 1000-1999/' "$work/range.headers" && [[ $code == 206 && $(stat -c %s "$work/range.body") -eq 1000 ]] \
    || fail "$name: Range bytes=1000-1999 gave $code, $(grep -i '^content-range' "$work/range.headers" | tr -d '\r'), $(stat -c %s "$work/range.body") bytes; want 206, Content-Range bytes 1000-1999/N and 1000 bytes"
}
direct=$(awk -F'\t' '$1 == "plays" && $3 == "direct" {print $2; exit}' "$expect")
byte_range "$direct"
[[ -z $(copy_of "$direct") ]] || byte_range "$(copy_of "$direct")"

# Subtitles: Babel Film's track 5 as WebVTT, whose first cue is its own.
vtt=
for _ in $(seq 30); do
  vtt=$(api "$base/Videos/${film_id[Babel Film]}/${source_id[Babel Film]}/Subtitles/5/Stream.vtt")
  [[ $vtt == WEBVTT* ]] && break
  sleep 2
done
[[ $vtt == WEBVTT* ]] || fail "Babel Film track 5 as Stream.vtt does not begin WEBVTT: ${vtt:0:80}"
cue=$(awk '/-->/ {f = 1; next} f && NF == 0 {exit} f {print}' <<<"$vtt")
[[ $cue == "Babel track 5" ]] || fail "Babel Film track 5's first cue is '$cue', want 'Babel track 5'"

resolve() { # base ref: ref as a URL, relative to base
  case $2 in
  http*) echo "$2" ;;
  /*) echo "http://$hostport$2" ;;
  *) local b=${1%%\?*}; echo "${b%/*}/$2" ;;
  esac
}
fetch() { # url file: with up to 3 minutes for the worker to make it
  local code until=$((SECONDS + 180))
  while :; do
    code=$(curl -s -m 60 -H "$auth" -o "$2" -w '%{http_code}' "$1")
    [[ $code == 200 ]] && return 0
    ((SECONDS >= until)) && { echo "GET $1 answered $code" >&2; return 1; }
    sleep 1
  done
}
# A film's first segment, for PlaybackInfo with the tracks a segment row names (- for the defaults). TranscodingUrl names
# them, and the segment, with its init part, holds the codecs the row gives and, where it gives colours, a picture whose
# primaries, transfer and matrix are all that colour.
segment_check() { # name audio subtitle codecs colours
  local name=$1 audio=$2 subtitle=$3 codecs=$4 colours=$5 body info url master variant init got
  body=$request
  if [[ $audio != - ]]; then
    body=$work/choice.json
    jq --argjson a "$audio" --argjson s "$subtitle" '. + {AudioStreamIndex: $a, SubtitleStreamIndex: $s}' "$request" >"$body"
  fi
  info=$(playback "${film_id[$name]}" "$body")
  url=$(jq -r '.MediaSources[0].TranscodingUrl // empty' <<<"$info")
  if [[ -z $url ]]; then fail "$name: PlaybackInfo has no TranscodingUrl, so there is no segment to look at"; return; fi
  if [[ $audio != - && ($url != *AudioStreamIndex=$audio* || $url != *SubtitleStreamIndex=$subtitle*) ]]; then fail "$name: TranscodingUrl does not name AudioStreamIndex=$audio and SubtitleStreamIndex=$subtitle"; fi
  master=$(resolve "$base/" "$url")
  fetch "$master" "$work/master.m3u8" || { fail "$name: the master playlist did not answer 200"; return; }
  variant=$(resolve "$master" "$(grep -v '^#' "$work/master.m3u8" | head -1)")
  fetch "$variant" "$work/variant.m3u8" || { fail "$name: the variant playlist did not answer 200"; return; }
  init=$(sed -n 's/^#EXT-X-MAP:URI="\([^"]*\)".*/\1/p' "$work/variant.m3u8" | head -1)
  segment=$(grep -v '^#' "$work/variant.m3u8" | head -1)
  : >"$work/segment.mp4"
  if [[ -n $init ]]; then
    fetch "$(resolve "$variant" "$init")" "$work/init.mp4" && cat "$work/init.mp4" >>"$work/segment.mp4" || { fail "$name: the init part did not answer 200"; return; }
  fi
  fetch "$(resolve "$variant" "$segment")" "$work/seg.m4s" && cat "$work/seg.m4s" >>"$work/segment.mp4" || { fail "$name: the first segment did not answer 200"; return; }
  got=$(docker run -i --rm --label "$run_label=$project" --entrypoint ffprobe "$fixtures_img" -v error -show_entries stream=codec_name -of csv=p=0 -i pipe:0 <"$work/segment.mp4" | sort | tr '\n' ' ')
  [[ $got == "$codecs " ]] || fail "$name's first segment holds '$got', want $codecs and no other codec"
  if [[ $colours != - ]]; then
    got=$(docker run -i --rm --label "$run_label=$project" --entrypoint ffprobe "$fixtures_img" -v error -select_streams v:0 -show_entries stream=color_primaries,color_transfer,color_space -of csv=p=0 -i pipe:0 <"$work/segment.mp4" | tr -d '\n')
    [[ $got == "$colours,$colours,$colours" ]] || fail "$name's first segment has the colours '$got', want $colours"
  fi
}
# Transcodes: each segment row checks its film's first segment and its S3 copy's.
while IFS=$'\t' read -r name audio subtitle codecs colours; do
  segment_check "$name" "$audio" "$subtitle" "$codecs" "$colours"
  [[ -z $(copy_of "$name") ]] || segment_check "$(copy_of "$name")" "$audio" "$subtitle" "$codecs" "$colours"
done < <(awk -F'\t' '$1 == "segment" {print $2 "\t" $3 "\t" $4 "\t" $5 "\t" $6}' "$expect")

# --- without the password the image refuses to start, creates no user and never answers /readyz with 200.
compose exec -T postgres createdb -U postgres nopassword >/dev/null || fail "could not make a database for the no-password start"
cid=$(docker run -d --label "$run_label=$project" --network "${project}_default" -p 127.0.0.1::8096 \
  -e JELLYSCALE_DATABASE_URL=postgres://postgres:dev@postgres:5432/nopassword "$fixtures_img")
answered=0
for _ in $(seq 300); do
  [[ $(docker inspect -f '{{.State.Running}}' "$cid") == true ]] || break
  nport=$(docker port "$cid" 8096/tcp 2>/dev/null | head -1)
  [[ -n $nport && $(curl -s -m 2 -o /dev/null -w '%{http_code}' "http://$nport/readyz") == 200 ]] && answered=1
  sleep 0.2
done
exit_code=$(timeout 60 docker wait "$cid")
nopw_log=$(docker logs "$cid" 2>&1)
[[ $exit_code != 0 && -n $exit_code ]] || fail "started without JELLYSCALE_ADMIN_PASSWORD, the container did not exit non-zero (exit '$exit_code')"
grep -q JELLYSCALE_ADMIN_PASSWORD <<<"$nopw_log" || fail "started without JELLYSCALE_ADMIN_PASSWORD, its log does not name it: ${nopw_log:0:200}"
[[ $answered -eq 0 ]] || fail "started without JELLYSCALE_ADMIN_PASSWORD, /readyz answered 200"
if [[ -n $(compose exec -T postgres psql -U postgres -d nopassword -tAc "select to_regclass('public.users')") ]]; then
  [[ $(compose exec -T postgres psql -U postgres -d nopassword -tAc 'select count(*) from users') == 0 ]] || fail "started without JELLYSCALE_ADMIN_PASSWORD, a user was created"
fi
docker rm -fv "$cid" >/dev/null 2>&1

if [[ $fail_count -gt 0 ]]; then echo "test/image.sh: $fail_count checks failed" >&2; exit 1; fi
echo "test/image.sh: passed"
