#!/bin/bash
# Install overdub persistently on a rooted Echo Dot (2nd Generation), over adb.
# Set ANDROID_SERIAL to pick one of several attached or connected devices.
# Every step here is read back; docs/deployment.md says why.
set -e
case "${1:-}" in
  -h|--help|"")
    echo "usage: install.sh <device-name>          e.g. install.sh kitchen" >&2
    echo "       ANDROID_SERIAL=<serial> install.sh <device-name>" >&2
    exit 2 ;;
esac
NAME="$1"
cd "$(dirname "$0")/.."

boot_script=""
trap 'rm -f "$boot_script"' EXIT

case "$NAME" in
  *[!a-z0-9_-]*) echo "name must be lowercase letters, digits, - or _" >&2; exit 2 ;;
  -*|*-) echo "name must not start or end with -; that is not a valid DNS label" >&2; exit 2 ;;
esac
if [ "${#NAME}" -gt 63 ]; then
  echo "name is ${#NAME} characters; a DNS label stops at 63" >&2
  exit 2
fi

local_md5() {
  if command -v md5sum >/dev/null 2>&1; then md5sum "$1" | awk '{print $1}'
  else md5 -q "$1"; fi
}

if git rev-parse --git-dir >/dev/null 2>&1; then
  dirty=$(git status --porcelain || true)
  if [ -n "$dirty" ]; then
    echo "WARNING: this build carries uncommitted changes:" >&2
    echo "$dirty" >&2
  fi
fi

./build.sh

boot_script=$(mktemp)
sed "s/^NAME=\$/NAME=$NAME/" deploy/overdub.sh > "$boot_script"
grep -q "^NAME=$NAME\$" "$boot_script" || { echo "could not set NAME in the boot script" >&2; exit 1; }

if ! adb shell 'su -c "id"' | tr -d '\r' | grep -q 'uid=0'; then
  echo "no root here: su -c id did not report uid=0" >&2
  exit 1
fi

adb push build/overdub /data/local/tmp/overdub
adb push "$boot_script" /data/local/tmp/s.sh

adb shell 'su -c "
  mkdir -p /data/local/bin
  cp /data/local/tmp/overdub /data/local/bin/overdub.new
  chmod 755 /data/local/bin/overdub.new
  mv -f /data/local/bin/overdub.new /data/local/bin/overdub
  cp /data/local/tmp/s.sh      /sbin/.core/img/.core/service.d/overdub.sh
  chmod 755 /sbin/.core/img/.core/service.d/overdub.sh
  rm -f /data/local/tmp/overdub /data/local/tmp/s.sh
"'

built_md5=$(local_md5 build/overdub)
installed_md5=$(adb shell 'su -c "md5 /data/local/bin/overdub"' | tr -d "\r" | awk '{print $1}')
if [ "${#installed_md5}" != 32 ]; then
  echo "INSTALL FAILED: the device did not hash the binary: ${installed_md5:-no output}" >&2
  exit 1
fi
if [ "$built_md5" != "$installed_md5" ]; then
  echo "INSTALL FAILED: device has $installed_md5, built $built_md5" >&2
  exit 1
fi
if [ "$(adb shell 'su -c "[ -x /data/local/bin/overdub ] && echo yes"' | tr -d "\r")" != yes ]; then
  echo "INSTALL FAILED: the binary landed, but is not executable." >&2
  exit 1
fi
echo "binary verified on device ($built_md5)"

if ! adb shell 'su -c "cat /sbin/.core/img/.core/service.d/overdub.sh"' 2>/dev/null |
   tr -d "\r" | diff -q - "$boot_script" >/dev/null 2>&1; then
  echo "INSTALL FAILED: the boot script did not land in service.d." >&2
  echo "  This device's Magisk 17.3 keeps service.d inside magisk.img, at the" >&2
  echo "  /sbin path above; later Magisk dropped the image for /data/adb/service.d." >&2
  echo "  Check which this one runs: adb shell su -c 'magisk -v'" >&2
  exit 1
fi
if [ "$(adb shell 'su -c "[ -x /sbin/.core/img/.core/service.d/overdub.sh ] && echo yes"' | tr -d "\r")" != yes ]; then
  echo "INSTALL FAILED: the boot script landed, but is not executable." >&2
  exit 1
fi
echo "boot script verified on device"
adb shell 'su -c "ls -l /data/local/bin/"'
overdub_pid() {
  local listing
  listing=$(adb shell 'su -c "ps"') || return 1
  printf '%s' "$listing" | tr -d "\r" | awk '$NF ~ /bin\/overdub$/ {print $2}' | head -1
}

old_pid=$(overdub_pid) || { echo "INSTALL FAILED: adb went away before the restart check" >&2; exit 1; }
new_pid=""
if [ -n "$old_pid" ]; then
  adb shell "su -c 'kill $old_pid'"
  sleep 8   # the supervisor respawns every 5
  new_pid=$(overdub_pid) || { echo "INSTALL FAILED: adb went away during the restart check" >&2; exit 1; }
fi

echo
if [ -z "$old_pid" ]; then
  echo "Installed. Reboot to start it, or run it by hand to test first."
elif [ -z "$new_pid" ]; then
  echo "Installed, but the daemon did NOT come back. It was not started by the"
  echo "boot script, so nothing respawned it. Reboot, or start it by hand."
elif [ "$new_pid" = "$old_pid" ]; then
  echo "Installed, but the daemon did not restart (still pid $new_pid)."
else
  echo "Installed, and restarted: pid $old_pid -> $new_pid, running the binary above."
fi

# Asked of the daemon rather than inferred from the script on disk. The
# supervisor read its arguments at boot, so a second install with the same new
# name finds the script already in place and would report nothing, while the
# daemon still runs under the old one.
if [ -n "$new_pid" ]; then
  running=$(adb shell "su -c 'cat /proc/$new_pid/cmdline'" 2>/dev/null |
    tr -d "\r" | tr "\0" " ")
  case "$running" in
    *"-name $NAME "*|*"-name $NAME") ;;
    "")
      echo
      echo "WARNING: could not read /proc/$new_pid/cmdline, so the running -name"
      echo "is unverified. Reboot if you changed it."
      ;;
    *)
      echo
      echo "REBOOT REQUIRED: the daemon is running as:"
      echo "  $running"
      echo "and not with -name $NAME. The supervisor is a shell loop holding the"
      echo "arguments it was started with, so it respawns the old ones. Only a"
      echo "reboot applies the new name."
      ;;
  esac
fi
echo "Home Assistant must be able to reach this device on tcp/6053. See README.md."
