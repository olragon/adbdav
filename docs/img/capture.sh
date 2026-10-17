#!/usr/bin/env bash
# Regenerate the README screenshots.
#
#   ./docs/img/capture.sh [-f <folder on the mount>] [-x X -y Y]
#
# ⛔ Three things in these windows must never be published:
#
#   * the DEVICE SERIAL, printed next to the model in the menu. Blurred below.
#   * Finder's SIDEBAR, which lists every mounted volume and signed-in cloud
#     account. Hidden before the capture.
#   * the FILE NAMES of whatever folder you point this at. The default is
#     DCIM/Camera, where names are camera timestamps; the volume root of a
#     working phone is not safe — its names describe what you were doing.
#
# Two things about capturing on macOS that cost an hour to learn:
#
#   * AppleScript's `bounds of Finder window` and `screencapture -R` do NOT
#     agree once a second display is attached. The accessibility API's
#     `position of window` IS in screencapture's coordinate space — use it,
#     and move the window by setting that same property.
#   * `screencapture -R` photographs the SCREEN, not the window. An
#     always-on-top window will appear in your shot and `AXRaise` will not
#     move it. Put the Finder window somewhere clear instead.
#
# It still needs a human eye: open both PNGs afterwards and read them.
set -euo pipefail
cd "$(dirname "$0")"
OUT=$PWD

MOUNT=${MOUNT:-$HOME/Android}
FOLDER=DCIM/Camera
X=-1800   # a clear patch of screen; see the note above
Y=220
W=700
H=440
while getopts "f:x:y:h" opt; do
  case "$opt" in
    f) FOLDER=$OPTARG ;;
    x) X=$OPTARG ;;
    y) Y=$OPTARG ;;
    h) sed -n '2,25p' "$0"; exit 0 ;;
    *) exit 2 ;;
  esac
done

mount | grep -q " on $MOUNT " || { echo "capture: nothing mounted at $MOUNT — mount it first" >&2; exit 1; }
command -v magick >/dev/null || { echo "capture: needs ImageMagick (brew install imagemagick)" >&2; exit 1; }
TARGET=$MOUNT/$FOLDER
[ -d "$TARGET" ] || { echo "capture: no such folder on the mount: $TARGET" >&2; exit 1; }
TITLE=$(basename "$TARGET")

# ---------------------------------------------------------------- Finder
osascript <<EOF >/dev/null
tell application "Finder"
  activate
  set w to make new Finder window to (POSIX file "$TARGET" as alias)
  set current view of w to list view
  set sidebar width of w to 0
end tell
delay 1
tell application "System Events" to tell process "Finder"
  set w to window "$TITLE"
  set position of w to {$X, $Y}
  set size of w to {$W, $H}
  perform action "AXRaise" of w
end tell
EOF
# A cold listing is an adb round trip; capturing too early photographs empty
# rows, which looks exactly like a broken mount.
sleep 5
screencapture -x -R"$X,$Y,$W,$H" "$OUT/finder.png"
magick "$OUT/finder.png" \
  \( -size "${W}x${H}" xc:none -draw "roundrectangle 0,0,$((W-1)),$((H-1)),10,10" -alpha extract \) \
  -alpha off -compose CopyOpacity -composite "$OUT/finder.png"
osascript -e 'tell application "Finder" to close front window' >/dev/null 2>&1 || true

# --------------------------------------------------------------- menu bar
# The menu must be open for its geometry to exist, and it closes as soon as
# anything else takes focus — so open, measure, shoot, in one breath.
GEO=$(osascript <<'EOF'
tell application "System Events" to tell process "adbdav"
  click menu bar item 1 of menu bar 1
  delay 1.5
  set p to position of menu 1 of menu bar item 1 of menu bar 1
  set s to size of menu 1 of menu bar item 1 of menu bar 1
  return ((item 1 of p) as string) & " " & ((item 2 of p) as string) & " " & ((item 1 of s) as string) & " " & ((item 2 of s) as string)
end tell
EOF
)
read -r MX MY MW MH <<<"$GEO"
[ "${MW:-0}" -gt 0 ] || { echo "capture: the menu did not open (is Adbdav.app running?)" >&2; exit 1; }
screencapture -x -R"$MX,0,$MW,$((MY + MH))" "$OUT/menubar-raw.png"
osascript -e 'tell application "System Events" to key code 53' >/dev/null 2>&1 || true

# Blur, in the captured image's own coordinates: the menu bar strip right of
# our own icon (other people's apps), and the right half of the first menu
# row, which is "Device: <model> (<serial>)".
magick "$OUT/menubar-raw.png" \
  -region "$((MW - 45))x24+45+0"                        -blur 0x8 +region \
  -region "$((MW / 2))x18+$((MW / 2 - 8))+$((MY + 14))" -blur 0x6 +region \
  "$OUT/menubar.png"
rm -f "$OUT/menubar-raw.png"

echo "wrote $OUT/finder.png and $OUT/menubar.png"
echo "NOW LOOK AT THEM: the serial must be unreadable, and no sidebar, no other"
echo "app's window and no revealing file name may be visible."
