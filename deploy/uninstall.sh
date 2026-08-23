#!/bin/bash
# Remove overdub from a rooted Echo Dot (2nd Generation), over adb, and give the
# action button back to Alexa. Set ANDROID_SERIAL to pick one of several
# attached or connected devices.
set -e

BOOT=/sbin/.core/img/.core/service.d/overdub.sh
BIN=/data/local/bin/overdub
LOG=/data/local/tmp/overdub.log
KEY=/data/local/bin/.overdub-noise-key
STAGE=/data/local/tmp/overdub-install

if ! adb shell 'su -c "id"' | tr -d '\r' | grep -q 'uid=0'; then
  echo "no root here: su -c id did not report uid=0" >&2
  exit 1
fi

overdub_pid() {
  local listing
  listing=$(adb shell 'su -c "ps"') || return 1
  printf '%s' "$listing" | tr -d "\r" | awk '$NF ~ /bin\/overdub$/ {print $2}' | head -1
}

adb shell "su -c 'rm -f $BOOT'"

adb shell "su -c '
  rm -f $BIN ${BIN}.new $KEY
  rm -rf $STAGE
  rm -f /data/local/tmp/overdub /data/local/tmp/s.sh
'"

pid=$(overdub_pid) || { echo "UNINSTALL FAILED: adb went away before the kill" >&2; exit 1; }
if [ -n "$pid" ]; then
  adb shell "su -c 'kill $pid'"
  sleep 8   # the supervisor respawns every 5
fi

adb shell "su -c '
  rm -f $LOG
  rmdir /data/local/bin 2>/dev/null
  true
'"
sleep 6   # the supervisor recreates the log every 5

# Matched against the paths themselves, and the loop says when it finished so an
# empty answer cannot pass for a clean device. This is the check standing between
# the operator and being told the key is gone. docs/pitfalls.md says why.
answer=$(adb shell "su -c '
  for path in $BOOT $BIN ${BIN}.new $KEY $STAGE $LOG; do
    [ -e \"\$path\" ] && echo \"\$path\"
  done
  echo swept
'" | tr -d '\r')
if ! printf '%s\n' "$answer" | grep -qx swept; then
  echo "UNINSTALL FAILED: could not read the device back, so nothing here is" >&2
  echo "  confirmed removed. Re-run this script with the Dot connected." >&2
  exit 1
fi
left=$(printf '%s\n' "$answer" |
  grep -Fx -e "$BOOT" -e "$BIN" -e "${BIN}.new" -e "$KEY" -e "$STAGE" -e "$LOG" || true)

fail=0
for path in $left; do
  if [ "$path" = "$LOG" ]; then
    echo "STILL SUPERVISED: $LOG came back after it was removed, so the" >&2
    echo "  service.d loop is still running. ps shows it as bare sh rather" >&2
    echo "  than the script it runs, so a reboot is what ends it." >&2
  else
    echo "STILL PRESENT: $path" >&2
  fi
  fail=1
done

still=$(overdub_pid) || { echo "UNINSTALL FAILED: adb went away during the check" >&2; exit 1; }
if [ -n "$still" ]; then
  echo "STILL RUNNING: overdub is pid $still." >&2
  # Conditional, because the reassuring version holds only when the removals
  # worked, and the supervisor respawns for as long as the binary is there.
  if [ -n "$left" ]; then
    echo "  Things above are still on the device, so it will start again, at the" >&2
    echo "  next respawn or the next boot. Fix those and run this script again." >&2
  else
    echo "  The kill did not take, but nothing starts it again: the boot script" >&2
    echo "  and the binary are both gone, so a reboot is the end of it." >&2
  fi
  fail=1
fi
[ "$fail" = 1 ] && { echo "UNINSTALL INCOMPLETE" >&2; exit 1; }

# Only now the daemon is gone: it re-asserts this rule every thirty seconds, so
# a deletion while it lived would be undone before the next line ran.
adb shell "su -c '
  while iptables -w -C INPUT -i wlan0 -p tcp --dport 6053 -j ACCEPT 2>/dev/null; do
    iptables -w -D INPUT -i wlan0 -p tcp --dport 6053 -j ACCEPT || break
  done
'" >/dev/null 2>&1 || true

# Same sentinel: silence would otherwise read as a chain with no rule in it.
rule_answer=$(adb shell "su -c 'iptables -L INPUT -n | grep 6053; echo checked'" | tr -d '\r')
rule=$(printf '%s\n' "$rule_answer" | grep -E 'dpt:6053' || true)
if ! printf '%s\n' "$rule_answer" | grep -qx checked; then
  rule="could not read the chain back"
fi
if [ -n "$rule" ]; then
  echo "The tcp/6053 rule is still in the INPUT chain:" >&2
  echo "  $rule" >&2
  echo "  Nothing listens behind it now. It lives in the chain rather than on" >&2
  echo "  disk, so a reboot clears it." >&2
fi

echo "Removed. The action button belongs to Alexa again."
