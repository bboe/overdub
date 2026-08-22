#!/system/bin/sh
# Magisk late_start service: start overdub, and restart it if it dies.

BIN=/data/local/bin/overdub
LOG=/data/local/tmp/overdub.log

: > "$LOG"

if [ ! -x "$BIN" ]; then
  echo "overdub: $BIN is missing or not executable; nothing to start" >> "$LOG"
  exit 0
fi

(
  restarts=0
  while [ -x "$BIN" ]; do
    "$BIN" >>"$LOG" 2>&1
    restarts=$((restarts + 1))
    if [ "$restarts" -ge 20 ]; then
      : > "$LOG"
      echo "overdub: 20 restarts; truncating the log and carrying on" >> "$LOG"
      restarts=0
    fi
    sleep 5
  done
) &
