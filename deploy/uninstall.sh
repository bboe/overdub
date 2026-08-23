#!/bin/bash
# Remove overdub from a rooted Echo Dot (2nd Generation), over adb, and give the
# action button back to Alexa. Set ANDROID_SERIAL to pick one of several
# attached or connected devices.
set -e

BOOT=/sbin/.core/img/.core/service.d/overdub.sh
BIN=/data/local/bin/overdub
LOG=/data/local/tmp/overdub.log

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
  rm -f $BIN ${BIN}.new
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

left=$(adb shell "su -c '
  for path in $BOOT $BIN ${BIN}.new $LOG; do
    [ -e \"\$path\" ] && echo \"\$path\"
  done
  true
'" | tr -d '\r' | grep . || true)

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
  echo "STILL RUNNING: overdub is pid $still; the kill did not take." >&2
  echo "  Nothing starts it again, since the boot script and the binary are" >&2
  echo "  both gone, so a reboot is the end of it." >&2
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

rule=$(adb shell "su -c 'iptables -L INPUT -n | grep 6053'" | tr -d '\r' | grep . || true)
if [ -n "$rule" ]; then
  echo "The tcp/6053 rule is still in the INPUT chain:" >&2
  echo "  $rule" >&2
  echo "  Nothing listens behind it now. It lives in the chain rather than on" >&2
  echo "  disk, so a reboot clears it." >&2
fi

echo "Removed. The action button belongs to Alexa again."
