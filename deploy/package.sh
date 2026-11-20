#!/bin/bash
# Usage: deploy/package.sh <tag>
#
# Packages build/overdub as the release for <tag>: the Fire OS 6 tarball, the
# Fire OS 5 decoy, release.json and SHA256SUMS, written to the current
# directory, then checks what the tarball carries. Needs GNU tar; CI runs it.
set -euo pipefail

tag=$1
dir="overdub-$tag"
mtime="@$(git log -1 --pretty=%ct)"

mkdir -p "$dir/build" "$dir/deploy"
cp README.md LICENSE.txt THIRD-PARTY.txt "$dir/"
cp -r docs "$dir/docs"
cp deploy/install.py deploy/uninstall.py deploy/overdub.sh deploy/overdub.rc "$dir/deploy/"
cp build/overdub "$dir/build/overdub"
tar --sort=name --owner=0 --group=0 --numeric-owner --mtime="$mtime" -cf - "$dir" |
  gzip -n > "$dir-fireos6.tar.gz"

decoy=$(mktemp -d)
mkdir -p "$decoy/$dir/build" "$decoy/$dir/deploy/mapdump"
cp deploy/fireos5-decoy/README.md "$decoy/$dir/README.md"
cp deploy/fireos5-decoy/overdub.sh "$decoy/$dir/deploy/overdub.sh"
cp deploy/fireos5-decoy/placeholder "$decoy/$dir/build/overdub"
cp deploy/fireos5-decoy/placeholder "$decoy/$dir/deploy/mapdump/mapdump.jar"
tar --sort=name --owner=0 --group=0 --numeric-owner --mtime="$mtime" -C "$decoy" -cf - "$dir" |
  gzip -n > "$dir.tar.gz"

cp deploy/release.json release.json
sha256sum "$dir-fireos6.tar.gz" "$dir.tar.gz" release.json > SHA256SUMS
cat SHA256SUMS

names=$(tar tzf "$dir-fireos6.tar.gz")
listing=$(tar tvzf "$dir-fireos6.tar.gz")
for f in LICENSE.txt THIRD-PARTY.txt build/overdub deploy/overdub.rc; do
  grep -qFx "$dir/$f" <<<"$names" ||
    { echo "not in the tarball: $f" >&2; exit 1; }
done
for f in install.py uninstall.py overdub.sh; do
  awk -v p="$dir/deploy/$f" '$1 == "-rwxr-xr-x" && $NF == p { ok = 1 } END { exit !ok }' <<<"$listing" ||
    { echo "not in the tarball, or not executable: deploy/$f" >&2; exit 1; }
done
