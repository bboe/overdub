#!/usr/bin/env python3
"""Unlock and root an Echo Dot (2nd Generation) over USB, from stock Fire OS 6 or
from any point part way through, and leave it on rooted Fire OS 5.5.5.4. It
detects where the Dot is, and keeps running until the Dot is rooted: it waits
while the Dot reboots, and while you take a step it asks for. Stopped, it picks
up where it left off on the next run. It uses the one Dot on USB; set
ANDROID_SERIAL when several are. It needs Python 3.9 or later, and adb and
fastboot from Android platform-tools. docs/rooting.md says why each step is
there.
"""

import argparse
import hashlib
import os
import pathlib
import shutil
import sqlite3
import subprocess
import sys
import tempfile
import textwrap
import threading
import time
import urllib.request
import zipfile

AMONET_V1 = "amonet-biscuit-v1.1.0.zip"
AMONET_V1_SHA = "bd4d3a18b6b6e9ff6e49a4739159a81020673202795cb3959f7c9ff24351b663"
AMONET_V2 = "amonet-biscuit-v2.0.0.zip"
AMONET_V2_SHA = "98297293701082bc7272efe077f941c56fc7b6e1f27ef6f2e93b6e4c6fc7b62d"
ARGS = argparse.Namespace(delay=0, probing=False, verbose=False)
BOOT_SH = """\
set -e
mountpoint -q /system || mount /system
rm -rf /tmp/bp; mkdir /tmp/bp; cd /tmp/bp; chmod 755 /tmp/magiskboot
dd if=/dev/block/other-boot of=boot.img bs=1048576 2>/dev/null
LD_LIBRARY_PATH=/system/lib /tmp/magiskboot --unpack boot.img >/dev/null 2>&1
mkdir r; cd r; cpio -id < ../ramdisk.cpio 2>/dev/null
for f in fstab*; do sed -i 's/,verify//g; s/verify,//g' "$f"; done
sed -i -e 's/^ro.secure=1$/ro.secure=0/' -e 's/^ro.debuggable=0$/ro.debuggable=1/' \\
  -e 's/^persist.sys.usb.config=.*/persist.sys.usb.config=mtp,adb/' default.prop
find . | cpio -o -H newc > ../ramdisk.cpio 2>/dev/null; cd ..
LD_LIBRARY_PATH=/system/lib /tmp/magiskboot --repack boot.img new.img >/dev/null 2>&1
cd /; umount /system
echo root-step-ok
"""
DATA_SH = """\
set -e
umount /data /sdcard 2>/dev/null || true
d=/dev/block/platform/mtk-msdc.0/by-name/userdata
mke2fs -q -t ext4 -b 4096 "$d" $(( $(blockdev --getsize64 "$d") / 4096 - 256 ))
mount -t ext4 "$d" /data
mountpoint -q /data
echo root-step-ok
"""
FASTBOOT_MODE = (
    "Unplug the USB cable, press and hold the action button (the one with a dot),"
    " plug the cable back in, and let go when the light ring turns green."
)
FIREOS = "update-kindle-csm_biscuit-272.6.8.0_user_680767620.bin"
FIREOS_SHA = "6ababc517529938f0d1e836c3410a91df19683ae62d7fca9e2ca57320d5d2faa"
FIREOS_URL = (
    "https://d1s31zyz7dcc2d.cloudfront.net/47a1457e0802980eb32f63cd3ce355c0/" + FIREOS
)
LK_V1 = "f379dba-20170906_000423"
LK_V2 = "63cb91b-20221007_072309"
MAGISK = "Magisk-v17.3.zip"
MAGISK_SHA = "18e46b16b25ebe691c282fe311beccd4811cd533848a64e2efbd754fb85efde7"
MAGISK_URL = "https://github.com/topjohnwu/Magisk/releases/download/v17.3/" + MAGISK
MIRROR = "https://github.com/hkfuertes/amazon_device_biscuit/releases/download/none"
MORE_THAN_ONE = (
    "more than one Dot on USB: set ANDROID_SERIAL to one's serial"
    " (adb devices lists them)"
)
PUSH_TRIES = 3
PYSERIAL = "pyserial-3.5-py2.py3-none-any.whl"
PYSERIAL_SHA = "c4451db6ba391ca6ca299fb3ec7bae67a5c55dde170964c7a14ceefec02f2cf0"

PYSERIAL_URL = (
    "https://files.pythonhosted.org/packages/07/bc/"
    "587a445451b253b285629263eb51c2d8e9bcea4fc97826266d186f96f558/" + PYSERIAL
)

UPDATER = "com.amazon.device.software.ota"

UPDATE_HOSTS = (
    "updates.amazon.com",
    "softwareupdates.amazon.com",
    "amzndigitaldownloads.edgesuite.net",
    "amzdigital-a.akamaihd.com",
)
SYSTEM_SH = """\
set -e
mountpoint -q /system || mount /system
f=/system/etc/init.fosflags.sh
sed -i 's/if \\[ $(( $FOS_FLAGS_ADB_ON & $FOSFLAGS )) != 0 \\]; then/if true; then/; \
s/^\\( *\\)unset_adb_persistent_property$/\\1true/' "$f"
for h in {}; do
  grep -q " $h\\$" /system/etc/hosts || echo "127.0.0.1 $h" >> /system/etc/hosts
done
grep -q 'if true; then' "$f"
grep -q '^ *unset_adb_persistent_property$' "$f" && exit 1
sync; umount /system
echo root-step-ok
""".format(" ".join(UPDATE_HOSTS))


USER_SERIAL = os.environ.get("ANDROID_SERIAL")


WAIT = 600


class Progress:
    def __init__(self):
        self.t0 = time.monotonic()
        self.ts = self.t0
        self.line = ""
        self.open = False
        self.stopped = threading.Event()
        self.ticker = None

    def begin(self, label, estimate=""):
        self.end()
        about = f"(~{estimate})" if estimate else ""
        self.line = f"{label:<44} {about:<10} ... "
        delay(label)
        self.ts = time.monotonic()
        self.open = True
        if ARGS.verbose:
            print(self.line.rstrip())
        else:
            self.start()

    def end(self):
        if not self.open:
            return
        self.open = False
        back = "\r" if self.halt() else ""
        print(f"{back}{self.line}done in {self.seconds()}, {since(self.t0)} total")

    def halt(self):
        if not self.ticker:
            return False
        self.stopped.set()
        self.ticker.join()
        self.ticker = None
        return True

    def note(self, message):
        running = self.halt()
        print()
        print(message)
        if running:
            self.start()

    def seconds(self):
        return f"{int(time.monotonic() - self.ts):3d}s"

    def start(self):
        print(self.line, end="", flush=True)
        self.stopped.clear()
        self.ticker = threading.Thread(daemon=True, target=self.tick)
        self.ticker.start()

    def tick(self):
        while not self.stopped.wait(1):
            print(f"\r{self.line}{self.seconds()}", end="", flush=True)


PROGRESS = Progress()


def bootrom(amonet, wheel, erase):
    log_path = CACHE / "bootrom.log"
    env = dict(os.environ, PYTHONPATH=str(wheel), PYTHONUNBUFFERED="1")
    with log_path.open("w") as log:
        if ARGS.verbose:
            print(f"{clock()} $ {sys.executable} main.py (amonet v1.1.0 bootrom step)")
        brom = subprocess.Popen(
            [sys.executable, "main.py"],
            cwd=amonet / "modules",
            env=env,
            stderr=subprocess.STDOUT,
            stdin=subprocess.PIPE,
            stdout=log,
        )
        brom.stdin.write(b"\n" * 5)
        brom.stdin.close()
        time.sleep(3)
        if brom.poll() is not None:
            die(f"v1.1.0's bootrom step did not start; see {log_path}")
        if erase:
            ERASED.touch()
            for args in (["fastboot", "erase", "boot0"], ["fastboot", "reboot"]):
                result = run(args, timeout=60)
                if result.returncode != 0:
                    brom.kill()
                    die(f"{' '.join(args)} failed:\n{result.stdout}")
        else:
            PROGRESS.begin("unplug the Dot, wait 2 s, plug it back in")
        deadline = time.monotonic() + (60 if erase else 600)
        while brom.poll() is None and time.monotonic() < deadline:
            if "Found port" in log_path.read_text(errors="replace"):
                break
            time.sleep(1)
        else:
            if brom.poll() is None:
                brom.kill()
                die(
                    "the Dot's bootrom did not show up as a serial port. "
                    + no_port_help()
                )
        if not erase:
            PROGRESS.begin("finishing the move to amonet v1.1.0", "5 min")
        try:
            brom.wait(timeout=1800)
        except subprocess.TimeoutExpired:
            brom.kill()
            die(f"v1.1.0's bootrom step did not finish; see {log_path}")
    if brom.returncode != 0:
        die(f"v1.1.0's bootrom step failed; see {log_path}")
    if "Reboot to unlocked fastboot" not in log_path.read_text(errors="replace"):
        die(f"v1.1.0's bootrom step did not finish; see {log_path}")
    for _ in range(30):
        if in_fastboot() and getvar("lk_build_desc") == LK_V1:
            break
        time.sleep(2)
    else:
        die("the Dot did not come back in v1.1.0's fastboot")
    ERASED.unlink(missing_ok=True)


def cache_dir():
    if os.name == "nt":
        base = os.environ.get("LOCALAPPDATA") or pathlib.Path.home()
    else:
        base = os.environ.get("XDG_CACHE_HOME") or pathlib.Path.home() / ".cache"
    return pathlib.Path(base) / "overdub-root"


CACHE = cache_dir()
ERASED = CACHE / "boot0-erased"


def cache_note():
    if CACHE.is_dir():
        size = sum(f.stat().st_size for f in CACHE.rglob("*") if f.is_file())
        print(
            f"{CACHE} holds {size // 1000000} MB of downloads for the next run."
            " It is safe to delete."
        )


def check_adb():
    words = run(["adb", "version"], timeout=30).stdout.split()
    version = (
        words[4] if words[:4] == ["Android", "Debug", "Bridge", "version"] else "?"
    )
    parts = version.split(".")
    if not all(part.isdigit() for part in parts) or tuple(map(int, parts)) < (1, 0, 36):
        die(
            f"adb reports version {version}; this needs 1.0.36 (platform-tools r24)"
            " or newer"
        )


def clock():
    return time.strftime("%H:%M:%S")


def delay(label):
    if not ARGS.delay:
        return
    for left in range(ARGS.delay, 0, -1):
        print(f"\r{clock()} next: {label}; starting in {left:2d}s", end="", flush=True)
        time.sleep(1)
    print(f"\r{clock()} next: {label}; starting now      ")


def die(message, prefix="ERROR: "):
    if PROGRESS.halt():
        print()
    sys.exit(prefix + message)


def downgrade(from_twrp):
    amonet = unpack(AMONET_V1, MIRROR + "/" + AMONET_V1, AMONET_V1_SHA, "v1")
    wheel = fetch(PYSERIAL, PYSERIAL_URL, PYSERIAL_SHA)
    if from_twrp:
        run(["adb", "reboot", "bootloader"], check=True)
        for _ in range(30):
            if in_fastboot():
                break
            time.sleep(2)
    if getvar("unlock_status").lower() != "true":
        die("not in amonet's fastboot")
    if getvar("lk_build_desc") == LK_V1:
        die("the Dot already runs amonet v1.1.0's bootloader. Run dot_root.py again.")
    PROGRESS.begin("moving to amonet v1.1.0; the ring stays off", "5 min")
    bootrom(amonet, wheel, erase=True)
    v1_recovery()


def fastbrick():
    amonet = unpack(AMONET_V2, MIRROR + "/" + AMONET_V2, AMONET_V2_SHA, "v2")
    if getvar("product") != "BISCUIT":
        die("fastboot reports a product other than BISCUIT")
    lk = getvar("lk_build_desc")
    if not lk:
        die("fastboot did not report the bootloader version; the Dot was not modified")
    image = "bin/fastbrick.img"
    if lk == LK_V2:
        image = "bin/fastbrick-20221007.img"
    PROGRESS.begin("unlocking with amonet v2.0.0", "10 s")
    for attempt in range(10):
        if attempt:
            time.sleep(2)
        args = ["fastboot", "-S", "256M", "flash", "brick", image]
        try:
            out = run(args, cwd=amonet, timeout=8).stdout
            started = False
        except subprocess.TimeoutExpired as e:
            out = e.output or ""
            if isinstance(out, bytes):
                out = out.decode(errors="replace")
            started = True
        if "eMMC-RO" in out:
            die("the Dot's eMMC is read-only; it was not modified")
        if "Device mismatch" in out:
            die("the payload rejected this device; it was not modified")
        if started:
            PROGRESS.begin("exploit running; waiting for recovery", "40 s")
            return
    die("the unlock did not start after 10 attempts")


def fetch(name, url, want):
    CACHE.mkdir(exist_ok=True, parents=True)
    path = CACHE / name
    if not path.is_file() or sha256(path) != want:
        print("downloading " + name)
        part = CACHE / (name + ".part")
        with urllib.request.urlopen(url) as response, part.open("wb") as out:
            save(response, out)
        part.replace(path)
    if sha256(path) != want:
        die(f"{name} does not hash to {want}")
    return path


def getvar(name):
    try:
        out = run(["fastboot", "getvar", name], timeout=30).stdout
    except subprocess.TimeoutExpired:
        return ""
    for line in out.splitlines():
        if line.startswith(name + ":"):
            return line[len(name) + 1 :].strip()
    return ""


def hide_updater():
    def hidden():
        out = rshell(f"su -c 'dumpsys package {UPDATER}'")
        return any(
            line.strip().startswith("User 0:") and "hidden=true" in line
            for line in out.split("\n")
        )

    if not hidden():
        rshell(f"su -c 'pm hide {UPDATER}'")
    if not hidden():
        die(
            f"{UPDATER} is not hidden; an update would replace the boot image and remove root"
        )


def in_fastboot():
    out = run(["fastboot", "devices"], timeout=30).stdout
    serials = [
        line.split()[0]
        for line in out.splitlines()
        if line.split()[1:2] == ["fastboot"]
    ]
    if USER_SERIAL:
        return USER_SERIAL in serials
    if len(serials) > 1:
        die(MORE_THAN_ONE)
    return bool(serials)


def install_fireos():
    fireos = fetch(FIREOS, FIREOS_URL, FIREOS_SHA)
    magisk = fetch(MAGISK, MAGISK_URL, MAGISK_SHA)
    with tempfile.TemporaryDirectory() as tmp:
        work = pathlib.Path(tmp)
        PROGRESS.begin("formatting data", "5 s")
        if not rscript(work, "data.sh", DATA_SH):
            die("userdata did not format and mount")
        PROGRESS.begin("pushing Fire OS 5.5.5.4 (397 MB)", "80 s")
        push_checked(fireos, "/data/fireos.zip")
        PROGRESS.begin("installing Fire OS 5.5.5.4", "2-3 min")
        out = rshell("twrp install /data/fireos.zip; rm -f /data/fireos.zip")
        if "Error installing zip" in out or "Updater process ended with ERROR" in out:
            die("the Fire OS install failed:\n" + out)

        PROGRESS.begin("patching the boot image", "15 s")
        magiskboot = work / "magiskboot"
        with zipfile.ZipFile(magisk) as z:
            magiskboot.write_bytes(z.read("arm/magiskboot"))
        push_checked(magiskboot, "/tmp/magiskboot")
        if not rscript(work, "boot.sh", BOOT_SH):
            die("patching the boot image failed")
        boot = work / "boot.img"
        run(["adb", "pull", "/tmp/bp/new.img", boot], check=True)
        patch_cmdline(boot)
        data = boot.read_bytes()
        boot.write_bytes(data.ljust(-(-len(data) // 4096) * 4096, b"\0"))
        push_checked(boot, "/tmp/bp/final.img")
        rshell(
            "dd if=/tmp/bp/final.img of=/dev/block/other-boot bs=1048576 2>/dev/null;"
            " sync; echo 3 > /proc/sys/vm/drop_caches"
        )
        blocks = boot.stat().st_size // 4096
        written = rshell(
            f"dd if=/dev/block/other-boot bs=4096 count={blocks} 2>/dev/null | md5sum"
        )
        if written.split("\n")[-1].split(" ")[0] != md5(boot):
            die("the patched boot image did not verify on the Dot")

        PROGRESS.begin("patching /system", "5 s")
        if not rscript(work, "system.sh", SYSTEM_SH):
            die("patching /system failed")

        PROGRESS.begin("installing Magisk 17.3", "20 s")
        push_checked(magisk, "/tmp/magisk.zip")
        if "Done" not in rshell("twrp install /tmp/magisk.zip"):
            die("Magisk did not install")
        db = work / "magisk.db"
        magisk_db(db)
        rshell("mkdir -p /data/adb; chmod 700 /data/adb")
        push_checked(db, "/data/adb/magisk.db")
        rshell("chmod 600 /data/adb/magisk.db; sync")
    PROGRESS.begin("first boot of Fire OS 5", "4 min")
    run(["adb", "reboot"])


def magisk_db(path):
    db = sqlite3.connect(path)
    db.execute(
        "CREATE TABLE policies (uid INT, package_name TEXT, policy INT, "
        "until INT, logging INT, notification INT)"
    )
    db.execute("INSERT INTO policies VALUES (2000, 'com.android.shell', 2, 0, 1, 0)")
    db.commit()
    db.close()


def main():
    parser = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter
    )
    parser.add_argument(
        "--verbose",
        action="store_true",
        help="print each adb and fastboot command, its exit status and the time,"
        " and each state the Dot reaches",
    )
    parser.add_argument(
        "--delay",
        const=10,
        default=0,
        help="count down before each stage, 10 seconds unless given, so a"
        " recording shows the Dot settled in each state; implies --verbose",
        metavar="SECONDS",
        nargs="?",
        type=int,
    )
    options = parser.parse_args()
    ARGS.delay = options.delay
    ARGS.verbose = options.verbose or options.delay > 0
    for tool in ("adb", "fastboot"):
        if not shutil.which(tool):
            die(tool + " not found: install Android platform-tools")
    check_adb()
    usage = run(["fastboot", "--help"], timeout=30).stdout
    if not any(line.split()[:1] == ["-S"] for line in usage.splitlines()):
        die("this fastboot has no -S option: install a newer Android platform-tools")
    done = set()
    guided = False
    seen = None
    shown = None
    deadline = None
    while True:
        current = state()
        if current not in ("none", "starting"):
            ERASED.unlink(missing_ok=True)
        if current == "none" and ERASED.exists() and "bootrom" not in done:
            done.update(("bootrom", "v1-fastboot"))
            say(
                "The last run stopped during the move to amonet v1.1.0. The Dot cannot"
                " start until that move is done, so this run finishes it."
            )
            bootrom(
                unpack(AMONET_V1, MIRROR + "/" + AMONET_V1, AMONET_V1_SHA, "v1"),
                fetch(PYSERIAL, PYSERIAL_URL, PYSERIAL_SHA),
                erase=False,
            )
            v1_recovery()
            seen = None
            continue
        if current != seen:
            seen = current
            deadline = time.monotonic() + WAIT
            if ARGS.verbose and current != shown:
                shown = current
                print(f"{clock()} state: {current}")
            if current not in done and current not in ("none", "starting", "booted"):
                PROGRESS.end()
            if current == "booted" and not PROGRESS.open:
                print("The Dot is starting Fire OS. Waiting for it to finish.")
            elif current == "stock-booted":
                guided = True
                say(
                    "This Dot appears to be unmodified. To unlock and root it, start it in"
                    " fastboot mode. " + FASTBOOT_MODE
                )
            elif current == "none" and not done and guided:
                print("Waiting for the Dot in fastboot mode, with a green ring.")
            elif current == "none" and not done:
                guided = True
                say(
                    "Waiting for a Dot on USB. Connect it with a USB cable. A Dot that"
                    " runs Amazon's own software needs fastboot mode. "
                    + FASTBOOT_MODE
                    + " Ctrl-C stops the script."
                )
        if current == "rooted":
            hide_updater()
            version = rshell("getprop ro.build.version.name")
            selinux = rshell("getenforce")
            print(f"the Dot is rooted: {version}, SELinux {selinux}, {UPDATER} hidden.")
            print("install overdub with deploy/install.sh <name>.")
            cache_note()
            return
        if current in done or current in ("none", "stock-booted", "booted", "starting"):
            if current not in ("none", "stock-booted") and time.monotonic() > deadline:
                if current == "booted":
                    die(
                        "Fire OS has not finished booting with root. Reboot to recovery and run"
                        " dot_root.py again."
                    )
                die(
                    f"the Dot has been {current} for {WAIT // 60} minutes."
                    " Run dot_root.py again."
                )
            time.sleep(2)
            continue
        if not done:
            print("Keep the Dot plugged in until dot_root.py finishes.")
        done.add(current)
        if current == "stock-fastboot":
            fastbrick()
        elif current == "v2-twrp":
            downgrade(from_twrp=True)
            done.add("v1-fastboot")
        elif current == "v2-fastboot":
            downgrade(from_twrp=False)
            done.add("v1-fastboot")
        elif current == "v1-fastboot":
            v1_recovery()
        elif current == "v1-twrp":
            install_fireos()
        seen = None


def md5(path):
    digest = hashlib.md5()
    with path.open("rb") as f:
        for block in iter(lambda: f.read(1 << 20), b""):
            digest.update(block)
    return digest.hexdigest()


def no_port_help():
    if sys.platform.startswith("linux"):
        return (
            "No new /dev/ttyACM* port could be opened. Add yourself to the dialout "
            "group (or run as root), and stop ModemManager if it is running."
        )
    if sys.platform == "darwin":
        return "No new /dev/cu.usbmodem* port appeared."
    return "No new COM port appeared. Windows may need a driver for USB ID 0e8d:0003."


def on_usb(line):
    if os.name != "nt":
        return " usb:" in line
    parts = line.split()
    return (
        parts[1:2] in (["device"], ["recovery"], ["unauthorized"])
        and ":" not in parts[0]
        and not parts[0].startswith("emulator-")
    )


def patch_cmdline(path):
    data = bytearray(path.read_bytes())
    if data[:8] != b"ANDROID!":
        die("the patched boot image is not a boot image")
    cmdline = bytes(data[64:576]).split(b"\0")[0]
    if b"androidboot.selinux=permissive" not in cmdline:
        cmdline = (cmdline + b" androidboot.selinux=permissive").strip()
    if len(cmdline) >= 512:
        die("the boot cmdline is too long")
    data[64:576] = cmdline.ljust(512, b"\0")
    path.write_bytes(data)


def probe():
    if in_fastboot():
        unlock = getvar("unlock_status").lower()
        if unlock == "false":
            return "stock-fastboot"
        if unlock != "true":
            return "starting"
        lk = getvar("lk_build_desc")
        if not lk:
            return "starting"
        if lk == LK_V1:
            return "v1-fastboot"
        return "v2-fastboot"
    if not usb_serial():
        return "none"
    adb_state = run(["adb", "get-state"], timeout=30).stdout
    if "unauthorized" in adb_state:
        return "stock-booted"
    adb_state = adb_state.strip()
    if adb_state == "recovery":
        version = rshell("getprop ro.twrp.version")
        if not version[:1].isdigit():
            return "starting"
        if version.startswith("3.2."):
            if "mtp" not in rshell("getprop sys.usb.config"):
                return "starting"
            return "v1-twrp"
        return "v2-twrp"
    if adb_state == "device":
        booted = rshell("getprop sys.boot_completed") == "1"
        if booted and "uid=0" in rshell("su -c id"):
            return "rooted"
        if rshell("getprop ro.build.version.name").startswith("Fire OS 6"):
            return "stock-booted"
        return "booted"
    return "none"


def push_checked(local, remote):
    for attempt in range(PUSH_TRIES):
        if attempt:
            PROGRESS.note(
                f"{remote} did not arrive; waiting for the Dot to reconnect to try again."
            )
            try:
                run(["adb", "wait-for-recovery"], timeout=120)
            except subprocess.TimeoutExpired:
                die("the Dot did not reconnect over USB in recovery")
            time.sleep(5)
        result = run(["adb", "push", local, remote])
        if result.returncode != 0:
            continue
        if rshell("md5sum " + remote).split(" ")[0] == md5(local):
            return
    die(
        f"{remote} did not arrive intact after {PUSH_TRIES} tries. adb said:\n"
        + result.stdout
    )


def rscript(work, name, body):
    local = work / name
    with local.open("w", newline="\n") as f:
        f.write(body)
    push_checked(local, "/tmp/root-step.sh")
    out = rshell("sh /tmp/root-step.sh; rm -f /tmp/root-step.sh")
    return out.split("\n")[-1] == "root-step-ok"


def rshell(command):
    out = run(["adb", "shell", command]).stdout.replace("\r", "")
    return "\n".join(
        line for line in out.split("\n") if not line.startswith("__bionic_open_tzdata")
    ).strip()


def run(args, timeout=None, cwd=None, check=False):
    if ARGS.verbose and not ARGS.probing:
        print(f"{clock()} $ {' '.join(map(str, args))}")
    result = subprocess.run(
        args,
        check=False,
        cwd=cwd,
        errors="replace",
        stderr=subprocess.STDOUT,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        text=True,
        timeout=timeout,
    )
    if ARGS.verbose and not ARGS.probing:
        print(f"{clock()}   exit {result.returncode}")
    if check and result.returncode != 0:
        die(f"{' '.join(map(str, args))} failed:\n{result.stdout}")
    return result


def save(response, out):
    total = int(response.headers.get("Content-Length") or 0)
    done = 0
    for block in iter(lambda: response.read(1 << 20), b""):
        out.write(block)
        done += len(block)
        if total:
            meter = f"{done / 1e6:.1f} of {total / 1e6:.1f} MB ({100 * done // total}%)"
        else:
            meter = f"{done / 1e6:.1f} MB"
        print("\r  " + meter, end="", flush=True)
    print()


def say(text):
    print(textwrap.fill(text, 79))


def sha256(path):
    digest = hashlib.sha256()
    with path.open("rb") as f:
        for block in iter(lambda: f.read(1 << 20), b""):
            digest.update(block)
    return digest.hexdigest()


def since(start):
    seconds = int(time.monotonic() - start)
    if seconds < 60:
        return f"{seconds}s"
    return f"{seconds // 60}m {seconds % 60}s"


def state():
    ARGS.probing = True
    try:
        current = probe()
    except subprocess.TimeoutExpired:
        current = "starting"
    finally:
        ARGS.probing = False
    return current


def unpack(name, url, want, dirname):
    archive = fetch(name, url, want)
    target = CACHE / dirname
    if not target.is_dir():
        part = CACHE / (dirname + ".part")
        shutil.rmtree(part, ignore_errors=True)
        with zipfile.ZipFile(archive) as z:
            z.extractall(part)
        part.replace(target)
    return target / "amonet"


def usb_serial():
    if USER_SERIAL:
        if ":" in USER_SERIAL:
            die("ANDROID_SERIAL names a network device; this needs the Dot on USB")
        return USER_SERIAL
    out = run(["adb", "devices", "-l"], timeout=30).stdout
    usb = [line.split()[0] for line in out.splitlines()[1:] if on_usb(line)]
    if len(usb) > 1:
        die(MORE_THAN_ONE)
    if usb:
        os.environ["ANDROID_SERIAL"] = usb[0]
        return usb[0]
    os.environ.pop("ANDROID_SERIAL", None)
    return None


def v1_recovery():
    amonet = unpack(AMONET_V1, MIRROR + "/" + AMONET_V1, AMONET_V1_SHA, "v1")
    PROGRESS.begin("v1.1.0 recovery; waiting for a cyan ring", "30 s")
    run(
        ["fastboot", "-S", "256M", "flash", "tee2", "bin/tz.img"],
        check=True,
        cwd=amonet,
    )
    run(
        ["fastboot", "-S", "256M", "flash", "recovery", "bin/twrp.img"],
        check=True,
        cwd=amonet,
    )
    run(["fastboot", "oem", "reboot-recovery"], check=True, cwd=amonet)


if __name__ == "__main__":
    try:
        main()
    except KeyboardInterrupt:
        die("stopped", prefix="")
