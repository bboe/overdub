#!/usr/bin/env python3
"""Remove overdub from a rooted Echo Dot (2nd Generation), over adb, and give the
action button back to Alexa. Set ANDROID_SERIAL to pick one of several attached
or connected devices. It needs Python 3.9 or later and adb. docs/deployment.md
says why each step is in the order it is.
"""

from __future__ import annotations

import argparse
import re
import shlex
import shutil
import subprocess
import sys
import textwrap
import time
from typing import NoReturn, TextIO

ADBKEY = "/data/local/bin/adb_keys"
ADBKEYS = "/data/misc/adb/adb_keys"
API_PORT = 6053
APPLIED = "/data/local/bin/.overdub-applied"
BIN = "/data/local/bin/overdub"
BOOT = "/sbin/.core/img/.core/service.d/overdub.sh"
KEY = "/data/local/bin/.overdub-noise-key"
LABEL = 14
INDENT = " " * (LABEL + 4)
LOG = "/data/local/tmp/overdub.log"
MAP = "/data/local/map"
SENDFLAG = "persist.overdub.sendspin"
SENDKEY = "/data/local/bin/.overdub-sendspin-key"
SENDSPIN_PORT = 8928
STAGE = "/data/local/tmp/overdub-install"
STATE = argparse.Namespace(changed=False, pending=False, warned=False)
# The authority the daemon mints always has the same subject, so Android always
# reads it under the same name. internal/avs pins this with a test.
VOICECA = "/system/etc/security/cacerts/4c55d173.0"
VOICEID = "/data/local/bin/.overdub-avs-identity"
SWEPT = [
    BOOT,
    BIN,
    BIN + ".new",
    KEY,
    SENDKEY,
    VOICEID,
    ADBKEY,
    APPLIED,
    STAGE,
    MAP,
    LOG,
]


def adb(*args: str) -> tuple[int, str]:
    result = subprocess.run(
        ["adb", *args],
        check=False,
        errors="replace",
        stderr=subprocess.STDOUT,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        text=True,
    )
    return result.returncode, result.stdout.replace("\r", "")


def fail(label: str, *lines: str) -> NoReturn:
    show(*lines, label=label, sign=mark("fail"), stream=sys.stderr)
    sys.exit(1)


def main() -> None:  # ruff: ignore[complex-structure, too-many-branches, too-many-locals, too-many-statements]
    parser = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter
    )
    parser.parse_args()
    if not shutil.which("adb"):
        print(
            "uninstall.py: adb not found: install Android platform-tools",
            file=sys.stderr,
        )
        sys.exit(2)

    adb("start-server")
    code, out = adb("get-serialno")
    lines = [line for line in out.strip().split("\n") if line]
    if code != 0 or len(lines) != 1 or lines[0] == "unknown":
        fail(
            "adb",
            "No single device to uninstall from:",
            *lines,
            "Set ANDROID_SERIAL to pick one.",
        )
    print(f"Removing overdub from {lines[0]}.")
    print()
    if not re.search(r"uid=0\b", su("id")[1]):
        fail("root", "su -c id did not report uid=0.")
    ok("root", "su works")

    before = present()
    if before is None:
        fail("files", "Could not read the device, so nothing was changed.")
    pid = overdub_pid()
    if pid is None:
        fail("daemon", "adb went away before the kill.")
    STATE.changed = bool(before or pid)

    su(f"rm -f {BOOT}")
    su(
        f"rm -f {BIN} {BIN}.new {KEY} {SENDKEY} {SENDKEY}.new-* {ADBKEY} {APPLIED}\n"
        f"rm -f {VOICEID}\n"
        f"rm -rf {STAGE} {MAP}\n"
        "rm -f /data/local/tmp/overdub /data/local/tmp/s.sh"
    )

    if pid:
        pending(detail=f"stopping pid {pid}", label="daemon")
        su(f"kill {pid}")
        time.sleep(8)

    su(f"rm -f {LOG}\nrmdir /data/local/bin 2>/dev/null\ntrue")
    if STATE.changed:
        pending(detail="waiting for the supervisor's next cycle", label="files")
        time.sleep(6)

    left = present()
    if left is None:
        fail(
            "files",
            "Could not read the device back, so nothing here is confirmed removed."
            " Run this again with the Dot connected.",
        )

    for path in left:
        if path == LOG:
            show(
                f"STILL SUPERVISED: {LOG} came back after it was removed, so the"
                " service.d loop is still running. ps shows it as bare sh rather than"
                " the script it runs, so a reboot is what ends it.",
                label="supervisor",
                sign=mark("fail"),
                stream=sys.stderr,
            )
        else:
            show(
                f"STILL PRESENT: {path}",
                label="files",
                sign=mark("fail"),
                stream=sys.stderr,
            )
    if not left:
        ok(
            "files",
            "removed " + ", ".join(path.rsplit("/", 1)[1] for path in before)
            if before
            else "none installed",
        )

    still = overdub_pid()
    if still is None:
        fail("daemon", "adb went away during the check.")
    if still:
        if left:
            reason = (
                "Things above are still on the device, so it will start again, at the"
                " next respawn or the next boot. Fix those and run this again."
            )
        else:
            reason = (
                "The kill did not take, but nothing starts it again: the boot script"
                " and the binary are both gone, so a reboot is the end of it."
            )
        show(
            f"STILL RUNNING as pid {still}. {reason}",
            label="daemon",
            sign=mark("fail"),
            stream=sys.stderr,
        )
    else:
        ok("daemon", "stopped" if pid else "was not running")
    redirect = su(
        "for name in persist.amazon.scl.host persist.amazon.scl.port; do\n"
        '  [ -n "$(getprop $name)" ] || continue\n'
        '  setprop "$name" ""\n'
        '  rm -f "/data/property/$name"\n'
        "  value=$(getprop $name)\n"
        '  [ -z "$value" ] && echo released || echo "kept $name=$value"\n'
        "done\n"
        "echo checked"
    )[1].split("\n")
    kept = [line for line in redirect if line.startswith("kept ")]
    if "checked" not in redirect:
        warn(
            "voice",
            "Could not read persist.amazon.scl.host back. If it still points at"
            " 127.0.0.1, Alexa cannot reach Amazon and will not answer.",
        )
    elif kept:
        warn(
            "voice",
            f"{kept[0][len('kept ') :]} did not clear. Alexa sends her session"
            " there and nothing listens, so she cannot answer at all until it"
            " is cleared by hand or the Dot is reset.",
        )
    elif "released" in redirect:
        STATE.changed = True
        ok("voice", "Alexa points at Amazon again")

    removed = su(
        f"[ -e {VOICECA} ] || echo absent\n"
        "mount -o rw,remount /system\n"
        f"rm -f {VOICECA}\n"
        "mount -o ro,remount /system\n"
        f"[ -e {VOICECA} ] && echo kept\n"
        "echo checked"
    )[1].split("\n")
    if "checked" not in removed:
        warn("voice", f"Could not read {VOICECA} back, so it may still be in place.")
    elif "kept" in removed:
        warn(
            "voice",
            f"{VOICECA} is still in place. The daemon added it, and it stays"
            " until removed.",
        )
    elif "absent" not in removed:
        STATE.changed = True
        ok("voice", f"removed {VOICECA}")

    # Alexa's app reads where to send its session when it starts, and nothing
    # brings it back by itself: its state machine parks in DisconnectState and
    # answers every wake word with "I'm having trouble understanding".
    woken = su(
        "for p in /proc/[0-9]*; do\n"
        '  [ "$(cat $p/cmdline 2>/dev/null)" = "amazon.speech.sim" ] || continue\n'
        '  kill -9 "${p##*/}" && echo restarted\n'
        "done\n"
        "echo checked"
    )[1].split("\n")
    if "checked" not in redirect:
        pass
    elif "restarted" in woken:
        STATE.changed = True
        ok("voice", "restarted Alexa's app so she talks to Amazon again")
    elif "checked" in woken:
        ok("voice", "Alexa's app was not running")
    else:
        warn(
            "voice",
            "Could not restart Alexa's app. It still points at a relay that is"
            " gone, so she cannot answer until the Dot is rebooted.",
        )

    if left or still:
        print(file=sys.stderr)
        print("Uninstall incomplete.", file=sys.stderr)
        sys.exit(1)

    closed = []
    for port in (API_PORT, SENDSPIN_PORT):
        rule = f"-i wlan0 -p tcp --dport {port} -j ACCEPT"
        deleted = su(
            f"while iptables -w -C INPUT {rule} 2>/dev/null; do\n"
            f"  iptables -w -D INPUT {rule} || break\n"
            "  echo deleted\n"
            "done"
        )[1].split("\n")
        answer = su(f"iptables -L INPUT -n | grep {port}; echo checked")[1].split("\n")
        found = [line for line in answer if f"dpt:{port}" in line]
        if "checked" not in answer:
            found = ["could not read the chain back"]
        if found:
            warn(
                "firewall",
                f"The tcp/{port} rule is still in the INPUT chain:",
                *[f"  {line}" for line in found],
                "Nothing listens behind it now. It lives in the chain rather than on"
                " disk, so a reboot clears it.",
            )
        elif "deleted" in deleted:
            closed.append(f"tcp/{port}")
        STATE.changed |= "deleted" in deleted
    if closed:
        ok("firewall", " and ".join(closed) + " closed")
    elif not STATE.warned:
        ok("firewall", "no overdub rules open")

    cleared = su(
        "for f in /data/property/persist.overdub.*; do\n"
        '  [ -e "$f" ] || continue\n'
        '  setprop "${f##*/}" ""\n'
        '  rm -f "$f"\n'
        '  [ -e "$f" ] || echo cleared\n'
        "done"
    )[1].split("\n")
    STATE.changed |= "cleared" in cleared
    answer = su(f"getprop {SENDFLAG}; echo checked")[1].split("\n")
    value = [line for line in answer if line not in {"checked", ""}]
    if "checked" not in answer:
        warn("settings", f"Could not read {SENDFLAG} back; it may still be set.")
    elif value:
        warn(
            "settings",
            f"{SENDFLAG} is still set to {value[0]}. It only decides whether a"
            " future install starts Sendspin switched on.",
        )
    else:
        ok(
            "settings",
            "persist.overdub.* cleared" if "cleared" in cleared else "none set",
        )

    if not STATE.changed:
        print()
        print(
            "Nothing removed; see the warning above."
            if STATE.warned
            else "Already uninstalled; nothing to remove."
        )
        return

    for paragraph in (
        (
            "Home Assistant can no longer talk to this device: the API key it was"
            " configured with is gone. Installing again generates a NEW key and"
            " prints it once. Give that to the ESPHome integration, which asks for"
            " it when the handshake fails."
        ),
        (
            "Reboot to finish. Whatever Network ADB was last set to is still in"
            " force: it lives in the property store and the firewall chain rather"
            " than on disk. If it was Insecure, tcp/5555 is an unauthenticated root"
            " shell until you reboot."
        ),
        (
            f"One file is left on purpose: the public key at {ADBKEYS}. adbd"
            " consults it only while ro.adb.secure is 1, and nothing sets that once"
            " this is gone, so it grants nothing. Removing it would take away the"
            " half that grants access and leave the half that denies it."
        ),
    ):
        print()
        print(textwrap.fill(paragraph, 79))
    print()
    print("Overdub uninstalled. The action button belongs to Alexa again.")


def mark(kind: str) -> str:
    marks = {"fail": "❌", "ok": "✅", "warn": "❗"}
    try:
        marks[kind].encode(sys.stdout.encoding or "ascii")
        return marks[kind]
    except UnicodeEncodeError:
        return {"fail": "XX", "ok": "OK", "warn": "!!"}[kind]


def ok(label: str, *lines: str) -> None:
    show(*lines, label=label, sign=mark("ok"))


def overdub_pid() -> str | None:
    code, listing = su("ps")
    if code != 0:
        return None
    for line in listing.split("\n"):
        fields = line.split()
        if len(fields) > 1 and fields[-1].endswith("bin/overdub"):
            return fields[1]
    return ""


def pending(*, detail: str, label: str) -> None:
    if STATE.pending:
        sys.stdout.write("\r" + " " * 79 + "\r")
    if sys.stdout.isatty():
        print(f"   {label:<{LABEL}} {detail} ...", end="", flush=True)
        STATE.pending = True


def present() -> list[str] | None:
    answer = su(
        f"for path in {' '.join(SWEPT)} {SENDKEY}.new-*; do\n"
        '  [ -e "$path" ] && echo "$path"\n'
        "done\n"
        "echo swept"
    )[1].split("\n")
    if "swept" not in answer:
        return None
    return [
        line for line in answer if line in SWEPT or line.startswith(SENDKEY + ".new-")
    ]


def show(*lines: str, label: str, sign: str, stream: TextIO = sys.stdout) -> None:
    if STATE.pending:
        sys.stdout.write("\r" + " " * 79 + "\r")
        sys.stdout.flush()
        STATE.pending = False
    first = f"{sign} {label:<{LABEL}} "
    for line in lines or [""]:
        if line.startswith(" "):
            print(INDENT + line, file=stream, flush=True)
        else:
            print(
                textwrap.fill(line, 79, initial_indent=first, subsequent_indent=INDENT),
                file=stream,
                flush=True,
            )
        first = INDENT


def su(command: str) -> tuple[int, str]:
    return adb("shell", "su -c " + shlex.quote(command))


def warn(label: str, *lines: str) -> None:
    STATE.warned = True
    show(*lines, label=label, sign=mark("warn"))


if __name__ == "__main__":
    try:
        main()
    except KeyboardInterrupt:
        print(file=sys.stderr)
        sys.exit(130)
