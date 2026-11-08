#!/usr/bin/env bash
# Checks what acceptance.sh promises about where it runs, by running it:
#   1. from a copy of the tree without .git (what git archive gives), and
#   2. while another test/differential/run.sh (another worktree, a reviewer) has its containers
#      up on the same host: acceptance.sh must still pass.
# Takes about as long as acceptance.sh. Usage: acceptance_isolation.sh <workdir>.
# Needs Docker; a missing dependency fails.
set -uo pipefail
repo=$(cd "$(dirname "$0")/../.." && pwd)
work=${1:?usage: acceptance_isolation.sh <workdir>}
mkdir -p "$work" && work=$(cd "$work" && pwd)
export_dir=$work/export
rm -rf "$export_dir" && mkdir -p "$export_dir" || exit 1
(cd "$repo" && git ls-files -co --exclude-standard -z | xargs -0 cp --parents -t "$export_dir") || exit 1
[[ -e $repo/web-dist ]] && ln -s "$repo/web-dist" "$export_dir/web-dist"
[[ -e $export_dir/.git ]] && { echo "FAIL  the export has a .git"; exit 1; }

bash "$export_dir/test/differential/acceptance.sh" "$work/acceptance" >"$work/acceptance.out" 2>&1 &
acceptance=$!
# Another run.sh (another worktree, a reviewer) starts its containers, named as run.sh names
# them after its own pid, after acceptance.sh has taken its snapshot of the containers, and
# they outlive acceptance.sh's checks. Stand-ins of the same images, so this does not depend on timing.
sleep 15
other=999999
docker run -d --name "jellyscale-differential-pg-$other" --entrypoint sleep postgres:18-alpine 900 >/dev/null || exit 1
docker run -d --name "jellyscale-differential-jf-$other" --entrypoint sleep jellyfin/jellyfin:12.4 900 >/dev/null || exit 1
wait "$acceptance"
status=$?
docker rm -fv "jellyscale-differential-pg-$other" "jellyscale-differential-jf-$other" >/dev/null
cat "$work/acceptance.out"
if [[ $status -eq 0 ]]; then echo "ok    acceptance.sh passes from an export without .git, beside a concurrent run.sh"; else echo "FAIL  acceptance.sh exited $status"; fi
rm -rf "$export_dir" "$work/acceptance"
exit $((status != 0))
