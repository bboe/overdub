#!/usr/bin/env python3
"""dot_firmware.py moved to firebreak, at https://github.com/bboe/firebreak.

Run firebreak instead. It takes these targets in place of the old ones:

  uvx firebreak@latest                              # was v1-bboe, the default
  uvx firebreak@latest amonet-biscuit-v1.1.0-bboe   # was v1-bboe
  uvx firebreak@latest amonet-biscuit-v1.1.0        # was v1
  uvx firebreak@latest amonet-biscuit-v2.0.0        # was v2
  uvx firebreak@latest stock BUILD                  # was stock BUILD

Without uv, download firebreak.pyz from
https://github.com/bboe/firebreak/releases and run python3 firebreak.pyz with
the same arguments. The flags are the same.

This file still runs: it translates its arguments and runs firebreak, with uvx
when it is on PATH, otherwise from firebreak.pyz, which it downloads into
firebreak's cache. When the download fails, it runs the copy already there.
It needs Python 3.9 or later.
"""

from __future__ import annotations

import contextlib
import http.client
import os
import pathlib
import shlex
import shutil
import subprocess
import sys
import tempfile
import urllib.request
import zipfile

PYZ_URL = "https://github.com/bboe/firebreak/releases/latest/download/firebreak.pyz"
TARGETS = {
    "v1": "amonet-biscuit-v1.1.0",
    "v1-bboe": "amonet-biscuit-v1.1.0-bboe",
    "v2": "amonet-biscuit-v2.0.0",
}
URL = "https://github.com/bboe/firebreak"


def cache_dir() -> pathlib.Path:
    if os.name == "nt":
        base = os.environ.get("LOCALAPPDATA") or pathlib.Path.home()
    else:
        base = os.environ.get("XDG_CACHE_HOME") or pathlib.Path.home() / ".cache"
    return pathlib.Path(base) / "firebreak"


def download() -> pathlib.Path | None:
    path = cache_dir() / "firebreak.pyz"
    try:
        path.parent.mkdir(exist_ok=True, parents=True)
        error = fetch(path)
    except (OSError, http.client.HTTPException) as caught:
        error = repr(caught)
    if not error:
        return path
    print(f"dot_firmware.py: {PYZ_URL}: {error}", file=sys.stderr)
    if not runnable(path):
        return None
    print(f"dot_firmware.py: the download failed; running {path}", file=sys.stderr)
    return path


def fetch(path: pathlib.Path) -> str:
    with tempfile.NamedTemporaryFile(
        delete=False, dir=path.parent, prefix="firebreak.pyz.", suffix=".part"
    ) as file:
        part = pathlib.Path(file.name)
    try:
        with urllib.request.urlopen(PYZ_URL, timeout=60) as response:
            part.write_bytes(response.read())
        if not runnable(part):
            return "not a zip with __main__.py"
        part.replace(path)
        return ""
    finally:
        with contextlib.suppress(OSError):
            part.unlink(missing_ok=True)


def main() -> int:
    arguments = translate(sys.argv[1:])
    hint = shlex.join(["uvx", "firebreak@latest", *arguments])
    print(f"{__doc__}\nUpdate scripts and docs to run:\n  {hint}\n", file=sys.stderr)
    if os.name != "nt" and os.geteuid() == 0:
        print(
            "dot_firmware.py: run this as your own user, not as root or with sudo:"
            " firebreak refuses root, and its downloads would belong to root",
            file=sys.stderr,
        )
        return 1
    uvx = shutil.which("uvx")
    if uvx:
        return run([uvx, "firebreak@latest", *arguments])
    pyz = download()
    if not pyz:
        print(
            "dot_firmware.py: no uvx, and firebreak.pyz did not download. Install uv"
            f" ({URL}#run-it) and run:\n  {hint}\nor download {PYZ_URL} and"
            f" run:\n  {shlex.join(['python3', 'firebreak.pyz', *arguments])}",
            file=sys.stderr,
        )
        return 1
    status = run([sys.executable, str(pyz), *arguments])
    if status:
        words = ["py", "-3"] if os.name == "nt" else ["python3"]
        words += [str(pyz), *arguments]
        join = subprocess.list2cmdline if os.name == "nt" else shlex.join
        print(
            f"dot_firmware.py: this ran {join(words)}; where firebreak's advice"
            " says firebreak.pyz, use that path",
            file=sys.stderr,
        )
    return status


def run(command: list[str]) -> int:
    child = subprocess.Popen(command)
    while True:
        try:
            status = child.wait()
        except KeyboardInterrupt:
            continue
        return 128 - status if status < 0 else status


def runnable(path: pathlib.Path) -> bool:
    try:
        with zipfile.ZipFile(path) as archive:
            return "__main__.py" in archive.namelist()
    except (OSError, zipfile.BadZipFile):
        return False


def translate(arguments: list[str]) -> list[str]:
    for index, argument in enumerate(arguments):
        if not argument.startswith("-"):
            return [
                *arguments[:index],
                TARGETS.get(argument, argument),
                *arguments[index + 1 :],
            ]
    return arguments


if __name__ == "__main__":
    sys.exit(main())
