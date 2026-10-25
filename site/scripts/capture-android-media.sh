#!/usr/bin/env bash
# Re-capture the two Android marketing screenshots from the current external
# Android app and this checkout's isolated Warden demo. Run in a persistent
# shell with an already-booted Android emulator/AVD; it never contacts the
# user's Warden daemon or uses its token.
#
# Example:
#   ANDROID_REPO=/tmp/warden-android-app bash site/scripts/capture-android-media.sh
#
# The script drives the connection form itself: only 10.0.2.2:8829 (the
# emulator's alias for this script's loopback-only demo) and a disposable,
# non-secret placeholder are entered. The visible fleet is seeded by
# demo-seed.sh only; no real address, token, device data, or fleet is used.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
ANDROID_REPO="${ANDROID_REPO:?set ANDROID_REPO to a current warden-android-app checkout}"
DEMO_ROOT=/tmp/wd-demo-android
DEMO_PORT=8829
BIN=/tmp/wd-t4-capture/bin/warden
ANDROID_APK="${ANDROID_APK:-}"
APK="$ANDROID_REPO/app/build/outputs/apk/debug/app-debug.apk"
ANDROID_SERIAL="${ANDROID_SERIAL:?set ANDROID_SERIAL to the isolated emulator serial (for example emulator-5554)}"

command -v adb >/dev/null || { echo 'adb is required' >&2; exit 1; }
[ -x "$ANDROID_REPO/gradlew" ] || { echo "not an Android checkout: $ANDROID_REPO" >&2; exit 1; }
git -C "$ANDROID_REPO" diff --quiet || { echo 'Android checkout must be clean' >&2; exit 1; }
git -C "$ANDROID_REPO" rev-parse --verify HEAD >/dev/null
[[ "$ANDROID_SERIAL" =~ ^emulator-[0-9]+$ ]] || { echo "refusing non-emulator ANDROID_SERIAL: $ANDROID_SERIAL" >&2; exit 1; }
ADB=(adb -s "$ANDROID_SERIAL")

# No WARDEN_* connection environment or real token may flow into the capture.
unset WARDEN_TOKEN WARDEN_ADDR WARDEN_CONFIG WARDEN_DATA_DIR
mkdir -p "$(dirname "$BIN")"
(cd "$ROOT" && go build -o "$BIN" ./cmd/warden)
WARDEN_BIN="$BIN" DEMO_ROOT="$DEMO_ROOT" DEMO_PORT="$DEMO_PORT" \
  bash "$ROOT/site/scripts/demo-seed.sh" up
WARDEN_BIN="$BIN" DEMO_ROOT="$DEMO_ROOT" DEMO_PORT="$DEMO_PORT" \
  bash "$ROOT/site/scripts/demo-seed.sh" verify

"${ADB[@]}" wait-for-device
"${ADB[@]}" reverse "tcp:$DEMO_PORT" "tcp:$DEMO_PORT"
if [ -n "$ANDROID_APK" ]; then
  APK="$ANDROID_APK"
  [ -f "$APK" ] || { echo "ANDROID_APK does not exist: $APK" >&2; exit 1; }
else
  (cd "$ANDROID_REPO" && ./gradlew :app:assembleDebug)
fi
"${ADB[@]}" install -r "$APK"
# The emulator is disposable, but clearing still proves no prior application
# connection, token, or host can leak into these pictures.
"${ADB[@]}" shell pm clear com.warden.android >/dev/null
"${ADB[@]}" shell monkey -p com.warden.android 1 >/dev/null

ui_dump() {
  "${ADB[@]}" shell uiautomator dump /sdcard/warden-capture.xml >/dev/null
  "${ADB[@]}" exec-out cat /sdcard/warden-capture.xml >"$DEMO_ROOT/android-ui.xml"
}
tap_label() {
  local label="$1" point
  ui_dump
  point="$(python3 - "$DEMO_ROOT/android-ui.xml" "$label" <<'PY'
import sys
import xml.etree.ElementTree as ET

path, label = sys.argv[1:]
for node in ET.parse(path).iter('node'):
    if label in (node.get('text'), node.get('content-desc')):
        bounds = node.get('bounds', '')
        nums = [int(n) for n in bounds.replace('[', ',').replace(']', ',').split(',') if n]
        if len(nums) == 4:
            print((nums[0] + nums[2]) // 2, (nums[1] + nums[3]) // 2)
            break
else:
    raise SystemExit(f'UI element not found: {label}')
PY
)"
  "${ADB[@]}" shell input tap $point
}
wait_for_label() {
  local label="$1"
  for _ in $(seq 1 30); do
    ui_dump
    grep -Fq "$label" "$DEMO_ROOT/android-ui.xml" && return 0
    sleep 1
  done
  echo "timed out waiting for UI element: $label" >&2
  return 1
}

wait_for_label Host
tap_label Host
"${ADB[@]}" shell input text '10.0.2.2:8829'
# The current app intentionally requires a non-empty token before it enables
# Test connection. This literal is disposable, non-secret, and ignored by the
# no-auth isolated demo daemon; no real WARDEN_TOKEN is ever read or stored.
tap_label 'Bearer token'
"${ADB[@]}" shell input text 'isolated-demo-token'
# Compose keeps the software keyboard open after token entry; hide it before
# locating the button so the accessibility-driven tap cannot be occluded.
"${ADB[@]}" shell input keyevent 4
tap_label 'Test connection'
wait_for_label 'Hosts menu'
grep -q 'api-gateway' "$DEMO_ROOT/android-ui.xml" || { echo 'agents assertion failed: isolated api-gateway fleet is absent' >&2; exit 1; }
grep -q 'billing-svc' "$DEMO_ROOT/android-ui.xml" || { echo 'agents assertion failed: isolated billing-svc fleet is absent' >&2; exit 1; }
"${ADB[@]}" exec-out screencap -p >"$ROOT/site/public/media/warden-app-agents.png"
tap_label 'Hosts menu'
wait_for_label Pipelines
"${ADB[@]}" exec-out screencap -p >"$ROOT/site/public/media/warden-app-hosts.png"

grep -q 'Agents' "$DEMO_ROOT/android-ui.xml" || { echo 'drawer assertion failed: Agents is absent' >&2; exit 1; }
grep -q 'Pipelines' "$DEMO_ROOT/android-ui.xml" || { echo 'drawer assertion failed: Pipelines is absent' >&2; exit 1; }
grep -q 'Scheduled' "$DEMO_ROOT/android-ui.xml" || { echo 'drawer assertion failed: Scheduled is absent' >&2; exit 1; }
grep -q 'Terminals' "$DEMO_ROOT/android-ui.xml" || { echo 'drawer assertion failed: Terminals is absent' >&2; exit 1; }

# This is lossless re-encoding only; it never redraws or edits product pixels.
python3 - "$ROOT/site/public/media/warden-app-agents.png" "$ROOT/site/public/media/warden-app-hosts.png" <<'PY'
from PIL import Image
import sys
for path in sys.argv[1:]:
    image = Image.open(path)
    image.save(path, optimize=True, compress_level=9)
PY

for image in "$ROOT/site/public/media/warden-app-agents.png" "$ROOT/site/public/media/warden-app-hosts.png"; do
  bytes="$(wc -c <"$image")"
  [ "$bytes" -le 300000 ] || { echo "$image is $bytes bytes; must be <=300000" >&2; exit 1; }
  file "$image"
done

"${ADB[@]}" shell pm clear com.warden.android >/dev/null
echo 'Android captures refreshed from current app + isolated demo. Review images before commit.'
