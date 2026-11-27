#!/usr/bin/env bash
# tools/plan.sh — read-only listing of the plan under tasks/.
#
#   tools/plan.sh        print open tasks in tasks/order.txt order
#                        (position, id, status, phase, title; open tasks
#                        missing from the order listed last, marked
#                        "unordered"; order ids that are not open skipped),
#                        then the open backlog (id, severity, title)
#   tools/plan.sh <id>   print one record of any kind (task, backlog, decision)
#
# Exits non-zero, naming the file, when an order id has no file, when a
# file's name is not its `id`, or (for <id>) when no record has that id.
# Reads files only; writes nothing; no generated index.
set -euo pipefail

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
tasks_root="$root/tasks"
tasks_dir="$tasks_root/tasks"
backlog_dir="$tasks_root/backlog"
decisions_dir="$tasks_root/decisions"
order_file="$tasks_root/order.txt"

fail() {
  echo "plan.sh: $*" >&2
  exit 1
}

json_field() {
  local file=$1 field=$2
  jq -r --arg field "$field" '.[$field] // ""' "$file"
}

# A record's file name (without .json) must equal its `id`.
check_names() {
  local dir=$1 file base record_id
  for file in "$dir"/*.json; do
    [[ -e $file ]] || continue
    base=$(basename "$file" .json)
    record_id=$(jq -r '.id // ""' "$file") || fail "$file: not valid JSON"
    [[ $record_id == "$base" ]] || fail "$file: name '$base' is not id '$record_id'"
  done
}

# Every id in the order file must have a tasks/tasks/<id>.json file.
check_order_ids() {
  local record_id file
  while IFS= read -r record_id || [[ -n $record_id ]]; do
    record_id=${record_id#"${record_id%%[![:space:]]*}"}
    record_id=${record_id%"${record_id##*[![:space:]]}"}
    [[ -n $record_id ]] || continue
    file="$tasks_dir/$record_id.json"
    [[ -e $file ]] || fail "$file: order id '$record_id' has no file"
  done < "$order_file"
}

is_open() {
  local status=$1
  [[ $status != "done" && $status != "dropped" ]]
}

order_ids() {
  local record_id
  while IFS= read -r record_id || [[ -n $record_id ]]; do
    record_id=${record_id#"${record_id%%[![:space:]]*}"}
    record_id=${record_id%"${record_id##*[![:space:]]}"}
    [[ -n $record_id ]] || continue
    printf '%s\n' "$record_id"
  done < "$order_file"
}

print_task_row() {
  local position=$1 file=$2 marker=$3
  local record_id status phase title
  record_id=$(json_field "$file" id)
  status=$(json_field "$file" status)
  phase=$(json_field "$file" phase)
  title=$(json_field "$file" title)
  printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$position" "$record_id" "$status" "$phase" "$title" "$marker"
}

list_open_tasks() {
  local position=0 record_id file status marker
  local -a ordered_ids=()
  while IFS= read -r record_id; do
    ordered_ids+=("$record_id")
  done < <(order_ids)

  local -A seen=()
  # Ordered open tasks first, in order-file order.
  for record_id in "${ordered_ids[@]}"; do
    file="$tasks_dir/$record_id.json"
    status=$(json_field "$file" status)
    is_open "$status" || continue
    seen[$record_id]=1
    position=$((position + 1))
    print_task_row "$position" "$file" ""
  done

  # Open tasks missing from the order listed last, marked unordered.
  for file in "$tasks_dir"/*.json; do
    [[ -e $file ]] || continue
    record_id=$(json_field "$file" id)
    status=$(json_field "$file" status)
    is_open "$status" || continue
    if [[ -n ${seen[$record_id]:-} ]]; then continue; fi
    position=$((position + 1))
    print_task_row "$position" "$file" "unordered"
  done
}

list_open_backlog() {
  local file record_id severity title
  while IFS= read -r file; do
    record_id=$(json_field "$file" id)
    severity=$(json_field "$file" severity)
    title=$(json_field "$file" title)
    printf '%s\t%s\t%s\n' "$record_id" "$severity" "$title"
  done < <(
    for file in "$backlog_dir"/*.json; do
      [[ -e $file ]] || continue
      [[ $(json_field "$file" status) == "open" ]] || continue
      printf '%s\n' "$file"
    done | sort
  )
}

find_record() {
  local record_id=$1
  local file
  for file in "$tasks_dir/$record_id.json" "$backlog_dir/$record_id.json" "$decisions_dir/$record_id.json"; do
    if [[ -e $file ]]; then
      printf '%s\n' "$file"
      return 0
    fi
  done
  return 1
}

command -v jq >/dev/null || fail "jq is required"
[[ -d $tasks_dir ]] || fail "$tasks_dir: no tasks directory"
[[ -d $backlog_dir ]] || fail "$backlog_dir: no backlog directory"
[[ -d $decisions_dir ]] || fail "$decisions_dir: no decisions directory"
[[ -f $order_file ]] || fail "$order_file: no order file"

# Validation always runs.
check_names "$tasks_dir"
check_names "$backlog_dir"
check_names "$decisions_dir"
check_order_ids

if [[ $# -gt 0 ]]; then
  requested=$1
  record_file=$(find_record "$requested") || fail "no record with id '$requested'"
  jq '.' "$record_file"
  exit 0
fi

list_open_tasks
list_open_backlog
