#!/usr/bin/env python3
"""Remove overdub from a rooted Echo Dot (2nd Generation), over adb, and give the
action button back to Alexa. Set ANDROID_SERIAL to pick one of several attached
or connected devices. It needs Python 3.9 or later and adb. docs/deployment.md
says why each step is in the order it is.
"""

from __future__ import annotations

import argparse
import json
import re
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
BOOT = "/data/local/bin/overdub.sh"
FAILED = "/data/local/bin/.overdub-failed"
INIT = "/system/etc/init/overdub.rc"
KEY = "/data/local/bin/.overdub-noise-key"
LABEL = 14
INDENT = " " * (LABEL + 4)
LOG = "/data/local/tmp/overdub.log"
SDKCONFIG = "/system/etc/AlexaClientSDKConfig.json"
SENDFLAG = "persist.overdub.sendspin"
SENDKEY = "/data/local/bin/.overdub-sendspin-key"
SENDLAST = "/data/local/bin/.overdub-sendspin-last"
SENDPAIRS = "/data/local/bin/.overdub-sendspin-pairs"
SENDSPIN_PORT = 8928
STAGE = "/data/local/tmp/overdub-install"
STATE = argparse.Namespace(changed=False, pending=False, warned=False)
TEMPS = (SENDKEY + ".new-", SENDPAIRS + ".new-", SENDLAST + ".new-")
TRIAL = "/data/local/bin/.overdub-trial"
VOICECAS = (
    "/system/etc/security/cacerts/4c55d173.0",
    "/system/etc/security/cacerts-sha1/cbac1ab7.0",
)
VOICEID = "/data/local/bin/.overdub-avs-identity"
SWEPT = [
    INIT,
    BOOT,
    BOOT + ".new",
    BIN,
    BIN + ".new",
    BIN + ".prev",
    TRIAL,
    FAILED,
    KEY,
    SENDKEY,
    SENDPAIRS,
    SENDLAST,
    ADBKEY,
    ADBKEYS,
    APPLIED,
    VOICEID,
    *VOICECAS,
    STAGE,
    LOG,
]
VOICE_PORT = 8443
VOICEURL = f"https://127.0.0.1:{VOICE_PORT}"


def adb(*args: str) -> tuple[int, str]:
    result = subprocess.run(
        ["adb", *args],
        check=False,
        stderr=subprocess.STDOUT,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
    )
    return result.returncode, result.stdout.decode("utf-8", "replace").replace("\r", "")


def endpoint() -> str | None:
    answer = shell("lipc-get-prop -s -q com.amazon.puffin AVSEndpoint; echo checked")[1]
    *values, last = [line for line in answer.split("\n") if line] or [""]
    if last != "checked" or len(values) != 1 or not values[0].startswith("https://"):
        return None
    return values[0]


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
    if not re.search(r"uid=0\b", shell("id")[1]):
        fail(
            "root",
            "adb shell id did not report uid=0. overdub needs the root adb that"
            " firebreak's Fire OS 6 unlock leaves.",
        )
    ok("root", "adb runs as root")

    before = present()
    if before is None:
        fail("files", "Could not read the device, so nothing was changed.")
    pid = overdub_pid()
    if pid is None:
        fail("daemon", "adb went away before the kill.")
    STATE.changed = bool(before or pid)

    shell(
        f"mount -o rw,remount /\nrm -f {INIT} {' '.join(VOICECAS)}\n"
        "mount -o ro,remount /"
    )
    shell(
        f"rm -f {BOOT} {BOOT}.new {BIN} {BIN}.new {BIN}.prev {TRIAL} {FAILED} {KEY}"
        f" {SENDKEY} {SENDKEY}.new-* {SENDPAIRS} {SENDPAIRS}.new-* {SENDLAST}"
        f" {SENDLAST}.new-* {ADBKEY} {ADBKEYS} {APPLIED} {VOICEID}\n"
        f"rm -rf {STAGE}\n"
        "rm -f /data/local/tmp/overdub /data/local/tmp/s.sh /data/local/tmp/overdub.rc"
    )

    if pid:
        pending(detail=f"stopping pid {pid}", label="daemon")
        shell(f"kill {pid}")
        time.sleep(8)
    shell("stop overdub")
    release_voice()

    shell(f"rm -f {LOG}\nrmdir /data/local/bin 2>/dev/null\ntrue")
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
                " boot loop is still running. ps shows it as bare sh rather than"
                " the script it runs. adb shell 'stop overdub' ends it, and so does"
                " a reboot.",
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
    if not re.search(r"^\S+ / \S+ ro[,\s]", shell("cat /proc/mounts")[1], re.MULTILINE):
        warn(
            "files",
            "/ did not remount read-only after removing the init service. A reboot"
            " mounts it read-only again.",
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
    if left or still:
        print(file=sys.stderr)
        print("Uninstall incomplete.", file=sys.stderr)
        sys.exit(1)

    answer = shell(
        "live() {\n"
        "  for p in $(pidof $1); do\n"
        '    f=$(ls -l /proc/$p/fd 2>/dev/null | grep " /dev/input/event")\n'
        '    [ -n "$f" ] || return 1\n'
        '    echo "$f" | grep -q "(deleted)" && return 1\n'
        "  done\n"
        '  [ -n "$(pidof $1)" ]\n'
        "}\n"
        "for pair in acebuttond:acebuttond usagestat_protod:ace_usagestat_protod; do\n"
        "  s=${pair%%:*}; b=${pair#*:}\n"
        '  [ "$(getprop init.svc.$s)" = running ] || continue\n'
        "  live $b && continue\n"
        "  old=$(pidof $b)\n"
        "  setprop ctl.restart $s || { echo failed $s; continue; }\n"
        "  i=0\n"
        "  while [ $i -lt 50 ]; do\n"
        '    [ "$(pidof $b)" != "$old" ] && live $b && break\n'
        "    sleep 0.1; i=$((i + 1))\n"
        "  done\n"
        "  [ $i -lt 50 ] && echo restarted $s || echo failed $s\n"
        "done; echo checked"
    )[1].split("\n")
    restarted = [line.split()[1] for line in answer if line.startswith("restarted ")]
    failed = [line.split()[1] for line in answer if line.startswith("failed ")]
    if "checked" not in answer or failed:
        warn(
            "buttons",
            " and ".join(failed or ["The key readers"])
            + " did not come back onto the real keys. They may still read the clone"
            " the daemon removed, burning CPU, until a reboot.",
        )
    elif restarted:
        ok("buttons", " and ".join(restarted) + " restarted onto the real keys")
    else:
        ok("buttons", "no key reader held the removed clone")

    closed = []
    for port in (API_PORT, SENDSPIN_PORT):
        rule = f"-i wlan0 -p tcp --dport {port} -j ACCEPT"
        deleted = shell(
            f"while iptables -w -C INPUT {rule} 2>/dev/null; do\n"
            f"  iptables -w -D INPUT {rule} || break\n"
            "  echo deleted\n"
            "done"
        )[1].split("\n")
        answer = shell(f"iptables -L INPUT -n | grep {port}; echo checked")[1].split(
            "\n"
        )
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
    voice_rules = [
        f"{chain} -{side} lo -p tcp -m multiport --ports {VOICE_PORT} -j ACCEPT"
        for chain, side in (("INPUT", "i"), ("OUTPUT", "o"))
    ]
    deleted = shell(
        "".join(
            f"while iptables -w -C {rule} 2>/dev/null; do\n"
            f"  iptables -w -D {rule} || break\n"
            "  echo deleted\n"
            "done\n"
            for rule in voice_rules
        )
        + "".join(
            f"iptables -w -C {rule} 2>/dev/null && echo left\n" for rule in voice_rules
        )
        + "echo checked"
    )[1].split("\n")
    if "checked" not in deleted or "left" in deleted:
        warn(
            "firewall",
            f"The loopback rules for tcp/{VOICE_PORT} may still be in the chains.",
            "Nothing listens behind them now. They live in the chain rather than on"
            " disk, so a reboot clears them.",
        )
    elif "deleted" in deleted:
        closed.append(f"tcp/{VOICE_PORT} on lo")
    STATE.changed |= "deleted" in deleted
    if closed:
        ok("firewall", "closed " + ", ".join(closed))
    elif not STATE.warned:
        ok("firewall", "no overdub rules open")

    cleared = shell(
        "for f in /data/property/persist.overdub.*; do\n"
        '  [ -e "$f" ] || continue\n'
        '  setprop "${f##*/}" ""\n'
        '  rm -f "$f"\n'
        '  [ -e "$f" ] || echo cleared\n'
        "done"
    )[1].split("\n")
    STATE.changed |= "cleared" in cleared
    answer = shell(f"getprop {SENDFLAG}; echo checked")[1].split("\n")
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
            f"The adb keys set from Home Assistant are gone with {ADBKEYS}, so adb"
            " falls back to the system's own: it stays on, as firebreak left it, and"
            " only the computer that rooted the Dot gets in, by its key in"
            " /adb_keys."
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
    code, listing = shell("ps")
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
    answer = shell(
        f"for path in {' '.join(SWEPT)} {SENDKEY}.new-* {SENDPAIRS}.new-*"
        f" {SENDLAST}.new-*; do\n"
        '  [ -e "$path" ] && echo "$path"\n'
        "done\n"
        "echo swept"
    )[1].split("\n")
    if "swept" not in answer:
        return None
    return [line for line in answer if line in SWEPT or line.startswith(TEMPS)]


def release_voice() -> None:
    now = endpoint()
    if now is None:
        warn(
            "voice",
            "Could not read where Alexa sends her session. If it is still"
            f" {VOICEURL}, nothing listens there and she cannot answer.",
        )
        return
    if now != VOICEURL:
        ok("voice", "Alexa's session goes to Amazon")
        return
    code, text = shell(f"cat {SDKCONFIG}; echo; echo checked")
    stock = ""
    if code == 0 and text.rstrip().endswith("checked"):
        body = re.sub(r"(?m)^\s*//.*$", "", text.rstrip()[: -len("checked")])
        try:
            stock = json.loads(body).get("acl", {}).get("endpoint", "")
        except ValueError:
            stock = ""
    if not re.fullmatch(r"https://[A-Za-z0-9.-]+(:[0-9]+)?/?", stock):
        warn(
            "voice",
            f"Alexa still sends her session to {VOICEURL}, where nothing listens, and"
            f" {SDKCONFIG} named no endpoint to put back. She cannot answer until"
            " AVSEndpoint is set by hand.",
        )
        return
    pending(detail=f"pointing Alexa back at {stock}", label="voice")
    shell(f"lipc-set-prop -s -q com.amazon.puffin AVSEndpoint {stock}")
    for _ in range(30):
        if endpoint() == stock:
            STATE.changed = True
            ok("voice", f"Alexa's session goes to {stock} again")
            return
        time.sleep(0.5)
    warn(
        "voice",
        f"AVSEndpoint did not read back as {stock}. If it is still {VOICEURL},"
        " nothing listens there and Alexa cannot answer.",
    )


def shell(command: str) -> tuple[int, str]:
    return adb("shell", command)


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


def warn(label: str, *lines: str) -> None:
    STATE.warned = True
    show(*lines, label=label, sign=mark("warn"))


if __name__ == "__main__":
    try:
        main()
    except KeyboardInterrupt:
        print(file=sys.stderr)
        sys.exit(130)
