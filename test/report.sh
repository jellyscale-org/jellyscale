#!/usr/bin/env bash
# Turns test results into something readable without opening a log: counts and
# the first lines of each failure on the job summary ($GITHUB_STEP_SUMMARY), and
# an annotation with file and line for each failure, which `gh run view` shows.
# Outside CI ($GITHUB_ACTIONS unset) it prints plain text, with no summary or annotations.
#
#   report.sh go <gotestsum.json> <title> [plain|integration|integration:<shard>]
#       Runs cmd/testreport on `go test -json` output. Any skipped test is a
#       failure, and so is a count different from what `go test -list` finds
#       for the tier named (a run that stopped partway), or from what
#       test/shard.sh counts for the shard: neither is allowed to leave CI green. The full output of
#       each failure goes to failures.log beside the JSON.
#   report.sh timings <gotestsum.json>
#       Prints the lines tests logged that hold a time or a multiple (the speedup
#       of more workers, the copy's multiple of real time), whether the test
#       passed or not. `go` runs it, so the job log of a run that passed keeps them:
#       CI uploads tests.json only for a failed job. Tests log timings, never judge them.
#   report.sh fail <file> <line> <title> <message>
#       One failure, from a bash check.
#   report.sh lines <file> <title>
#       Reads a gate's output ("ok  name" and "FAIL  name: why" lines) on stdin,
#       passes it through, and reports each FAIL at the line of <file> that names it.
#   report.sh totals <title> <passed> <failed>
set -uo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)

summary() { [[ -n ${GITHUB_STEP_SUMMARY:-} ]] && cat >>"$GITHUB_STEP_SUMMARY"; }
escape() { # Annotation values are single-line; commas and colons end a property.
  local s=$1
  s=${s//'%'/%25}; s=${s//$'\r'/%0D}; s=${s//$'\n'/%0A}
  [[ ${2:-} == property ]] && { s=${s//:/%3A}; s=${s//,/%2C}; }
  printf '%s' "$s"
}
annotate() { # file line title message
  [[ -n ${GITHUB_ACTIONS:-} ]] || return 0
  local where=""
  [[ -n $1 ]] && where="file=$(escape "$1" property),line=${2:-1},"
  echo "::error ${where}title=$(escape "$3" property)::$(escape "$4")"
}

case ${1:-} in
timings)
  python3 -I -c '
import json, re, sys
for line in open(sys.argv[1]):
    try:
        event = json.loads(line)
    except ValueError:
        continue
    text = event.get("Output", "").strip()
    if event.get("Action") == "output" and event.get("Test") and re.search(r"_test\.go:\d+: .*\d(?:ms|µs|s|x)\b", text):
        print("timing  %s: %s" % (event["Test"], text.split(": ", 1)[1]))
' "$2"
  ;;
fail)
  _file=$2 _line=$3 _title=$4 _message=$5
  annotate "$_file" "$_line" "$_title" "$_message"
  [[ -n ${GITHUB_ACTIONS:-} ]] && printf -- '- **%s** `%s:%s` %s\n' "$_title" "$_file" "$_line" "$_message" | summary
  ;;
totals)
  echo "$2: $3 passed, $4 failed"
  [[ -n ${GITHUB_ACTIONS:-} ]] && printf '\n**%s**: %s passed, %s failed\n\n' "$2" "$3" "$4" | summary
  ;;
lines)
  _file=$repo/$2 _title=$3 _passed=0 _failed=0
  while IFS= read -r line; do
    printf '%s\n' "$line"
    case $line in
    "ok  "*) _passed=$((_passed + 1)) ;;
    "FAIL  "*)
      _failed=$((_failed + 1))
      # The reason follows the name after ": ", and names can hold ": " too, so
      # the longest part of the line that is a quoted literal is the name. A
      # scenario keeps what is known only at run time after ": ", so it is still
      # found; a name with no quoted literal gets line 1.
      _name=${line#FAIL  } _name=${_name% (*s)} _at=
      while [[ -z $_at ]]; do
        _at=$(grep -nF -m1 -- "\"$_name\"" "$_file" | cut -d: -f1)
        [[ $_name == *": "* ]] || break
        _name=${_name%: *}
      done
      bash "$repo/test/report.sh" fail "$2" "${_at:-1}" "$_title" "$line"
      # One greppable line per failure, for collecting flakes across runs with gh.
      _ledger=${line#FAIL  }
      printf 'failed: %s.%s\n\n' "$_title" "${_ledger% (*s)}" | summary
      ;;
    esac
  done
  bash "$repo/test/report.sh" totals "$_title" "$_passed" "$_failed" >/dev/null
  [[ -n ${GITHUB_ACTIONS:-} ]] || echo "$_title: $_passed passed, $_failed failed"
  ;;
go)
  json=$2 title=$3 tier=${4:-} want=-1
  cd "$repo" || exit 1
  if [[ $tier == integration:* ]]; then
    want=$(bash test/shard.sh count "${tier#integration:}") || exit 1
  elif [[ -n $tier ]]; then
    tags=(); [[ $tier == integration ]] && tags=(-tags integration)
    # The compiler says which tests the tier has, so no regex has to.
    want=$(go test "${tags[@]}" -list '.*' ./... 2>/dev/null | grep -cE '^(Test|Example|Fuzz)')
  fi
  go run ./cmd/testreport -json "$json" -title "$title" -want "$want" -log "$(dirname "$json")/failures.log"
  _status=$?
  # After the summary line, which callers read first. The timings stay in the job's output.
  echo "logged timings of the tests (kept, not judged):"
  bash "$repo/test/report.sh" timings "$json"
  exit $_status
  ;;
*)
  sed -n '2,/^set /p' "$0" | sed '$d' | sed 's/^# \{0,1\}//' >&2
  exit 2
  ;;
esac
