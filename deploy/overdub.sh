#!/system/bin/sh
# Run by init through deploy/overdub.rc: start overdub, and restart it if it
# dies. While an update is on trial, a new binary that exits 3 times is replaced
# by the one it updated.

BIN=/data/local/bin/overdub
PREV=/data/local/bin/overdub.prev
TRIAL=/data/local/bin/.overdub-trial
FAILED=/data/local/bin/.overdub-failed
JAR=/data/local/map/mapdump.jar
LOG=/data/local/tmp/overdub.log

NAME=

: > "$LOG"

if [ ! -x "$BIN" ]; then
  echo "overdub: $BIN is missing or not executable; nothing to start" >> "$LOG"
  exit 0
fi

if [ -z "$NAME" ]; then
  echo "overdub: NAME is unset in $0; refusing to start without a unique name" >> "$LOG"
  exit 0
fi

restarts=0
tries=0
tried=
while [ -x "$BIN" ]; do
  if [ -f "$TRIAL" ]; then
    trial=$(cat "$TRIAL")
    if [ "$trial" != "$tried" ]; then
      tried=$trial
      tries=0
    fi
    tries=$((tries + 1))
  else
    tried=
    tries=0
  fi
  if [ "$tries" -gt 3 ]; then
    if [ -x "$PREV" ]; then
      mv "$PREV" "$BIN"
      if [ -f "$JAR.prev" ]; then
        mv "$JAR.prev" "$JAR"
      fi
      echo "$tried" > "$FAILED"
      echo "overdub: the update to $tried exited 3 times; back to the binary it replaced" >> "$LOG"
    else
      rm -f "$JAR.prev"
      echo "overdub: the update to $tried exited 3 times, and $PREV is missing; keeping it" >> "$LOG"
    fi
    rm -f "$TRIAL"
    tried=
    tries=0
  fi
  OVERDUB_ROLLBACK=1 "$BIN" -name "$NAME" >>"$LOG" 2>&1
  restarts=$((restarts + 1))
  if [ "$restarts" -ge 20 ]; then
    : > "$LOG"
    echo "overdub: 20 restarts; truncating the log and carrying on" >> "$LOG"
    restarts=0
  fi
  sleep 5
done
