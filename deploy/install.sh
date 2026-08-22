#!/bin/bash
# Every step here is read back; docs/deployment.md says why.
set -e
cd "$(dirname "$0")/.."

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
if ! adb shell 'su -c "id"' | tr -d '\r' | grep -q 'uid=0'; then
  echo "no root here: su -c id did not report uid=0" >&2
  exit 1
fi

adb push build/overdub /data/local/tmp/overdub
adb push deploy/overdub.sh /data/local/tmp/s.sh

script_changed=yes
if adb shell 'su -c "cat /sbin/.core/img/.core/service.d/overdub.sh"' 2>/dev/null |
   tr -d "\r" | diff -q - deploy/overdub.sh >/dev/null 2>&1; then
  script_changed=no
fi

adb shell 'su -c "
  mkdir -p /data/local/bin
  cp /data/local/tmp/overdub /data/local/bin/overdub.new
  chmod 755 /data/local/bin/overdub.new
  mv -f /data/local/bin/overdub.new /data/local/bin/overdub
  cp /data/local/tmp/s.sh      /sbin/.core/img/.core/service.d/overdub.sh
  chmod 755 /sbin/.core/img/.core/service.d/overdub.sh
  rm -f /data/local/tmp/overdub /data/local/tmp/s.sh
  ls -l /data/local/bin/overdub /sbin/.core/img/.core/service.d/overdub.sh
"'
if ! adb shell 'su -c "cat /sbin/.core/img/.core/service.d/overdub.sh"' 2>/dev/null |
   tr -d "\r" | diff -q - deploy/overdub.sh >/dev/null 2>&1; then
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

if [ -n "$old_pid" ] && [ "$script_changed" = yes ]; then
  echo
  echo "REBOOT REQUIRED: deploy/overdub.sh changed. The supervisor is a shell"
  echo "loop holding the script it was started with, so the daemon that just came"
  echo "back is running under the OLD one. Only a reboot picks up the new."
fi
