#!/bin/bash
set -e
cd "$(dirname "$0")"

: "${ANDROID_NDK_HOME:=/opt/homebrew/share/android-ndk}"

ndk_tool() {
  for candidate in "$ANDROID_NDK_HOME"/toolchains/llvm/prebuilt/*/bin/"$1"; do
    if [ -x "$candidate" ]; then
      printf '%s\n' "$candidate"
      return 0
    fi
  done
  return 1
}

no_ndk() {
  echo "no $1 under $ANDROID_NDK_HOME" >&2
  echo "Point ANDROID_NDK_HOME at an Android NDK. On macOS:" >&2
  echo "  brew install --cask android-ndk" >&2
  echo "Elsewhere, unpack a release from https://developer.android.com/ndk" >&2
  exit 1
}

CC=$(ndk_tool armv7a-linux-androideabi22-clang) || no_ndk armv7a-linux-androideabi22-clang
AR=$(ndk_tool llvm-ar) || no_ndk llvm-ar

stubs=build/stublibs
mkdir -p "$stubs"
"$AR" rcs "$stubs/libpthread.a"
"$AR" rcs "$stubs/librt.a"

export CC CGO_ENABLED=1 CGO_LDFLAGS="-L$stubs" GOOS=android GOARCH=arm GOARM=7

go vet ./...

ldflags="-s -w"
if [ -n "${OVERDUB_VERSION:-}" ]; then
  ldflags="$ldflags -X main.version=$OVERDUB_VERSION"
fi

go build -trimpath -buildvcs=false -ldflags="$ldflags" -o build/overdub .
ls -l build/overdub
file build/overdub 2>/dev/null || true
