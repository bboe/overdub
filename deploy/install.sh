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
keyfile=""
staged=0
# Set while the key is on the device and not yet shown. An interrupt in that
# window would otherwise leave a live key nobody has seen: the local copy goes
# with the trap, and the next install finds the device's and keeps it.
key_landed=0
# The device half matters as much as the host half: an interrupt between the
# push and the move leaves a second live copy of the key on the device.
cleanup() {
  rm -f "$boot_script" "$keyfile"
  if [ "$key_landed" = 1 ]; then
    echo >&2
    echo "Interrupted after the key was written and before it was shown." >&2
    discard_new_key
  fi
  if [ "$staged" = 1 ]; then
    adb shell 'su -c "rm -rf /data/local/tmp/overdub-install"' >/dev/null 2>&1 || true
    # Said rather than assumed: a staged copy left behind is the key at 0666.
    if [ -z "$(adb shell 'su -c "[ -e /data/local/tmp/overdub-install ] || echo gone"' 2>/dev/null |
         tr -d "\r" | grep -x gone || true)" ]; then
      echo "WARNING: could not confirm the staged key is gone from" >&2
      echo "  /data/local/tmp/overdub-install -- remove it once the Dot is back:" >&2
      echo "  adb shell 'su -c \"rm -rf /data/local/tmp/overdub-install\"'" >&2
    fi
  fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

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

# The key as the device has it, matched by shape rather than taken whole.
device_key() {
  # The sentinel separates "the file says this" from "the device did not answer";
  # the advice on a bad key is to delete it, so the two must not be confused.
  answer=$(adb shell 'su -c "cat /data/local/bin/.overdub-noise-key; echo --read-ok--"' | tr -d "\r")
  printf '%s\n' "$answer" | grep -qx -- '--read-ok--' || return 1
  body=$(printf '%s\n' "$answer" | grep -vx -- '--read-ok--' || true)

  found=$(printf '%s\n' "$body" |
    sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' |
    grep -Ex '[A-Za-z0-9+/]{43}=' || true)
  # Stripped because the daemon trims the file before decoding it, so a key with
  # a stray space is one it starts with.
  if [ "$(printf '%s\n' "$found" | grep -Ecx '[A-Za-z0-9+/]{43}=')" = 1 ]; then
    printf '%s\n' "$found"
    return 0
  fi
  # Go's base64 skips newlines, so a key wrapped across lines is one the daemon
  # starts with; two copies join into 88 characters and are refused, as it
  # refuses them. Neither look can tell a stray line from su apart from a stray
  # line in the file, so a key file carrying a comment passes here and the daemon
  # rejects it, loudly and with a message README quotes.
  printf '%s' "$body" | tr -d "[:space:]" | grep -Ex '[A-Za-z0-9+/]{43}=' || true
}

# The device's answer to a test, or a failure if it did not give one: silence and
# "no" must not read alike.
probe() {
  adb shell "su -c '[ $1 ] && echo yes || echo no'" | tr -d "\r" |
    grep -m1 -x -e yes -e no || return 1
}

# Repaired rather than only reported, and on both paths: the mode is what keeps
# the key from every other uid, and one that arrived another way has never had it
# set.
check_key_mode() {
  adb shell 'su -c "chmod 600 /data/local/bin/.overdub-noise-key"'
  key_mode=$(adb shell 'su -c "ls -l /data/local/bin/.overdub-noise-key"' |
    tr -d "\r" | grep -E '^-' | head -1 | awk '{print $1}')
  case "$key_mode" in
    -rw-------) ;;
    *) echo "INSTALL FAILED: the API key is ${key_mode:-nothing}, want -rw-------." >&2
       return 1 ;;
  esac
}

# A key this script wrote and could not verify has to go, not stay: it was never
# printed, and the next run would find it and keep it, leaving the Dot serving an
# API under a key that exists nowhere. Only ever called on the generate branch.
discard_new_key() {
  adb shell 'su -c "rm -f /data/local/bin/.overdub-noise-key"' >/dev/null 2>&1 || true
  # Reported as it really went. This runs because something already failed, and
  # the usual reason is a device that stopped answering, which is the same reason
  # the rm cannot land.
  if [ -n "$(adb shell 'su -c "[ -f /data/local/bin/.overdub-noise-key ] || echo gone"' |
       tr -d "\r" | grep -x gone || true)" ]; then
    echo "The unverified key was removed. Run install.sh again to generate one." >&2
    return 0
  fi
  echo "COULD NOT REMOVE the key just written, and it was never printed." >&2
  echo "Nothing can connect with it. Remove it by hand before installing again:" >&2
  echo "  adb shell 'su -c \"rm -f /data/local/bin/.overdub-noise-key\"'" >&2
}

# Before anything is pushed, so a device whose key cannot be settled keeps the
# binary and boot script it already had, and "Nothing was changed" is true.

# Only when there is none: a device that already has a key keeps it, so
# reinstalling does not lock Home Assistant out of a Dot it was talking to.
# This is the only check in either script whose failure is destructive, so it
# demands one of two definite answers and treats anything else as fatal.
key_present=$(adb shell 'su -c "if [ -f /data/local/bin/.overdub-noise-key ]; then echo yes; else echo no; fi"' |
  tr -d "\r" | grep -Ex 'yes|no' || true)
case "$key_present" in
  yes|no) ;;
  *) echo "INSTALL FAILED: could not tell whether the device already has a key." >&2
     echo "Nothing was changed. Check that adb and su still work, and try again." >&2
     exit 1 ;;
esac

if [ "$key_present" = yes ]; then
  # A present but unreadable key leaves the daemon refusing to start, and the
  # check above would keep it through any number of reinstalls.
  if ! existing=$(device_key); then
    echo "INSTALL FAILED: could not read the key off the device." >&2
    echo "Nothing was changed. Check that adb and su still work, and try again." >&2
    exit 1
  fi
  if [ -z "$existing" ]; then
    echo "INSTALL FAILED: the key already on the device is not 32 bytes of base64." >&2
    echo "The daemon will not start with it, and an install keeps an existing key." >&2
    echo "Nothing was changed." >&2
    echo "Delete it and install again to get a new one, then give that to Home Assistant:" >&2
    echo "  adb shell 'su -c \"rm -f /data/local/bin/.overdub-noise-key\"'" >&2
    exit 1
  fi
  check_key_mode || exit 1
  echo "API key already on device, kept"
else
  keyfile=$(mktemp)
  head -c 32 /dev/urandom | base64 > "$keyfile"
  # Measured before it goes anywhere: head is the left half of a pipe, so a
  # failed read leaves an empty file and a zero status, and every check after
  # this one compares that file against itself.
  if ! grep -Exq '[A-Za-z0-9+/]{43}=' "$keyfile"; then
    echo "INSTALL FAILED: could not generate a 32-byte key from /dev/urandom." >&2
    echo "Nothing was changed." >&2
    exit 1
  fi
  # 0700 and not /data/local/tmp, which any uid can walk into; without su, so
  # shell still owns it and adb push can write; and mkdir without -p, so an
  # existing one fails here and this doubles as the lock against a second
  # install. docs/pitfalls.md has the measurement.
  if ! adb shell 'mkdir /data/local/tmp/overdub-install 2>/dev/null && chmod 700 /data/local/tmp/overdub-install && echo made' |
     tr -d "\r" | grep -qx made; then
    echo "INSTALL FAILED: /data/local/tmp/overdub-install already exists, so either" >&2
    echo "  another install is running or one was interrupted. Nothing was changed." >&2
    echo "  If nothing else is running, remove it and try again:" >&2
    echo "  adb shell 'su -c \"rm -rf /data/local/tmp/overdub-install\"'" >&2
    exit 1
  fi
  staged=1
  # Read back before the key goes in: the mode is the whole point of the
  # directory, and nothing downstream would notice its absence.
  stage_mode=$(adb shell 'ls -ld /data/local/tmp/overdub-install' |
    tr -d "\r" | grep -E '^d' | head -1 | awk '{print $1}')
  case "$stage_mode" in
    drwx------) ;;
    *) echo "INSTALL FAILED: the key staging directory is ${stage_mode:-missing}, want drwx------." >&2
       echo "The key was not pushed. Nothing was changed." >&2
       exit 1 ;;
  esac
  adb push "$keyfile" /data/local/tmp/overdub-install/k.txt
  # Armed before the write, not after: an interrupt arrives during the call, so
  # a flag set on the next line is one that never runs.
  key_landed=1
  adb shell 'su -c "
    umask 077
    mkdir -p /data/local/bin
    cp /data/local/tmp/overdub-install/k.txt /data/local/bin/.overdub-noise-key
    chmod 600 /data/local/bin/.overdub-noise-key
    rm -rf /data/local/tmp/overdub-install
  "'
  # A staging copy that outlived the rm is the live key at 0666, and the flag
  # below is what tells the trap to try again, so it is not cleared on faith.
  if [ -z "$(adb shell 'su -c "[ -e /data/local/tmp/overdub-install ] || echo gone"' |
       tr -d "\r" | grep -x gone || true)" ]; then
    echo "INSTALL FAILED: could not confirm the staged key is gone from" >&2
    echo "  /data/local/tmp/overdub-install -- treat it as still there." >&2
    discard_new_key
    exit 1
  fi
  staged=0
  if ! landed=$(device_key) || [ "$landed" != "$(cat "$keyfile")" ]; then
    echo "INSTALL FAILED: the API key did not land on the device." >&2
    discard_new_key
    exit 1
  fi
  if ! check_key_mode; then
    discard_new_key
    exit 1
  fi
  echo "API key verified on device"

  echo
  echo "Generated an API encryption key. Paste it into Home Assistant's"
  echo "ESPHome integration. The installer keeps no copy:"
  echo
  echo "    $(cat "$keyfile")"
  key_landed=0
  echo
  echo "The API is ENCRYPTED from the next start. Home Assistant must be"
  echo "configured with this key, or it cannot connect at all."
  echo
  rm -f "$keyfile"
  keyfile=""
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
installed_md5=$(adb shell 'su -c "md5 /data/local/bin/overdub"' | tr -d "\r" |
  grep -Eo '^[0-9a-f]{32}' | head -1)
if [ "${#installed_md5}" != 32 ]; then
  echo "INSTALL FAILED: the device did not hash the binary: ${installed_md5:-no output}" >&2
  exit 1
fi
if [ "$built_md5" != "$installed_md5" ]; then
  echo "INSTALL FAILED: device has $installed_md5, built $built_md5" >&2
  exit 1
fi
if ! answer=$(probe "-x /data/local/bin/overdub"); then
  echo "INSTALL FAILED: the device did not say whether the binary is executable." >&2
  exit 1
elif [ "$answer" != yes ]; then
  echo "INSTALL FAILED: the binary landed, but is not executable." >&2
  exit 1
fi
echo "binary verified on device ($built_md5)"

# By hash rather than by diffing the stream, so a warning line from su cannot
# fail this and blame the Magisk layout for it.
boot_md5=$(local_md5 "$boot_script")
installed_boot_md5=$(adb shell 'su -c "md5 /sbin/.core/img/.core/service.d/overdub.sh"' |
  tr -d "\r" | grep -Eo '^[0-9a-f]{32}' | head -1)
if [ "$installed_boot_md5" != "$boot_md5" ]; then
  echo "INSTALL FAILED: the boot script did not land in service.d" >&2
  echo "  (device has ${installed_boot_md5:-no hash}, pushed $boot_md5)." >&2
  echo "  This device's Magisk 17.3 keeps service.d inside magisk.img, at the" >&2
  echo "  /sbin path above; later Magisk dropped the image for /data/adb/service.d." >&2
  echo "  Check which this one runs: adb shell su -c 'magisk -v'" >&2
  exit 1
fi
if ! answer=$(probe "-x /sbin/.core/img/.core/service.d/overdub.sh"); then
  echo "INSTALL FAILED: the device did not say whether the boot script is executable." >&2
  exit 1
elif [ "$answer" != yes ]; then
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
