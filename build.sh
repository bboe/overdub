#!/bin/bash
set -e
cd "$(dirname "$0")"
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 \
  go build -trimpath -buildvcs=false -ldflags="-s -w" -o build/overdub .
ls -l build/overdub
file build/overdub 2>/dev/null || true
