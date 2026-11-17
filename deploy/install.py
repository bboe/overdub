#!/usr/bin/env python3
"""Install overdub persistently on a rooted Echo Dot (2nd Generation), over adb.
It builds with build.sh when it runs from a source tree, and installs
build/overdub as it stands when it runs from a release. Set ANDROID_SERIAL to
pick one of several attached or connected devices. It needs Python 3.9 or later
and adb. Every step is read back; docs/deployment.md says why. A Dot on Fire OS
5.5.5.4 is moved to Fire OS 6 first, over USB, by firebreak through uv, and
keeps its setup.
"""

from __future__ import annotations

import argparse
import base64
import contextlib
import ctypes
import hashlib
import os
import pathlib
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import textwrap
import time
from typing import TYPE_CHECKING, Callable, NoReturn, TextIO

if TYPE_CHECKING:
    from collections.abc import Generator

ADB_COPY = "/data/misc/adb/adb_keys"
BIN_DIR = "/data/local/bin"
ADB_KEYS = BIN_DIR + "/adb_keys"
APPLIED = BIN_DIR + "/.overdub-applied"
BINARY = BIN_DIR + "/overdub"
BOOT = "/data/local/bin/overdub.sh"
BOOT_ID = re.compile(r"^[0-9a-f-]{36}$", re.MULTILINE)
CLEANUP_WAIT = 30
DNS_LABEL = 63
FAILED = "/data/local/bin/.overdub-failed"
FIREBREAK = "firebreak>=1.4.1,<2"
FIRE_OS_5_LEFTOVERS = (
    "/cache/magisk.log",
    "/cache/magisk.log.bak",
    "/data/adb/magisk",
    "/data/adb/magisk.db",
    "/data/adb/magisk.img",
    "/data/app/com.topjohnwu.magisk-*",
    "/data/data/com.topjohnwu.magisk",
    "/data/local/map",
)
INIT = "/system/etc/init/overdub.rc"
KEY = "/data/local/bin/.overdub-noise-key"
KEY_SHAPE = re.compile(r"[A-Za-z0-9+/]{43}=")
LABEL = 14
INDENT = " " * (LABEL + 4)
MD5_SHAPE = re.compile(r"^[0-9a-f]{32}", re.MULTILINE)
MOVE_WAIT = 300
OD_LINE = re.compile(r"(?:\s*[0-9a-f]{2})+\s*")
ROOT = pathlib.Path(__file__).resolve().parent.parent
SENDKEY = "/data/local/bin/.overdub-sendspin-key"
SENDKEY_BYTES = 64
SENDPAIRS = "/data/local/bin/.overdub-sendspin-pairs"
STAGE = "/data/local/tmp/overdub-install"
STATE = argparse.Namespace(
    binary_changed=False,
    boot_changed=False,
    changed=False,
    init_changed=False,
    key_landed=False,
    moved=False,
    pending=False,
    staged=False,
    temp=[],
    timeout=None,
    warned=False,
)
STOP_SIGNALS = ("SIGBREAK", "SIGHUP", "SIGINT", "SIGTERM")
TRIAL = "/data/local/bin/.overdub-trial"
WINDOWS_CTRL_C = 0xC000013A
X25519_A24 = 121665
X25519_BASE = 9
X25519_PRIME = 2**255 - 19


def adb(*args: str) -> tuple[int, str]:
    result = subprocess.run(
        ["adb", *args],
        check=False,
        stderr=subprocess.STDOUT,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        timeout=STATE.timeout,
    )
    return result.returncode, result.stdout.decode("utf-8", "replace").replace("\r", "")


def adb_ok(*args: str) -> str:
    code, out = adb(*args)
    if code != 0:
        fail("adb", f"adb {' '.join(args)} failed:", *out.strip().split("\n"))
    return out


@contextlib.contextmanager
def answering(label: str) -> Generator[None, None, None]:
    previous = STATE.timeout
    STATE.timeout = CLEANUP_WAIT
    try:
        yield
    except subprocess.TimeoutExpired:
        fail(label, f"The Dot did not answer adb within {CLEANUP_WAIT} seconds.")
    finally:
        STATE.timeout = previous


def applied(pid: str) -> str:
    listing = shell(f"md5sum {BINARY} {KEY} 2>/dev/null")[1]
    found = {
        path: digest
        for digest, path in re.findall(
            r"^([0-9a-f]{32})\s+(\S+)$", listing, re.MULTILINE
        )
    }
    return " ".join(
        [f"pid={pid}"] + [found.get(path, "none") for path in (BINARY, KEY)]
    )


def attempt(*, action: Callable[[], object], fix: str, label: str) -> None:
    try:
        action()
    except subprocess.TimeoutExpired:
        warn(
            label,
            f"The Dot did not answer within {CLEANUP_WAIT} seconds. Once it is"
            " back, remove what this install left:",
            f"  adb shell '{fix}'",
        )


def boot_id() -> str:
    found = BOOT_ID.search(shell("cat /proc/sys/kernel/random/boot_id")[1])
    return found.group() if found else ""


def boot_script_for(name: str) -> str:
    text = (ROOT / "deploy/overdub.sh").read_text().replace("\r\n", "\n")
    text, count = re.subn(r"^NAME=$", f"NAME={name}", text, flags=re.MULTILINE)
    if count != 1:
        fail("boot script", "Could not set NAME in deploy/overdub.sh.")
    return text


def booted() -> bool:
    return shell("getprop sys.boot_completed")[1].strip() == "1"


def bounded(check: Callable[[], bool]) -> bool:
    previous = STATE.timeout
    STATE.timeout = CLEANUP_WAIT
    try:
        return check()
    except subprocess.TimeoutExpired:
        return False
    finally:
        STATE.timeout = previous


def build() -> None:
    script = ROOT / "build.sh"
    binary = ROOT / "build/overdub"
    if not script.is_file():
        if not binary.is_file():
            fail("build", "No build.sh to build with, and no build/overdub to install.")
        ok(
            "build",
            f"prebuilt build/overdub, md5 {md5(binary)}",
        )
        return
    if os.name == "nt":
        fail(
            "build",
            "build.sh is here, so this is a source tree, and it builds only on"
            " macOS or Linux. On Windows, install from a release tarball.",
        )
    if not os.access(script, os.X_OK):
        fail(
            "build",
            "./build.sh is here but not executable, so this is a source tree that"
            " cannot build. Refusing to install whatever build/overdub holds:",
            "  chmod +x build.sh",
        )
    if shutil.which("git"):
        dirty = subprocess.run(
            ["git", "status", "--porcelain"],
            check=False,
            cwd=ROOT,
            stderr=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
        )
        changes = dirty.stdout.decode("utf-8", "replace").replace("\r", "").rstrip()
        if dirty.returncode == 0 and changes:
            warn(
                "build", "This build carries uncommitted changes:", *changes.split("\n")
            )
    pending(detail="running build.sh", label="build")
    result = subprocess.run(
        [str(script)],
        check=False,
        cwd=ROOT,
        stderr=subprocess.STDOUT,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
    )
    if result.returncode != 0:
        said = result.stdout.decode("utf-8", "replace").replace("\r", "").rstrip()
        fail("build", "build.sh failed:", *said.split("\n"))
    ok("build", f"build/overdub, md5 {md5(binary)}")


def can_move(serial: str) -> str:
    if ":" in serial:
        fail(
            "Fire OS",
            f"{serial} runs Fire OS 5, and this build runs on Fire OS 6 only."
            " firebreak moves a Dot over USB only: connect it by USB, and run this"
            " again with ANDROID_SERIAL set to its USB serial.",
        )
    uvx = shutil.which("uvx")
    if not uvx:
        fail(
            "Fire OS",
            "This Dot runs Fire OS 5, and this build runs on Fire OS 6 only."
            " firebreak moves it, and runs through uv. Install uv"
            " (https://github.com/bboe/firebreak#run-it) and run this again.",
        )
    if not (sys.stdin.isatty() and sys.stdout.isatty()):
        fail(
            "Fire OS",
            "This Dot runs Fire OS 5, and firebreak asks before it moves it to Fire"
            " OS 6, which needs a terminal. Run this again from a terminal, with"
            " nothing piped in or out.",
        )
    return uvx


def check_key_mode() -> str | None:
    if first_field(start="-", text=shell(f"ls -l {KEY}")[1]) != "-rw-------":
        STATE.changed = True
    shell(f"chmod 600 {KEY}")
    mode = first_field(start="-", text=shell(f"ls -l {KEY}")[1])
    if mode != "-rw-------":
        return f"The API key is {mode or 'nothing'}, want -rw-------."
    return None


def cleanup() -> None:
    stop_interrupts()
    STATE.timeout = CLEANUP_WAIT
    for path in STATE.temp:
        pathlib.Path(path).unlink(missing_ok=True)
    if STATE.key_landed:
        warn("API key", "Stopped after the key was written and before it was shown.")
        attempt(action=discard_new_key, fix=f"rm -f {KEY}", label="API key")
    if STATE.staged:
        attempt(action=remove_stage, fix=f"rm -rf {STAGE}", label="API key")


def device_key() -> str | None:
    code, answer = shell(f"cat {KEY}; echo --read-ok--")
    lines = answer.split("\n")
    if code != 0 or "--read-ok--" not in lines:
        return None
    body = [line for line in lines if line != "--read-ok--"]
    found = [line.strip() for line in body if KEY_SHAPE.fullmatch(line.strip())]
    if len(found) == 1:
        return found[0]
    joined = "".join("".join(body).split())
    return joined if KEY_SHAPE.fullmatch(joined) else ""


def discard_new_key() -> None:
    STATE.key_landed = False
    shell(f"rm -f {KEY}")
    if probe(f"-f {KEY}") == "no":
        note("The unverified key was removed. Install again to generate one.")
        return
    warn(
        "API key",
        "COULD NOT REMOVE the key just written, and it was never printed."
        " Nothing can connect with it. Remove it by hand before installing again:",
        f"  adb shell 'rm -f {KEY}'",
    )


def fail(label: str, *lines: str) -> NoReturn:
    show(*lines, label=label, sign=mark("fail"), stream=sys.stderr)
    sys.exit(1)


def fire_os() -> str:
    return shell("getprop ro.build.version.name")[1].strip()


def first_field(*, start: str, text: str) -> str:
    for line in text.split("\n"):
        if line.startswith(start):
            return line.split()[0]
    return ""


def install_api_key() -> None:
    present = shell(f"if [ -f {KEY} ]; then echo yes; else echo no; fi")[1]
    present = yes_no(present)
    if present is None:
        fail(
            "API key",
            "Could not tell whether the device already has a key. Nothing was"
            " changed. Check that adb still works, and try again.",
        )
    if present == "yes":
        existing = device_key()
        if existing is None:
            fail(
                "API key",
                "Could not read the key off the device. Nothing was changed. Check"
                " that adb still works, and try again.",
            )
        if not existing:
            fail(
                "API key",
                "The key already on the device is not 32 bytes of base64. The"
                " daemon will not start with it, and an install keeps an existing"
                " key. Nothing was changed. Delete it and install again to get a"
                " new one, then give that to Home Assistant:",
                f"  adb shell 'rm -f {KEY}'",
            )
        problem = check_key_mode()
        if problem:
            fail("API key", problem)
        ok("API key", "kept the one already on the device")
        return

    STATE.changed = True
    key = base64.b64encode(os.urandom(32)).decode()
    keyfile = temp_file(key + "\n")
    stage(directory=STAGE, flag="staged", label="API key")
    adb_ok("push", keyfile, STAGE + "/k.txt")
    STATE.key_landed = True
    shell_ok(
        f"umask 077\nmkdir -p {BIN_DIR}\ncp {STAGE}/k.txt {KEY}\nchmod 600 {KEY}\n"
        f"rm -rf {STAGE}"
    )
    if probe(f"-e {STAGE}") != "no":
        discard_new_key()
        fail(
            "API key",
            f"Could not confirm the staged key is gone from {STAGE}; treat it as"
            " still there.",
        )
    STATE.staged = False
    if device_key() != key:
        discard_new_key()
        fail("API key", "The API key did not land on the device.")
    problem = check_key_mode()
    if problem:
        discard_new_key()
        fail("API key", problem)
    ok("API key", "generated, and verified on the device")
    print()
    print("   Paste this key into Home Assistant's ESPHome integration.")
    print("   The installer keeps no copy:")
    print()
    print(f"       {key}", flush=True)
    STATE.key_landed = False
    print()
    print("   The API is encrypted from the next start. Home Assistant needs")
    print("   this key to connect at all.")
    print()
    pathlib.Path(keyfile).unlink()


def install_binary(boot_script: str) -> None:
    built = md5(ROOT / "build/overdub")
    STATE.binary_changed = remote_md5(BINARY) != built or probe(f"-x {BINARY}") != "yes"
    STATE.boot_changed = (
        remote_md5(BOOT) != md5(boot_script) or probe(f"-x {BOOT}") != "yes"
    )
    STATE.changed |= STATE.binary_changed or STATE.boot_changed
    if not re.search(r"^drwx------", shell(f"ls -ldn {BIN_DIR}")[1], re.MULTILINE):
        STATE.changed = True
    shell_ok(f"mkdir -p {BIN_DIR}\nchmod 700 {BIN_DIR}")
    shell_ok(f"rm -f {TRIAL} {FAILED} {BINARY}.prev")
    if probe(f"-e {TRIAL}") != "no":
        fail(
            "binary",
            f"{TRIAL} is still there, so the boot script could replace the"
            " binary this installs with the one an update replaced.",
        )
    if STATE.binary_changed:
        adb_ok("push", str(ROOT / "build/overdub"), "/data/local/tmp/overdub")
        shell_ok(
            f"cp /data/local/tmp/overdub {BINARY}.new\n"
            f"chmod 755 {BINARY}.new\n"
            f"mv -f {BINARY}.new {BINARY}\n"
            "rm -f /data/local/tmp/overdub"
        )
    if STATE.boot_changed:
        adb_ok("push", boot_script, "/data/local/tmp/s.sh")
        shell_ok(
            f"cp /data/local/tmp/s.sh {BOOT}.new\n"
            f"chmod 755 {BOOT}.new\n"
            f"mv -f {BOOT}.new {BOOT}\n"
            "rm -f /data/local/tmp/s.sh"
        )

    installed = remote_md5(BINARY)
    if not installed:
        fail("binary", "The device did not hash the binary.")
    if installed != built:
        fail("binary", f"The device has {installed}, built {built}.")
    answer = probe(f"-x {BINARY}")
    if answer is None:
        fail("binary", "The device did not say whether the binary is executable.")
    if answer != "yes":
        fail("binary", "The binary landed, but is not executable.")
    ok("binary", f"{'md5' if STATE.binary_changed else 'unchanged, md5'} {built}")

    mode = shell(f"ls -ldn {BIN_DIR}")[1]
    found = re.search(r"^d[rwx-]{9}", mode, re.MULTILINE)
    mode = found.group() if found else ""
    if mode != "drwx------":
        fail(
            "key directory",
            f"{BIN_DIR} is {mode or 'unreadable'}, want drwx------. The API key"
            " lives there, and the mode is asserted rather than inherited: mkdir"
            " leaves 0700 or keeps whatever an older install had.",
        )
    ok("key directory", f"{BIN_DIR} is {mode}")


def install_boot_script(boot_script: str) -> None:
    built = md5(boot_script)
    installed = remote_md5(BOOT)
    if installed != built:
        fail(
            "boot script",
            f"The boot script did not land (device has {installed or 'no hash'},"
            f" pushed {built}).",
        )
    answer = probe(f"-x {BOOT}")
    if answer is None:
        fail("boot script", "The device did not say whether it is executable.")
    if answer != "yes":
        fail("boot script", "The boot script landed, but is not executable.")
    ok("boot script", BOOT if STATE.boot_changed else f"{BOOT} unchanged")


def install_init() -> None:
    rc = temp_file((ROOT / "deploy/overdub.rc").read_text().replace("\r\n", "\n"))
    built = md5(rc)
    previous = remote_md5(INIT)
    STATE.init_changed = previous != built
    if STATE.init_changed:
        STATE.changed = True
        adb_ok("push", rc, "/data/local/tmp/overdub.rc")
        shell(
            "mount -o rw,remount /\n"
            f"cp /data/local/tmp/overdub.rc {INIT}\n"
            f"chmod 644 {INIT}\n"
            "mount -o ro,remount /\n"
            "rm -f /data/local/tmp/overdub.rc"
        )
    installed = remote_md5(INIT)
    if installed != built:
        fail(
            "init",
            f"{INIT} did not land (device has {installed or 'no hash'}, pushed"
            f" {built}). The system partition must remount read-write, which"
            " firebreak's Fire OS 6 unlock allows by turning verity off.",
        )
    if not re.search(r"^\S+ / \S+ ro[,\s]", shell("cat /proc/mounts")[1], re.MULTILINE):
        warn(
            "init",
            "/ did not remount read-only after the write. A reboot mounts it"
            " read-only again.",
        )
    ok("init", INIT if STATE.init_changed else f"{INIT} unchanged")
    if STATE.init_changed and previous and service_state():
        warn(
            "init",
            f"REBOOT REQUIRED: init read the previous {INIT} at boot and keeps that"
            " definition until the next one.",
        )


def interrupted(signum: int, _frame: object) -> NoReturn:
    stop_interrupts()
    with contextlib.suppress(OSError):
        print(file=sys.stderr)
    sys.exit(128 + signum)


def main() -> None:
    parser = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter
    )
    parser.add_argument(
        "name",
        help="the device name Home Assistant knows it by, unique on your network,"
        " for example kitchen",
    )
    name = parser.parse_args().name
    if not re.fullmatch(r"[a-z0-9_-]+", name):
        usage("the name must be lowercase letters, digits, - or _")
    if name.startswith("-") or name.endswith("-"):
        usage("the name must not start or end with -; that is not a valid DNS label")
    if len(name) > DNS_LABEL:
        usage(f"the name is {len(name)} characters; a DNS label stops at {DNS_LABEL}")
    if not shutil.which("adb"):
        usage("adb not found: install Android platform-tools")

    serial = serial_number()
    print(f"Installing overdub as {name} on {serial}.")
    print()
    version = fire_os()
    uvx = can_move(serial) if version.startswith("Fire OS 5") else ""
    build()
    boot_script = temp_file(boot_script_for(name))
    if uvx:
        move_to_fire_os_6(serial=serial, uvx=uvx, version=version)
    if not re.search(r"uid=0\b", shell("id")[1]):
        fail(
            "root",
            "adb shell id did not report uid=0. overdub needs the root adb that"
            " firebreak's Fire OS 6 unlock leaves.",
        )
    ok("root", "adb runs as root")
    remove_fire_os_5_leftovers()
    install_api_key()
    remove_old_adb_key()
    install_binary(boot_script)
    install_boot_script(boot_script)
    install_init()
    if STATE.moved:
        reboot_to_start(name)
    else:
        restart(name)
    print_pairing_token()
    print()
    if STATE.changed:
        print(
            "Installed. Home Assistant must be able to reach this device on tcp/6053;"
        )
        print("README.md says how.")
    elif STATE.warned:
        print("Nothing changed; see the warning above.")
    else:
        print("Already installed; nothing changed.")


def mark(kind: str) -> str:
    marks = {"fail": "❌", "ok": "✅", "warn": "❗"}
    try:
        marks[kind].encode(sys.stdout.encoding or "ascii")
        return marks[kind]
    except UnicodeEncodeError:
        return {"fail": "XX", "ok": "OK", "warn": "!!"}[kind]


def md5(path: str | pathlib.Path) -> str:
    return hashlib.md5(
        pathlib.Path(path).read_bytes(), usedforsecurity=False
    ).hexdigest()


def move_to_fire_os_6(*, serial: str, uvx: str, version: str) -> None:
    ok(
        "Fire OS",
        f"{version}: overdub needs Fire OS 6. Installing and running firebreak"
        " (https://github.com/bboe/firebreak), a separate tool, to move the Dot"
        " there.",
    )
    note(
        "firebreak keeps the Dot's Wi-Fi, its Alexa setup and overdub's keys, asks"
        " before it writes, and takes about 8 minutes. Everything between the two"
        " firebreak lines below is firebreak's."
    )
    print()
    print(f"---- firebreak ({FIREBREAK}) starts ----", flush=True)
    status = run_foreground(
        [
            uvx,
            "--refresh-package",
            "firebreak",
            "--from",
            FIREBREAK,
            "firebreak",
            "unlock",
            "fireos6",
        ],
        env={**os.environ, "ANDROID_SERIAL": serial},
    )
    print(f"---- firebreak ends, exit status {status}; back to overdub ----")
    print()
    if status in {128 + signal.SIGINT, WINDOWS_CTRL_C}:
        sys.exit(128 + signal.SIGINT)
    if status != 0:
        fail(
            "Fire OS",
            f"firebreak exited {status}, so the Dot is wherever it stopped. overdub"
            " changed nothing. Once firebreak's message is dealt with, run this"
            " again.",
        )
    pending(detail="waiting for Fire OS 6 to start", label="Fire OS")
    with answering("Fire OS"):
        if not wait_for(lambda: bounded(started_fire_os_6), seconds=MOVE_WAIT):
            fail(
                "Fire OS",
                f"firebreak finished, and {MOVE_WAIT} seconds later the Dot had not"
                f" started Fire OS 6. It reports {fire_os()!r}.",
            )
        version = fire_os()
    STATE.changed = True
    STATE.moved = True
    ok("Fire OS", f"moved to {version}")


def note(text: str) -> None:
    with contextlib.suppress(OSError):
        print(textwrap.fill(text, 79, initial_indent=INDENT, subsequent_indent=INDENT))


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


def pairing_token(key: bytes) -> str:
    body = base64.b32encode(x25519_public(key[:32]) + key[32:]).decode()
    return "SP:0" + body.rstrip("=").replace("2", "9")


def pending(*, detail: str, label: str) -> None:
    if sys.stdout.isatty():
        print(f"   {label:<{LABEL}} {detail} ...", end="", flush=True)
        STATE.pending = True


def print_pairing_token() -> None:
    code, answer = shell(
        f"[ -e {SENDKEY} ] || echo --no-key--;"
        f" od -v -An -tx1 {SENDKEY} && echo --read-ok--"
    )
    lines = answer.split("\n")
    if "--no-key--" in lines:
        warn(
            "pairing",
            "The daemon has not made its Sendspin key yet. Install again once it"
            " runs to see the pairing token.",
        )
        return
    if code != 0 or "--read-ok--" not in lines:
        warn("pairing", "Could not read the Sendspin key off the device.")
        return
    key = bytes.fromhex(
        "".join(line for line in lines if OD_LINE.fullmatch(line)).replace(" ", "")
    )
    if len(key) != SENDKEY_BYTES:
        warn("pairing", f"The Sendspin key is {len(key)} bytes, want {SENDKEY_BYTES}.")
        return
    if STATE.moved and probe(f"-s {SENDPAIRS}") == "yes":
        ok(
            "pairing",
            "kept the Sendspin key and its pairing, so Music Assistant needs"
            " nothing. Run this again to see the pairing token.",
        )
        return
    ok("pairing", "read the Sendspin key")
    print()
    print("   To pair Music Assistant with this device, paste this token into")
    print("   its Sendspin player settings. Anyone holding it can pair a")
    print("   server with this device:")
    print()
    print(f"       {pairing_token(key)}", flush=True)
    print()


def probe(expression: str) -> str | None:
    return yes_no(shell(f"[ {expression} ] && echo yes || echo no")[1])


def reboot_and_wait(name: str) -> None:
    before = boot_id()
    if not before:
        fail(
            "daemon",
            "Could not read the boot id, so a reboot could not be told from the boot"
            f" before it. Nothing was rebooted. init reads {INIT} only at boot, so"
            " reboot to start the daemon.",
        )
    pending(detail="rebooting, so init reads overdub.rc", label="daemon")
    bounded(lambda: shell("reboot")[0] == 0)
    if not wait_for(
        lambda: bounded(lambda: boot_id() not in {"", before} and booted()),
        seconds=MOVE_WAIT,
    ):
        fail(
            "daemon",
            f"The Dot did not start again within {MOVE_WAIT} seconds of the reboot"
            " that starts the daemon.",
        )
    wait_for(lambda: bounded(lambda: bool(overdub_pid())), seconds=60)
    pid = overdub_pid()
    if not pid:
        warn(
            "daemon",
            "Did NOT start after the reboot. The reason is in"
            " /data/local/tmp/overdub.log.",
        )
        return
    shell_ok(f"umask 077; echo '{applied(pid)}' > {APPLIED}")
    report_name(
        done=f"started as -name {name} after a reboot, pid {pid}", name=name, pid=pid
    )


def reboot_to_start(name: str) -> None:
    with answering("daemon"):
        reboot_and_wait(name)


def remote_md5(path: str) -> str:
    found = MD5_SHAPE.search(shell(f"md5sum {path}")[1])
    return found.group() if found else ""


def remove_fire_os_5_leftovers() -> None:
    listing = f"ls -d {' '.join(FIRE_OS_5_LEFTOVERS)} 2>/dev/null"
    found = [line for line in shell(listing)[1].split("\n") if line.startswith("/")]
    if not found:
        return
    shell(f"rm -rf {' '.join(found)}")
    left = [line for line in shell(listing)[1].split("\n") if line.startswith("/")]
    if left:
        warn(
            "Fire OS 5",
            "Could not remove what Fire OS 5 left:",
            *(f"  {path}" for path in left),
        )
        return
    STATE.changed = True
    ok("Fire OS 5", "removed what Fire OS 5 left:", *(f"  {path}" for path in found))


def remove_old_adb_key() -> None:
    if probe('"$(getprop ro.adb.secure)" = 1') == "no":
        warn(
            "adb",
            "ro.adb.secure is not 1, so tcp/5555 is a root shell with no key."
            " The Dot was rooted before firebreak v1.2.0 locked adb;"
            " docs/usage.md says how to lock it.",
        )
    if probe(f"-f {ADB_KEYS}") == "no":
        return
    old = remote_md5(ADB_KEYS)
    removed = ADB_KEYS
    if old and remote_md5(ADB_COPY) == old:
        shell(f"rm -f {ADB_COPY}")
        removed += f" and its copy at {ADB_COPY}"
    shell(f"rm -f {ADB_KEYS}")
    if probe(f"-f {ADB_KEYS}") != "no":
        warn("adb key", f"Could not remove the old key file at {ADB_KEYS}.")
        return
    STATE.changed = True
    ok(
        "adb key",
        f"removed {removed}; set keys with the set_adb_keys action in Home Assistant",
    )


def remove_stage() -> None:
    shell(f"rm -rf {STAGE}")
    if probe(f"-e {STAGE}") != "no":
        warn(
            "API key",
            f"Could not confirm the staged key is gone from {STAGE}."
            " Remove it once the Dot is back:",
            f"  adb shell 'rm -rf {STAGE}'",
        )


def report_name(*, done: str, name: str, pid: str) -> None:
    running = running_name(pid)
    if running is None:
        warn(
            "daemon",
            f"Running as pid {pid}, but /proc/{pid}/cmdline was unreadable, so the"
            " running -name is unverified. Reboot if you changed it.",
        )
    elif running != name:
        warn(
            "daemon",
            f"REBOOT REQUIRED: pid {pid} runs with -name {running}, not {name}, and"
            " restarting did not change that.",
        )
    else:
        ok("daemon", done)


def restart(name: str) -> None:
    old = overdub_pid()
    if old is None:
        fail("daemon", "adb went away before the restart check.")
    state = service_state()
    if not state:
        if old and (STATE.binary_changed or STATE.boot_changed):
            warn(
                "daemon",
                f"REBOOT REQUIRED: pid {old} runs what it started with. init has not"
                f" read {INIT} yet, so nothing here can restart it.",
            )
        elif old:
            report_name(done=f"running as -name {name}, pid {old}", name=name, pid=old)
        else:
            warn(
                "daemon",
                f"Not running. init reads {INIT} only at boot, so reboot to start it.",
            )
        return
    stamp = shell(f"cat {APPLIED} 2>/dev/null")[1].split("\n")
    if (
        old
        and applied(old) in stamp
        and not STATE.boot_changed
        and running_name(old) == name
    ):
        report_name(done=f"running as -name {name}, pid {old}", name=name, pid=old)
        return
    STATE.changed = True
    pending(
        detail=f"restarting pid {old}" if old else "starting the boot loop",
        label="daemon",
    )
    if old:
        shell(f"kill {old}")
        wait_for(lambda: overdub_pid() != old, seconds=10)
    shell("stop overdub")
    if not wait_for(lambda: service_state() == "stopped", seconds=10):
        fail(
            "daemon",
            f"init still reports the service {service_state() or 'unknown'} 10"
            " seconds after stop overdub. A start now would do nothing, so none"
            " was sent. Run this again, or reboot.",
        )
    shell("start overdub")
    wait_for(lambda: bool(overdub_pid()), seconds=15)
    new = overdub_pid()
    if new is None:
        fail("daemon", "adb went away during the restart check.")
    if not new:
        warn(
            "daemon",
            "Did NOT start. init started the boot loop, so the reason is in"
            " /data/local/tmp/overdub.log.",
        )
        return
    shell_ok(f"umask 077; echo '{applied(new)}' > {APPLIED}")
    done = (
        f"restarted as -name {name}, pid {old} -> {new}"
        if old
        else (f"started as -name {name}, pid {new}")
    )
    report_name(done=done, name=name, pid=new)


def run_foreground(command: list[str], *, env: dict[str, str]) -> int:
    handlers = {}
    for name in STOP_SIGNALS:
        if hasattr(signal, name):
            number = getattr(signal, name)
            handlers[number] = signal.signal(number, lambda *_: None)
    try:
        status = subprocess.run(command, check=False, env=env).returncode
    finally:
        for number, handler in handlers.items():
            signal.signal(number, handler)
    return 128 - status if status < 0 else status


def running_name(pid: str) -> str | None:
    code, running = shell(f"cat /proc/{pid}/cmdline")
    words = running.split("\0") if code == 0 else []
    if "-name" not in words or words.index("-name") + 1 >= len(words):
        return None
    return words[words.index("-name") + 1]


def serial_number() -> str:
    adb("start-server")
    code, out = adb("get-serialno")
    lines = [line for line in out.strip().split("\n") if line]
    if code != 0 or len(lines) != 1 or lines[0] == "unknown":
        fail(
            "adb",
            "No single device to install on:",
            *lines,
            "Set ANDROID_SERIAL to pick one.",
        )
    return lines[0]


def service_state() -> str:
    return shell("getprop init.svc.overdub")[1].strip()


def shell(command: str) -> tuple[int, str]:
    return adb("shell", command)


def shell_ok(command: str) -> str:
    code, out = shell(command)
    if code != 0:
        fail("adb", "A command on the device failed:", *out.strip().split("\n"))
    return out


def show(*lines: str, label: str, sign: str, stream: TextIO = sys.stdout) -> None:
    with contextlib.suppress(OSError):
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
                    textwrap.fill(
                        line, 79, initial_indent=first, subsequent_indent=INDENT
                    ),
                    file=stream,
                    flush=True,
                )
            first = INDENT


def stage(*, directory: str, flag: str, label: str) -> None:
    made = adb(
        "shell",
        f"mkdir {directory} 2>/dev/null && chmod 700 {directory} && echo made",
    )[1]
    if "made" in made.split("\n"):
        setattr(STATE, flag, True)
    else:
        fail(
            label,
            f"{directory} already exists, so either another install is running or"
            " one was interrupted. Nothing was changed. If nothing else is running,"
            " remove it and try again:",
            f"  adb shell 'rm -rf {directory}'",
        )
    mode = first_field(start="d", text=adb("shell", f"ls -ld {directory}")[1])
    if mode != "drwx------":
        fail(
            label,
            f"The staging directory is {mode or 'missing'}, want drwx------. The key"
            " was not pushed. Nothing was changed.",
        )


def started_fire_os_6() -> bool:
    return fire_os().startswith("Fire OS 6") and booted()


def stop_interrupts() -> None:
    for name in STOP_SIGNALS:
        if hasattr(signal, name):
            signal.signal(getattr(signal, name), signal.SIG_IGN)
    if os.name == "nt":
        ctypes.windll.kernel32.SetConsoleCtrlHandler(None, True)  # ruff: ignore[boolean-positional-value-in-call]


def temp_file(text: str) -> str:
    handle, path = tempfile.mkstemp(prefix="overdub-")
    STATE.temp.append(path)
    with os.fdopen(handle, "w", newline="\n") as f:
        f.write(text)
    return path


def usage(message: str) -> NoReturn:
    print(f"install.py: {message}", file=sys.stderr)
    sys.exit(2)


def wait_for(ready: Callable[[], bool], *, seconds: float) -> bool:
    deadline = time.monotonic() + seconds
    while not ready():
        if time.monotonic() > deadline:
            return False
        time.sleep(0.5)
    return True


def warn(label: str, *lines: str) -> None:
    STATE.warned = True
    show(*lines, label=label, sign=mark("warn"))


def x25519_ladder_step(
    x_2: int, z_2: int, x_3: int, z_3: int
) -> tuple[int, int, int, int]:
    prime = X25519_PRIME
    add_2 = x_2 + z_2
    add_2_squared = add_2 * add_2 % prime
    subtract_2 = x_2 - z_2
    subtract_2_squared = subtract_2 * subtract_2 % prime
    difference = add_2_squared - subtract_2_squared
    cross_left = (x_3 - z_3) * add_2 % prime
    cross_right = (x_3 + z_3) * subtract_2 % prime
    return (
        add_2_squared * subtract_2_squared % prime,
        difference * (add_2_squared + X25519_A24 * difference) % prime,
        pow(cross_left + cross_right, 2, prime),
        X25519_BASE * pow(cross_left - cross_right, 2, prime) % prime,
    )


def x25519_public(private: bytes) -> bytes:
    clamped = bytearray(private)
    clamped[0] &= 248
    clamped[31] &= 127
    clamped[31] |= 64
    scalar = int.from_bytes(clamped, "little")
    x_2, z_2, x_3, z_3 = 1, 0, X25519_BASE, 1
    swap = 0
    for bit_index in reversed(range(255)):
        bit = (scalar >> bit_index) & 1
        swap ^= bit
        if swap:
            x_2, x_3, z_2, z_3 = x_3, x_2, z_3, z_2
        swap = bit
        x_2, z_2, x_3, z_3 = x25519_ladder_step(x_2, z_2, x_3, z_3)
    if swap:
        x_2, z_2 = x_3, z_3
    prime = X25519_PRIME
    return (x_2 * pow(z_2, prime - 2, prime) % prime).to_bytes(32, "little")


def yes_no(text: str) -> str | None:
    for line in text.split("\n"):
        if line in {"yes", "no"}:
            return line
    return None


if __name__ == "__main__":
    for name in STOP_SIGNALS:
        if hasattr(signal, name):
            signal.signal(getattr(signal, name), interrupted)
    try:
        main()
    finally:
        cleanup()
