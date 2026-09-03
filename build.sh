#!/bin/bash
set -e
cd "$(dirname "$0")"

# GOOS=android rather than linux, and cgo rather than none, because the chime is
# played through OpenSL ES. Go's linux runtime hangs before main() against
# Bionic, so the target is not a detail that can be got wrong quietly: it does
# not fail, it stops.
: "${ANDROID_NDK_HOME:=/opt/homebrew/share/android-ndk}"

# The prebuilt directory is a glob, and an NDK may carry more than one host --
# darwin-x86_64 beside darwin-arm64. Taking the first executable match beats
# letting the glob collapse into a space-joined string that names two paths and
# is not a program.
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

# Bionic keeps pthread inside libc and ships no libpthread, while cgo appends
# -lpthread unconditionally. Older NDKs carried empty stubs for exactly this;
# r29 does not, so make our own rather than patch the toolchain.
#
# Relative, and it has to stay that way: an absolute path here lands in cgo's
# action hash, so the same commit built from two directories would produce two
# different binaries and quietly break what docs/pitfalls.md promises. The cd
# above is what makes a relative path resolve.
stubs=build/stublibs
mkdir -p "$stubs"
"$AR" rcs "$stubs/libpthread.a"
"$AR" rcs "$stubs/librt.a"

export CC CGO_ENABLED=1 CGO_LDFLAGS="-L$stubs" GOOS=android GOARCH=arm GOARM=7

# The only thing that type-checks the cgo half at all. CI vets and tests for
# linux/arm, where internal/audio is the stub, so without this the player and
# its C are compiled by the build and read by nobody.
go vet ./...

go build -trimpath -buildvcs=false -ldflags="-s -w" -o build/overdub .
ls -l build/overdub
file build/overdub 2>/dev/null || true
