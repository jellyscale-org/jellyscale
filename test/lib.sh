# Helpers the test scripts share. Source it; it sets $repo.
repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

# Work dirs that scripts make for themselves live under one root, named
# <name>.<pid>.XXXXXX. A run that was SIGKILLed leaves its dir behind, so every
# script removes, before it starts, the dirs whose script has exited.
test_root=${JELLYSCALE_TEST_ROOT:-/tmp/jellyscale-test}

# make_work <name> [dir]: sets $work (a new dir under the root, or <dir>),
# $tmp_base (the dir under the root) and $tmp_dir, which is TMPDIR. TMPDIR is set
# before go test and ffmpeg start, so that what they leave behind (the tests' own
# temp dirs and media, a browser's profile) goes when the script's trap removes
# $tmp_base and $work/t. With <dir>, the files are in <dir>/t, so that a killed
# run leaves them there and not in RAM-backed /tmp; TMPDIR is a short link to it
# under the root, because a unix socket path holds at most 108 bytes.
make_work() {
  mkdir -p "$test_root"
  local old pid
  for old in "$test_root"/*.*.*; do
    [[ -e $old || -L $old ]] || continue
    pid=${old##*/}; pid=${pid#*.}; pid=${pid%%.*}
    [[ $pid =~ ^[0-9]+$ ]] && ! kill -0 "$pid" 2>/dev/null && rm -rf "$old"
  done
  find "$repo/.results" -mindepth 1 -maxdepth 1 -mtime +14 -exec rm -rf {} + 2>/dev/null
  tmp_base=$(mktemp -d "$test_root/$1.$$.XXXXXX") || return 1
  tmp_dir=$tmp_base/t
  if [[ -n ${2:-} ]]; then
    mkdir -p "$2/t" && work=$(cd "$2" && pwd) || return 1
    ln -s "$work/t" "$tmp_dir"
  else
    work=$tmp_base
    mkdir -p "$tmp_dir"
  fi
  export TMPDIR=$tmp_dir
}

# Recorded results, for QA: <repo>/.results/<tree>/<name> says whether a full
# run of that tier or shard passed on that tree (tools/snapshot.sh), with the
# log's path. Only the scripts write them, and only for a whole selection: a
# script leaves result_name empty when it was given -run arguments or E2E_ONLY.
# .results is gitignored, so writing it doesn't change the tree.
result_tree= result_name= result_summary= result_log=
result_start() {
  result_tree=$(cd "$repo" && bash tools/snapshot.sh 2>/dev/null) || result_tree=
}
# result_write <name> <exit status> <summary> <log>: for a script with several
# results (the e2e gates). Nothing is written if the tree changed during the run.
result_write() {
  [[ -n $result_tree ]] || return 0
  # A tree that changed during the run was not the one tested.
  [[ $(cd "$repo" && bash tools/snapshot.sh 2>/dev/null) == "$result_tree" ]] || return 0
  local dir=$repo/.results/$result_tree
  mkdir -p "$dir"
  {
    echo "tier=$1"
    echo "status=$([[ $2 -eq 0 ]] && echo pass || echo fail)"
    echo "summary=$3"
    echo "log=$4"
    echo "tree=$result_tree"
    echo "finished=$(date -u +%FT%TZ)"
  } >"$dir/$1.tmp" && mv "$dir/$1.tmp" "$dir/$1"
}
# result_finish <exit status>, from the script's EXIT trap, for the one result in $result_name.
result_finish() {
  [[ -n $result_name ]] || return 0
  result_write "$result_name" "$1" "$result_summary" "$result_log"
}
# result_logdir <name>: where a Go tier keeps its logs when JELLYSCALE_LOG_DIR is unset.
result_logdir() {
  if [[ -n $result_tree ]]; then echo "$repo/.results/$result_tree/$1.d"; else echo "$work/logs"; fi
}
