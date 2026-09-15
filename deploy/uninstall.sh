#!/bin/bash
# Remove overdub from a rooted Echo Dot (2nd Generation), over adb, and give the
# action button back to Alexa. Set ANDROID_SERIAL to pick one of several
# attached or connected devices.
set -e

BOOT=/sbin/.core/img/.core/service.d/overdub.sh
BIN=/data/local/bin/overdub
LOG=/data/local/tmp/overdub.log
KEY=/data/local/bin/.overdub-noise-key
SENDKEY=/data/local/bin/.overdub-sendspin-key
ADBKEY=/data/local/bin/adb_keys
ADBKEYS=/data/misc/adb/adb_keys
STAGE=/data/local/tmp/overdub-install
MAP=/data/local/map
APIPORT=6053
SENDPORT=8928
SENDFLAG=persist.overdub.sendspin

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
  rm -f $BIN ${BIN}.new $KEY $SENDKEY ${SENDKEY}.new-* $ADBKEY
  rm -rf $STAGE $MAP
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

answer=$(adb shell "su -c '
  for path in $BOOT $BIN ${BIN}.new $KEY $SENDKEY ${SENDKEY}.new-* $STAGE $MAP $LOG; do
    [ -e \"\$path\" ] && echo \"\$path\"
  done
  echo swept
'" | tr -d '\r')
if ! printf '%s\n' "$answer" | grep -qx swept; then
  echo "UNINSTALL FAILED: could not read the device back, so nothing here is" >&2
  echo "  confirmed removed. Re-run this script with the Dot connected." >&2
  exit 1
fi
# Two passes: the fixed paths by exact match, and the key's temporary files by
# prefix, because a crashed first run leaves one behind with a random suffix. An
# allowlist rather than "anything the device echoed", because adb merges the
# device's stderr into its stdout and a linker warning would read as a leftover.
left=$(
  printf '%s\n' "$answer" |
    grep -Fx -e "$BOOT" -e "$BIN" -e "${BIN}.new" -e "$KEY" -e "$SENDKEY" -e "$STAGE" \
      -e "$MAP" -e "$LOG" || true
  printf '%s\n' "$answer" | grep -E "^${SENDKEY//./[.]}[.]new-" || true
)

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

for port in "$APIPORT" "$SENDPORT"; do
  adb shell "su -c '
    while iptables -w -C INPUT -i wlan0 -p tcp --dport $port -j ACCEPT 2>/dev/null; do
      iptables -w -D INPUT -i wlan0 -p tcp --dport $port -j ACCEPT || break
    done
  '" >/dev/null 2>&1 || true

  rule_answer=$(adb shell "su -c 'iptables -L INPUT -n | grep $port; echo checked'" | tr -d '\r')
  rule=$(printf '%s\n' "$rule_answer" | grep -E "dpt:$port" || true)
  if ! printf '%s\n' "$rule_answer" | grep -qx checked; then
    rule="could not read the chain back"
  fi
  if [ -n "$rule" ]; then
    echo "The tcp/$port rule is still in the INPUT chain:" >&2
    echo "  $rule" >&2
    echo "  Nothing listens behind it now. It lives in the chain rather than on" >&2
    echo "  disk, so a reboot clears it." >&2
  fi
done

adb shell "su -c '
  setprop persist.overdub.sendspin \"\"
  rm -f /data/property/persist.overdub.sendspin
'" >/dev/null 2>&1 || true

flag_answer=$(adb shell "su -c 'getprop persist.overdub.sendspin; echo checked'" | tr -d '\r')
if ! printf '%s\n' "$flag_answer" | grep -qx checked; then
  echo "Could not read $SENDFLAG back; it may still be set." >&2
elif [ -n "$(printf '%s\n' "$flag_answer" | grep -v -e '^checked$' -e '^$' || true)" ]; then
  echo "$SENDFLAG is still set:" >&2
  printf '  %s\n' "$(printf '%s\n' "$flag_answer" | grep -v -e '^checked$' -e '^$')" >&2
  echo "  It only decides whether a future install starts Sendspin switched on." >&2
fi

echo
echo "Home Assistant can no longer talk to this device: the API key it was"
echo "configured with is gone. Installing again generates a NEW key and prints"
echo "it once. Give that to the ESPHome integration, which asks for it when the"
echo "handshake fails."

echo
echo "Reboot to finish. Whatever Network ADB was last set to is still in force:"
echo "it lives in the property store and the firewall chain rather than on disk."
echo "If it was Insecure, tcp/5555 is an unauthenticated root shell until you"
echo "reboot."
echo
echo "One file is left on purpose: the public key at $ADBKEYS."
echo "adbd consults it only while ro.adb.secure is 1, and nothing sets that once"
echo "this is gone, so it grants nothing. Removing it would take away the half"
echo "that grants access and leave the half that denies it."

echo "Removed. The action button belongs to Alexa again."
