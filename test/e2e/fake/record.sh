#!/usr/bin/env bash
# What each default gate does, with a stand-in browser (run-gate.sh): per gate and shard, the scenarios
# it ran in order and its shard line; for browse and play, each step's outcome and name. The recorded
# output of the tree before JS-114 is compared with the build's by test/js114-before-after.sh.
# Leaves each gate's output in <workdir>/<gate>[-<shard>].out and its exit status in .status.
# Usage: record.sh <repo> <workdir>
set -uo pipefail
repo=$1 work=$2
mkdir -p "$work"
pids=()
for gate in subtitles failover transcode; do
  for shard in 0 1 2; do
    ( bash "$repo/test/e2e/fake/run-gate.sh" "$repo" "$work/$gate-$shard" "$gate" "E2E_SHARD=$shard/3" >"$work/$gate-$shard.out" 2>&1; echo $? >"$work/$gate-$shard.status" ) &
    pids+=($!)
  done
done
for gate in browse play; do
  ( bash "$repo/test/e2e/fake/run-gate.sh" "$repo" "$work/$gate" "$gate" >"$work/$gate.out" 2>&1; echo $? >"$work/$gate.status" ) &
  pids+=($!)
done
wait "${pids[@]}"
for gate in subtitles failover transcode; do
  for shard in 0 1 2; do
    echo "== $gate $shard/3"
    grep -E '^(run   |shard )' "$work/$gate-$shard.out"
  done
done
for gate in browse play; do
  echo "== $gate"
  # A step's outcome and name; its reason and time are not recorded.
  grep -E '^(ok|FAIL)  ' "$work/$gate.out" | sed -E 's/ \([0-9.]+s\)$//; s/: (fake browser: |no |the client never).*$//' 
  grep -E '^shard ' "$work/$gate.out"
done
