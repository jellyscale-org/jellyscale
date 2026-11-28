#!/usr/bin/env bash
# One-shot spike: real 30-minute pause on Media3, then a seek, then a refused link.
# Requires RESULTS_DIR, ANDROID_AVD_HOME, ANDROID_HOME and JAVA17_HOME.
set -euo pipefail

SPIKE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RESULTS_DIR="${RESULTS_DIR:?set RESULTS_DIR}"
ANDROID_AVD_HOME="${ANDROID_AVD_HOME:?set ANDROID_AVD_HOME}"
SDK="${ANDROID_HOME:-${ANDROID_SDK_ROOT:?set ANDROID_HOME}}"
JAVA17_HOME="${JAVA17_HOME:?set JAVA17_HOME}"

PORT="${PORT:-18080}"
CONSOLE_PORT="${CONSOLE_PORT:-5580}"
AVD_NAME="${AVD_NAME:-js219_tv}"
SECRET="${SECRET:-reel-secret-73}"
DEVICE="emulator-${CONSOLE_PORT}"
BASE="http://10.0.2.2:${PORT}/media/${SECRET}"
URL="${BASE}/master.m3u8"
URL_AGAIN="${BASE}/again/master.m3u8"
STATE_PATH="/sdcard/Android/data/dev.spike.pathsecret/files/state.txt"
APK="$SPIKE_DIR/android/app/build/outputs/apk/debug/app-debug.apk"
ADB="$SDK/platform-tools/adb"

mkdir -p "$RESULTS_DIR" "$ANDROID_AVD_HOME"
export ANDROID_AVD_HOME

if ss -ltn 2>/dev/null | grep -q ":${CONSOLE_PORT}[[:space:]]"; then
  echo "console port ${CONSOLE_PORT} is already in use; choose another" >&2
  exit 1
fi

echo "1/8 generating media"
"$SPIKE_DIR/make-media.sh" "$SPIKE_DIR/assets"

echo "2/8 building app"
printf 'sdk.dir=%s\n' "$SDK" > "$SPIKE_DIR/android/local.properties"
(
  cd "$SPIKE_DIR/android"
  JAVA_HOME="$JAVA17_HOME" ./gradlew assembleDebug --console=plain > "$RESULTS_DIR/gradle.log" 2>&1
)

echo "3/8 building stand-in origin"
go build -o "$RESULTS_DIR/origin" "$SPIKE_DIR/server.go"

echo "4/8 creating AVD and starting processes"
if [ ! -d "$ANDROID_AVD_HOME/${AVD_NAME}.avd" ]; then
  printf 'no\n' | "$SDK/cmdline-tools/latest/bin/avdmanager" create avd \
    -n "$AVD_NAME" -k 'system-images;android-36;android-tv;x86_64' \
    --path "$ANDROID_AVD_HOME/${AVD_NAME}.avd" > "$RESULTS_DIR/avd.log" 2>&1
fi

"$RESULTS_DIR/origin" -addr "127.0.0.1:${PORT}" -dir "$SPIKE_DIR/assets" \
  -secret "$SECRET" -log "$RESULTS_DIR/server.log" \
  > "$RESULTS_DIR/origin.stderr" 2>&1 &
SERVER_PID=$!

"$SDK/emulator/emulator" -avd "$AVD_NAME" -no-window -no-audio \
  -gpu swiftshader_indirect -port "$CONSOLE_PORT" -no-snapshot -no-boot-anim \
  > "$RESULTS_DIR/emulator.log" 2>&1 &
EMULATOR_PID=$!

cleanup() {
  "$ADB" -s "$DEVICE" emu kill > "$RESULTS_DIR/kill.log" 2>&1 || true
  kill "$EMULATOR_PID" 2>/dev/null || true
  kill "$SERVER_PID" 2>/dev/null || true
  pkill -P "$SERVER_PID" 2>/dev/null || true
}
on_exit() {
  status=$?
  if [ "$status" -ne 0 ]; then
    "$ADB" -s "$DEVICE" logcat -d -v threadtime > "$RESULTS_DIR/logcat-failure.log" 2>&1 || true
  fi
  cleanup
}
trap on_exit EXIT

for attempt in {1..30}; do
  curl -fsS -o /dev/null "http://127.0.0.1:${PORT}/media/${SECRET}/master.m3u8" && break
  sleep 1
done

timeout 180 "$ADB" -s "$DEVICE" wait-for-device
for attempt in {1..180}; do
  state=$("$ADB" -s "$DEVICE" get-state 2>/dev/null || true)
  if [ "$state" = "device" ]; then
    break
  fi
  sleep 1
done
if [ "${state:-}" != "device" ]; then
  echo "emulator ${DEVICE} never came online" >&2
  exit 1
fi
for attempt in {1..420}; do
  boot=$("$ADB" -s "$DEVICE" shell getprop sys.boot_completed 2>/dev/null || true)
  if [ "$boot" = "1" ]; then
    break
  fi
  sleep 1
done
if [ "${boot:-}" != "1" ]; then
  echo "emulator ${DEVICE} did not finish booting" >&2
  exit 1
fi
"$ADB" -s "$DEVICE" shell input keyevent 82 || true
"$ADB" -s "$DEVICE" install -r "$APK" > "$RESULTS_DIR/install.log" 2>&1
"$ADB" -s "$DEVICE" logcat -c
"$ADB" -s "$DEVICE" shell rm -f "$STATE_PATH"
"$ADB" -s "$DEVICE" logcat -v threadtime > "$RESULTS_DIR/logcat.log" 2>&1 &
LOGCAT_PID=$!

echo "5/8 starting playback"
"$ADB" -s "$DEVICE" shell am start -n dev.spike.pathsecret/.PlayerActivity --es url "$URL" \
  > "$RESULTS_DIR/start.log" 2>&1

STATE_LINES=0
snapshot_state() {
  local name=$1
  "$ADB" -s "$DEVICE" shell cat "$STATE_PATH" > "$RESULTS_DIR/state-${name}.txt" 2>/dev/null || true
  STATE_LINES=$(wc -l < "$RESULTS_DIR/state-${name}.txt")
}

wait_state() {
  local needle=$1 limit=$2
  for ((step = 0; step < limit; step++)); do
    local state
    state=$("$ADB" -s "$DEVICE" shell cat "$STATE_PATH" 2>/dev/null || true)
    if printf '%s\n' "$state" | grep -Eq "$needle"; then
      return 0
    fi
    sleep 1
  done
  echo "timed out waiting for state pattern: $needle" >&2
  return 1
}

wait_new_state() {
  local needle=$1 limit=$2
  for ((step = 0; step < limit; step++)); do
    local state
    state=$("$ADB" -s "$DEVICE" shell cat "$STATE_PATH" 2>/dev/null || true)
    if printf '%s\n' "$state" | tail -n "+$((STATE_LINES + 1))" | grep -Eq "$needle"; then
      return 0
    fi
    sleep 1
  done
  echo "timed out waiting for new state pattern: $needle" >&2
  return 1
}

wait_state 'TICK state=3 .* playing=true' 90
wait_state 'VIDEO 1280x720' 30
snapshot_state playing

PAUSE_START=skipped
PAUSE_END=skipped
REQUESTS_AT_START=0
REQUESTS_AT_PAUSE=0
REQUESTS_AT_RESUME=0
SEEK_OK=0
SEGMENTS_AFTER_SEEK=""

if [ "${SKIP_PAUSE:-0}" != "1" ]; then
echo "6/8 pausing for a real 30 minutes"
PAUSE_START=$(date -u +%Y-%m-%dT%H:%M:%SZ)
REQUESTS_AT_START=$(grep -c ' REQ ' "$RESULTS_DIR/server.log" || true)
"$ADB" -s "$DEVICE" shell am broadcast -a dev.spike.pathsecret.COMMAND \
  -n dev.spike.pathsecret/.CommandReceiver --es action pause \
  > "$RESULTS_DIR/pause-command.log" 2>&1
wait_new_state 'CMD pause applied' 20
sleep 5
REQUESTS_AT_PAUSE=$(grep -c ' REQ ' "$RESULTS_DIR/server.log" || true)
snapshot_state paused
sleep 1800
PAUSE_END=$(date -u +%Y-%m-%dT%H:%M:%SZ)
REQUESTS_AT_RESUME=$(grep -c ' REQ ' "$RESULTS_DIR/server.log" || true)
SERVER_LINES_AT_RESUME=$(wc -l < "$RESULTS_DIR/server.log")

echo "7/8 seeking after the pause"
"$ADB" -s "$DEVICE" shell am broadcast -a dev.spike.pathsecret.COMMAND \
  -n dev.spike.pathsecret/.CommandReceiver --es action seek --ei ms 45000 \
  > "$RESULTS_DIR/seek-command.log" 2>&1
wait_new_state 'CMD seek applied' 20
"$ADB" -s "$DEVICE" shell am broadcast -a dev.spike.pathsecret.COMMAND \
  -n dev.spike.pathsecret/.CommandReceiver --es action resume \
  > "$RESULTS_DIR/resume-command.log" 2>&1
SEEK_OK=0
if wait_new_state 'TICK state=3 pos=(4[5-9][0-9]{3}|[5-9][0-9]{4}|[1-9][0-9]{5}) playing=true' 120; then
  SEEK_OK=1
fi
snapshot_state after_seek
SEGMENTS_AFTER_SEEK=$(tail -n "+$((SERVER_LINES_AT_RESUME + 1))" "$RESULTS_DIR/server.log" \
  | grep -E ' REQ GET /media/.*/seg[0-9]+\.m4s ' | head -10 || true)
fi

echo "8/8 asking for a refused link"
"$ADB" -s "$DEVICE" shell am broadcast -a dev.spike.pathsecret.COMMAND \
  -n dev.spike.pathsecret/.CommandReceiver --es action stop \
  > "$RESULTS_DIR/stop-command.log" 2>&1
curl -fsS "http://127.0.0.1:${PORT}/control/end" > "$RESULTS_DIR/control.log" 2>&1
"$ADB" -s "$DEVICE" shell am force-stop dev.spike.pathsecret
"$ADB" -s "$DEVICE" shell am start -n dev.spike.pathsecret/.PlayerActivity --es url "$URL_AGAIN" \
  > "$RESULTS_DIR/start-again.log" 2>&1
REFUSAL_START_LINE=$STATE_LINES
REFUSAL_OK=0
if wait_new_state 'PLAYER_ERROR|LOAD_ERROR' 90; then
  REFUSAL_OK=1
fi
snapshot_state refused

printf 'pause_start=%s\npause_end=%s\nrequests_at_start=%s\nrequests_at_pause=%s\nrequests_at_resume=%s\nrequests_during_pause=%s\nseek_ok=%s\nrefusal_reported=%s\n' \
  "$PAUSE_START" "$PAUSE_END" "$REQUESTS_AT_START" "$REQUESTS_AT_PAUSE" \
  "$REQUESTS_AT_RESUME" "$((REQUESTS_AT_RESUME - REQUESTS_AT_PAUSE))" \
  "$SEEK_OK" "$REFUSAL_OK" > "$RESULTS_DIR/summary.txt"
printf 'segments after seek:\n%s\n' "$SEGMENTS_AFTER_SEEK" >> "$RESULTS_DIR/summary.txt"
printf 'refusal lines:\n' >> "$RESULTS_DIR/summary.txt"
tail -n "+$((REFUSAL_START_LINE + 1))" "$RESULTS_DIR/state-refused.txt" >> "$RESULTS_DIR/summary.txt" || true

echo "done: $(cat "$RESULTS_DIR/summary.txt" | tr '\n' ' ')"
