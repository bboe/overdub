#!/bin/bash
# Every Amazon identifier the code resolves at runtime must still look like an
# identifier.
set -e
cd "$(dirname "$0")/.."

bad=0
found=0
while IFS= read -r hit; do
  found=$((found + 1))
  file=${hit%%:*}
  literal=${hit#*:}
  if ! [[ $literal =~ ^[A-Za-z_$][A-Za-z0-9_$]*(\.[A-Za-z_$][A-Za-z0-9_$]*)*(/[A-Za-z_$][A-Za-z0-9_$]*(\.[A-Za-z_$][A-Za-z0-9_$]*)*)?\.?$ ]]; then
    echo "malformed Amazon identifier in $file: \"$literal\"" >&2
    bad=1
  fi
done < <(
  git ls-files '*.go' '*.java' |
  while read -r source_file; do
    grep -oE '"[^"]*"' "$source_file" |
      sed 's/^"//; s/"$//' |
      grep -E '^(com\.amazon\.|amazon\.speech\.)' |
      sed "s|^|$source_file:|"
  done
)

if [ "$found" -lt 4 ]; then
  echo "found only $found Amazon identifiers, expected at least 4:" >&2
  echo "  the extraction above is broken, not the identifiers." >&2
  exit 1
fi

for method in X v getAccounts; do
  if ! grep -qF "getMethod(\"$method\"" deploy/mapdump/MapDump.java; then
    echo "MapDump no longer calls getMethod(\"$method\")" >&2
    bad=1
  fi
done

[ "$bad" -eq 0 ] && echo "Amazon identifiers and MapDump method names look right"
exit "$bad"
