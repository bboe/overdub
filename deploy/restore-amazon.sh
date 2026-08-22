#!/bin/bash
# Return a Dot from an EchoMuse install to stock Amazon services, so that
# overdub can run beside Alexa. Host-side over adb, like install.sh.
set -e

KEEP_HIDDEN="com.amazon.device.software.ota"

SERVICES="vitals_service perfmonitord perfrecoveryd shblemeshd meshmgrservice
          drm whad_cc avahi-daemon"

DEBLOAT=/sbin/.core/img/.core/service.d/echomuse-debloat.sh

pkgs() { adb shell "su -c 'pm list packages $1'" | tr -d '\r' | sed 's/^package://' | sort; }
state() { adb shell "su -c 'dumpsys package $1'" | tr -d '\r' | sed -n 's/^ *User 0: *//p'; }

if ! adb shell 'su -c "id"' | tr -d '\r' | grep -q 'uid=0'; then
  echo "no root here: su -c id did not report uid=0" >&2
  exit 1
fi

hidden=$(comm -23 <(pkgs -u) <(pkgs))
echo "hidden:   $(echo "$hidden" | grep -c .) packages"

commands=""
kept=0
for package in $hidden; do
  if [ "$package" = "$KEEP_HIDDEN" ]; then kept=1; continue; fi
  commands="$commands pm unhide $package;"
done
[ -n "$commands" ] && adb shell "su -c '$commands'" > /dev/null
[ "$kept" = 1 ] && echo "left hidden: $KEEP_HIDDEN"

disabled=$(pkgs -d)
echo "disabled: $(echo "$disabled" | grep -c .) packages"
commands=""
for package in $disabled; do
  [ "$package" = "$KEEP_HIDDEN" ] && continue
  commands="$commands pm enable $package;"
done
[ -n "$commands" ] && adb shell "su -c '$commands'" > /dev/null

adb shell "su -c 'rm -f $DEBLOAT'"
commands=""
for service in $SERVICES; do commands="$commands start $service;"; done
adb shell "su -c '$commands'" > /dev/null

adb shell "su -c '
  mkdir -p /data/local/echomuse-disabled
  for stale in /data/local/bin/server /data/local/bin/server_a \
           /data/local/bin/start_server.sh /data/local/etc/echomuse \
           /data/local/share /data/local/dnsmasq.pid; do
    [ -e \"\$stale\" ] && mv -f \"\$stale\" /data/local/echomuse-disabled/
  done
  true
'"

still_hidden=$(comm -23 <(pkgs -u) <(pkgs))
still_disabled=$(pkgs -d)
fail=0
for package in $still_hidden; do
  [ "$package" = "$KEEP_HIDDEN" ] && continue
  case "$(state "$package")" in
    *hidden=true*)
      echo "STILL HIDDEN: pm unhide did not take on $package" >&2; fail=1 ;;
    *installed=false*)
      echo "NOT RESTORED, uninstalled for this user rather than hidden: $package" >&2 ;;
    *)
      echo "UNREADABLE: dumpsys reported no user state for $package" >&2; fail=1 ;;
  esac
done
for package in $still_disabled; do
  [ "$package" = "$KEEP_HIDDEN" ] && continue
  echo "STILL DISABLED: $package" >&2; fail=1
done
if adb shell "su -c '[ -f $DEBLOAT ] && echo yes'" | tr -d '\r' | grep -q yes; then
  echo "STILL PRESENT: $DEBLOAT" >&2; fail=1
fi
[ "$fail" = 1 ] && { echo "RESTORE INCOMPLETE" >&2; exit 1; }

echo
echo "Restored. $KEEP_HIDDEN is still hidden, by design."
echo "Reboot now: the packages just unhidden include persistent services that"
echo "only init starts, and Alexa will not be whole until they are running."
