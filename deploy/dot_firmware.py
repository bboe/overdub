#!/usr/bin/env python3
"""Change the firmware on an Echo Dot (2nd Generation) over USB: root it, or
return it to stock. It finds where the Dot is, from stock, part way through, or
on another target, and keeps going until the Dot is on the target: it waits
while the Dot reboots, and while you take a step it asks for. Stopped, it picks
up where it left off on the next run. It uses the one Dot on USB; set
ANDROID_SERIAL when several are. It needs Python 3.9 or later, and adb and
fastboot from Android platform-tools. docs/rooting.md says why each step is
there.
"""

from __future__ import annotations

import argparse
import base64
import bz2
import contextlib
import dataclasses
import enum
import gzip
import hashlib
import http.client
import lzma
import os
import pathlib
import platform
import re
import shlex
import shutil
import sqlite3
import stat
import struct
import subprocess
import sys
import tempfile
import textwrap
import threading
import time
import urllib.request
import zipfile
import zlib
from typing import IO, TYPE_CHECKING, BinaryIO, NamedTuple, NoReturn, TextIO

if TYPE_CHECKING:
    from collections.abc import Callable, Iterator

ARGS = argparse.Namespace(build="", target="v1-bboe", verbose=False)
AS_ROOT = (
    "run this as your own user, not as root or with sudo: the downloads would"
    " belong to root, and on Linux udev rules let a user open the Dot"
)
BCB = b"\0ABB\x01\x8f\0"
BCB_OFFSET = 0x360
BLOCK_IMAGES = {
    "boot": "boot.img",
    "lk": "images/lk.bin",
    "preloader": "images/preloader.img",
    "tee": "images/tz.img",
}
BOOT0_EMPTY = (
    "boot0 has no preloader until the last step, so the Dot shows no light and"
    " starts nothing until a run finishes: it waits in its bootrom, and this"
    " script takes it from there by itself when it runs again on this computer"
)
BOOT_ROOT_SHA256 = "de49cc88b27a8e77cf97cf0156bee50e4ddc0e116c41aaede06b494e38397be0"
BOOT_ROOT_URL = (
    "https://xdaforums.com/attachments/boot-root-zip.6388001/"
    "?hash=51efcb2ca8973118855bf9ccae40446b"
)
BROM_PID = 0x0003
BY_NAME = "/dev/block/platform/mtk-msdc.0/by-name"
CHAIN_PARTS = (
    "boot_a",
    "boot_b",
    "lk_a",
    "lk_b",
    "misc",
    "recovery",
    "tee1",
    "tee2",
)
CHAIN_TEE = ("tee2", "tee1")
CMDLINE_SIZE = 512
DISK = "/dev/block/mmcblk0"
DOT_TMP = pathlib.PurePosixPath("/tmp")  # ruff: ignore[hardcoded-temp-file]
EMMC_FIELDS = ("name", "manfid", "oemid", "prv", "life_time", "pre_eol_info")
EMMC_PATTERN = bytes(range(256)) + b"firebreak"
EMMC_SPARE = 16
EMMC_TARGET = "cache"
EMMC_TIMEOUT = 1800
EMOS_USB_ID = (0x1949, 0x2007)
FASTBOOT_MODE = (
    "Unplug the USB cable, press and hold the action button (the one with a dot),"
    " plug the cable back in, and let go when the light ring turns green."
)
FTVDB = "https://ftvdb.com/echo/firmware/com.amazon.biscuit.android.os/"
GPT_HEADER_SIZE = 92
GUID = r"[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}"
GUNZIP_FAILED = "gunzip-failed"
GZIP_MAGIC = b"\x1f\x8b"
HANDSHAKE_WAIT = 10
HEAD_CHECK = 1 << 20
IMAGES = ("preloader", "lk", "tee", "boot", "system")
LK_DESC = re.compile(r"[0-9a-f]{7}-\d{8}_\d{6}")
MD5_DIGITS = 32
MEBI = 1048576
MEDIATEK_VID = 0x0E8D
MEGA = 1e6
MINUTE = 60
MIRROR = "https://github.com/hkfuertes/amazon_device_biscuit/releases/download/none"
MMC_LOG = 'dmesg | grep -iE "mmc|msdc" | tail -60'
MMC_LOG_REACHES_BOOT = "MMC card at address"
MORE_THAN_ONE = (
    "more than one Dot on USB: set ANDROID_SERIAL to one's serial (adb"
    " devices lists them)"
)
NEW_GROUP = """this shell predates its user joining plugdev. Log in again, or run:

adb kill-server
"""
NO_ACCESS = """this user cannot open the Dot over USB. These commands let it:

sudo groupadd -f plugdev
sudo tee /etc/udev/rules.d/51-echo-dot.rules >/dev/null <<'EOF'
SUBSYSTEM=="usb", ATTR{idVendor}=="1949", MODE="0660", GROUP="plugdev", TAG+="uaccess"
SUBSYSTEM=="usb", ATTR{idVendor}=="18d1", ATTR{idProduct}=="4ee2", MODE="0660", GROUP="plugdev", TAG+="uaccess"
SUBSYSTEM=="usb", ATTR{idVendor}=="18d1", ATTR{idProduct}=="d001", MODE="0660", GROUP="plugdev", TAG+="uaccess"
SUBSYSTEM=="usb", ATTR{idVendor}=="0bb4", ATTR{idProduct}=="0c01", MODE="0660", GROUP="plugdev", TAG+="uaccess"
SUBSYSTEM=="usb", ATTR{idVendor}=="0e8d", ATTR{idProduct}=="0003", MODE="0660", GROUP="plugdev", TAG+="uaccess"
SUBSYSTEM=="tty", ATTRS{idVendor}=="0e8d", ATTRS{idProduct}=="0003", MODE="0660", GROUP="plugdev", TAG+="uaccess"
SUBSYSTEM=="tty", ATTRS{idVendor}=="1949", ATTRS{idProduct}=="2007", MODE="0660", GROUP="plugdev", TAG+="uaccess"
EOF
sudo udevadm control --reload
sudo udevadm trigger
sudo usermod -aG plugdev "$USER"
adb kill-server

Then run it again with the new group, which a new login also has:

"""  # ruff: ignore[line-too-long]
PARTITION_FIELDS = 4
PAYLOAD_VERSION = 2
PRELOADER_PID = 0x2000
PUSH_TRIES = 3
REPORT_PROPS = (
    "ro.product.device",
    "ro.build.version.name",
    "ro.build.version.number",
    "ro.build.type",
    "ro.twrp.version",
    "ro.boot.lk_build_desc",
)
SHORT_WAIT = 5
SLICE_OK = "slice-ok"
SPINNER = "\u280b\u2819\u2839\u2838\u283c\u2834\u2826\u2827\u2807\u280f"
STOCK_PARTITIONS = 16
STOCK_STEPS = 16
STREAM_OK = "stream-ok"
SYSTEM_FIELDS = 2
SYSTEM_SLICE = 128
SYSTEM_TIMEOUT = 1800
TABLE_FIELDS = 6
TABLE_OK = "table-ok"
TRANSFER_FIELDS = 2
TWRP_VERSION = "3.7.0_9-bboe2"
TWRP_VERSIONS = ("3.2.", "3.7.")
UPDATER = "com.amazon.device.software.ota"
UPDATE_HOSTS = (
    "updates.amazon.com",
    "softwareupdates.amazon.com",
    "amzndigitaldownloads.edgesuite.net",
    "amzdigital-a.akamaihd.com",
)
USB_TEST_BLOCKS = 128
USB_TEST_COLUMN = 3
USB_TEST_LEAST = 16
USB_TEST_SPARE = 32
USER_SERIAL = os.environ.get("ANDROID_SERIAL")
V1_ALIGN = 0x400
V1_APPEND = 0x6E000
V1_BOOT_BLOCKS = 0x37000
V1_PAYLOAD_SEEK = 223207
V2_BUILD = "8146"
VARINT_MORE = 0x80
WAIT = 600


class ANSIColor(enum.Enum):
    RED = 31
    YELLOW = 33


class Build(NamedTuple):
    ftvdb_version: str
    ns: str
    number: str
    date: str
    md5: str
    sha256: str


BUILDS = {
    "4315": Build(
        date="2023-11-20",
        ftvdb_version="6-5-5-5",
        md5="03c7c4dc338a93635dda0f5e1cd4e451",
        ns="NS6555",
        number="8087722874",
        sha256="c1ca33efd975cb8491ea9438eff95af559c632dd7ca75ff3b73facacce5f758a",
    ),
    "4405": Build(
        date="2023-01-21",
        ftvdb_version="6-5-5-6",
        md5="570f3f6b28f94323e01e95561c87887f",
        ns="NS6556",
        number="8289072516",
        sha256="6839b0a1e5c4f6aa57f89ea7ca67c85aa69f8fe32e5ddc252764b0d96be5bf69",
    ),
    "5041": Build(
        date="2023-04-06",
        ftvdb_version="6-5-0-5",
        md5="1a25d21e0363158fe0c14cb4d41b843e",
        ns="NS6505",
        number="8960323972",
        sha256="80d98d3b57bc654d435b384f096dea46edd2b86e9f8adaaf91a7b0c3001e9716",
    ),
    "6302": Build(
        date="2025-04-13",
        ftvdb_version="6-4-6-6",
        md5="62da0c58f8c3e5c8094f302346da754f",
        ns="NS6466",
        number="11712110212",
        sha256="6d395345b1d1db373f9d714172c2ac5de4b5dd3e52d1ed1c3f37c2f007537667",
    ),
    "8138": Build(
        date="2026-08-31",
        ftvdb_version="6574-1",
        md5="723886117a7a4a8a543c2ec955dcd54e",
        ns="NS65741",
        number="13222529668",
        sha256="d2a61dd2af1d322e9ecfb4643670fbe416bc2442410f1aebe9748e2438f6d55f",
    ),
    "8142": Build(
        date="2026-09-09",
        ftvdb_version="6574-1",
        md5="ead2ea9a9ca2fa1c708381a07c605356",
        ns="NS65741",
        number="13222530692",
        sha256="ed4ddb01cd53e38751bb6274ad1a8255043e0795843d6ac2d44cc2803b2179a0",
    ),
    "8146": Build(
        date="2026-09-17",
        ftvdb_version="6574-1",
        md5="8ca06ee4ef2806c974d2944b7fadd543",
        ns="NS65741",
        number="13222531716",
        sha256="90832e86498c5e803974c30359aea71c1129757be0ddc59d831a94b27f81487f",
    ),
}


class Download(NamedTuple):
    name: str
    sha256: str
    url: str
    folder: str = ""


AMONET_V1 = Download(
    folder="v1",
    name="amonet-biscuit-v1.1.0.zip",
    sha256="bd4d3a18b6b6e9ff6e49a4739159a81020673202795cb3959f7c9ff24351b663",
    url=MIRROR + "/amonet-biscuit-v1.1.0.zip",
)
AMONET_V2 = Download(
    folder="v2",
    name="amonet-biscuit-v2.0.0.zip",
    sha256="98297293701082bc7272efe077f941c56fc7b6e1f27ef6f2e93b6e4c6fc7b62d",
    url=MIRROR + "/amonet-biscuit-v2.0.0.zip",
)
FIREOS = Download(
    name="update-kindle-csm_biscuit-272.6.8.0_user_680767620.bin",
    sha256="6ababc517529938f0d1e836c3410a91df19683ae62d7fca9e2ca57320d5d2faa",
    url="https://d1s31zyz7dcc2d.cloudfront.net/47a1457e0802980eb32f63cd3ce355c0/"
    "update-kindle-csm_biscuit-272.6.8.0_user_680767620.bin",
)
MAGISK = Download(
    name="Magisk-v17.3.zip",
    sha256="18e46b16b25ebe691c282fe311beccd4811cd533848a64e2efbd754fb85efde7",
    url="https://github.com/topjohnwu/Magisk/releases/download/v17.3/Magisk-v17.3.zip",
)
PYSERIAL = Download(
    name="pyserial-3.5-py2.py3-none-any.whl",
    sha256="c4451db6ba391ca6ca299fb3ec7bae67a5c55dde170964c7a14ceefec02f2cf0",
    url="https://files.pythonhosted.org/packages/07/bc/"
    "587a445451b253b285629263eb51c2d8e9bcea4fc97826266d186f96f558/pyserial-3.5-py2.py3-none-any.whl",
)
TWRP = Download(
    name=f"twrp-{TWRP_VERSION}-biscuit.img",
    sha256="f59052713a6580a1477490b2f9cad80e9b31d22408861b18fd442129a71f2ad9",
    url="https://github.com/bboe/twrp_device_amazon_echo-mt8163/releases/download/"
    f"v{TWRP_VERSION}/twrp-v{TWRP_VERSION}-biscuit.img",
)
LOCKS = {
    key: threading.Lock()
    for key in (
        AMONET_V1,
        AMONET_V2,
        FIREOS,
        MAGISK,
        PYSERIAL,
        TWRP,
        *BUILDS,
        "v1",
        "v2",
    )
}


class ExtentField(enum.IntEnum):
    START_BLOCK = 1
    NUM_BLOCKS = 2


class InfoField(enum.IntEnum):
    SIZE = 1
    HASH = 2


class Kind(enum.Enum):
    ERROR = "error"
    INFO = "info"
    WARN = "warn"


class LK(enum.Enum):
    V1 = "f379dba-20170906_000423"
    V2 = "63cb91b-20221007_072309"
    V2_MIGRATE = "41fb3ce-20221007_151724"


class ManifestField(enum.IntEnum):
    BLOCK_SIZE = 3
    PARTITIONS = 13


class OperationField(enum.IntEnum):
    TYPE = 1
    DATA_OFFSET = 2
    DATA_LENGTH = 3
    DST_EXTENTS = 6


class OperationType(enum.IntEnum):
    REPLACE = 0
    REPLACE_BZ = 1
    REPLACE_XZ = 8


class Partition(NamedTuple):
    number: int
    first: int
    sectors: int

    @property
    def size(self) -> int:
        return self.sectors * 512


class PartitionField(enum.IntEnum):
    NAME = 1
    NEW_INFO = 7
    OPERATIONS = 8


class Progress:
    def __init__(self) -> None:
        self.t0 = time.monotonic()
        self.ts = self.t0
        self.line = ""
        self.open = False
        self.step = 0
        self.steps = 0
        self.stopped = threading.Event()
        self.ticker = None

    def begin(self, *, estimate: str = "", label: str) -> None:
        self.end()
        self.step += 1
        about = f"(~{estimate})" if estimate else ""
        total = str(self.steps or "?")
        self.line = f"[{self.step:>{len(total)}}/{total}] {label:<38} {about:<8} "
        self.ts = time.monotonic()
        self.open = True
        if ARGS.verbose:
            show(text=f"{clock()} {self.line.rstrip()}")
        elif not sys.stdout.isatty():
            show(text=self.line.rstrip())
        else:
            self.start()

    def end(self, *, skipped: bool = False) -> None:
        if not self.open:
            return
        self.open = False
        back = "\r" if self.halt() else ""
        took = "skip" if skipped else self.seconds()
        total = f"({since(self.t0)} total)"
        stamp = f"{clock()} " if ARGS.verbose else ""
        show(text=f"{back}{stamp}{self.line}{mark()} {took} {total:>15}")

    def halt(self) -> bool:
        if not self.ticker:
            return False
        self.stopped.set()
        self.ticker.join()
        self.ticker = None
        return True

    def note(self, message: str) -> None:
        running = self.halt()
        if running:
            print()
        warn(message)
        if running:
            self.start()

    def seconds(self) -> str:
        return f"{int(time.monotonic() - self.ts):3d}s"

    def start(self) -> None:
        show(end="", flush=True, text=self.line)
        self.stopped.clear()
        self.ticker = threading.Thread(daemon=True, target=self.tick)
        self.ticker.start()

    def tick(self) -> None:
        width = 2 if mark() == "✅" else len(mark())
        frames = SPINNER if mark() == "✅" else "|/-\\"
        count = 0
        while not self.stopped.wait(0.1):
            frame = frames[count % len(frames)]
            show(
                end="",
                flush=True,
                text=f"\r{self.line}{frame:<{width}} {self.seconds()}",
            )
            count += 1


@dataclasses.dataclass
class Session:
    carried: int = 0
    carries: bool | None = None
    dd: str = "dd"
    probing: bool = False
    short: bool = False
    shown: Kind | None = None
    system_lock: threading.Lock = dataclasses.field(default_factory=threading.Lock)
    verified: set[pathlib.Path] = dataclasses.field(default_factory=set)
    writing: bool = False


SESSION = Session()


class Shell(enum.Enum):
    EMMC = (
        "umount /cache 2>/dev/null;"
        " if mountpoint -q /cache; then echo cache is still mounted; exit 8; fi;"
        " if [ ! -b {node} ]; then echo {node} is not a block device; exit 7; fi;"
        " mke2fs -q -t ext4 -b 4096 -O ^sparse_super,^resize_inode"
        " -E packed_meta_blocks=1 -J size=4 -N 8192 {node} || exit 6;"
        " i=0;"
        " while [ $i -lt {rounds} ]; do"
        " {dd} if={source} of={node} bs=1048576"
        " seek=$(( {spare} + i * {chunk} )){notrunc}"
        " || exit 9; i=$(( i + 1 )); done;"
        " sync; echo 3 > /proc/sys/vm/drop_caches;"
        " echo card:$({dd} if={node} bs=1048576 skip={spare} count={blocks}"
        " 2>/dev/null | md5sum)"
    )
    EMMC_FORMAT = (
        "mke2fs -q -t ext4 -b 4096 {node}"
        " $(( $(blockdev --getsize64 {node}) / 4096 - 256 )) && echo formatted"
    )
    CLEAR_BOOT0 = (
        "d=dd; toybox dd --help >/dev/null 2>&1 && d='toybox dd'; "
        "echo 0 > /sys/block/mmcblk0boot0/force_ro; "
        "$d if=/dev/zero of=/dev/block/mmcblk0boot0 bs=4096 count=1 2>/dev/null; "
        "echo 1 > /sys/block/mmcblk0boot0/force_ro; sync; "
        "echo 3 > /proc/sys/vm/drop_caches; "
        'echo "$($d if=/dev/block/mmcblk0boot0 bs=4096 count=1 2>/dev/null | wc -c)'
        " $($d if=/dev/block/mmcblk0boot0 bs=4096 count=1 2>/dev/null"
        " | tr -d '\\0' | wc -c)\""
    )
    BOOT0 = (
        "d=dd; toybox dd --help >/dev/null 2>&1 && d='toybox dd'; "
        "echo 0 > /sys/block/mmcblk0boot0/force_ro; "
        "$d if={src} of=/dev/block/mmcblk0boot0 bs=1048576 2>/dev/null; "
        "echo 1 > /sys/block/mmcblk0boot0/force_ro; sync; "
        "echo 3 > /proc/sys/vm/drop_caches"
    )
    DATA = f"""\
set -e
umount /sdcard /data 2>/dev/null || true
d={BY_NAME}/userdata
mke2fs -q -t ext4 -b 4096 "$d" $(( $(blockdev --getsize64 "$d") / 4096 - 256 ))
mount -t ext4 "$d" /data
mountpoint -q /data
"""
    FLUSH = "sync && echo 3 > /proc/sys/vm/drop_caches && echo flushed"
    MAGISK = """\
set -e
mountpoint -q /data
cd /; cpio -idu < /tmp/magisk.cpio 2>/dev/null
chmod 700 /data/adb; chmod -R 755 /data/adb/magisk; chmod 600 /data/adb/magisk.db
sync
"""
    SYSTEM_STREAM = (
        "rm -f /tmp/gunzip-failed;"
        " ( gunzip -c || touch /tmp/gunzip-failed )"
        " | {dd} of={system} bs=1048576{notrunc} || exit 9;"
        " [ -f /tmp/gunzip-failed ] && echo gunzip-failed || echo stream-ok"
    )
    SYSTEM_SLICE = (
        "rm -f /tmp/gunzip-failed;"
        " ( gunzip -c {held} || touch /tmp/gunzip-failed )"
        " | {dd} of={system} bs=1048576 seek={seek}{notrunc} || exit 9;"
        " rm -f {held};"
        " [ -f /tmp/gunzip-failed ] && echo gunzip-failed || echo slice-ok"
    )
    SYSTEM = """\
set -e
m=/tmp/fireos-system
mkdir -p $m
mountpoint -q $m || mount -t ext4 {system} $m
f=$m/etc/init.fosflags.sh
sed -i 's/if \\[ $(( $FOS_FLAGS_ADB_ON & $FOSFLAGS )) != 0 \\]; then/if true; then/; \
s/^\\( *\\)unset_adb_persistent_property$/\\1true/' "$f"
for h in {hosts}; do
  grep -q " $h\\$" $m/etc/hosts || echo "127.0.0.1 $h" >> $m/etc/hosts
done
grep -q 'if true; then' "$f"
grep -q '^ *unset_adb_persistent_property$' "$f" && exit 1
sync; umount $m
"""
    TOOLS = (
        "m=; for t in sgdisk mke2fs blockdev md5sum; do"
        ' command -v "$t" >/dev/null 2>&1 || which "$t" >/dev/null 2>&1'
        ' || m="$m $t"; done; echo "tools:$m"'
    )
    UNMOUNT = (
        'for m in $(grep "^/dev/block" /proc/mounts | cut -d" " -f2); do umount "$m";'
        ' done; echo "left:$(grep "^/dev/block" /proc/mounts | cut -d" " -f2'
        ' | tr "\\n" " ")"'
    )
    NODES = (
        "b=; for p in {pairs}; do n=${{p%:*}}; s=${{p#*:}};"
        ' g=$([ -b "$n" ] && blockdev --getsize64 "$n" || echo no);'
        ' [ "$g" = "$s" ] || b="$b $n=$g"; done; echo "$b nodes-ok"'
    )
    SEEK_WRITE = (
        "dd if={src} of={dst} bs=512 seek={seek} 2>/dev/null;"
        " sync; echo 3 > /proc/sys/vm/drop_caches"
    )
    WRITE = (
        "dd if={src} of={dst} bs=1048576 2>/dev/null;"
        " sync; echo 3 > /proc/sys/vm/drop_caches"
    )


class Stage(NamedTuple):
    run: Callable[[], None]
    steps: int
    then: State | None
    passes: frozenset[State] = frozenset()


class State(enum.Enum):
    AMONET_V1_TWRP = "amonet-v1-twrp"
    AMONET_V2_TWRP = "amonet-v2-twrp"
    AMONET_V2_TWRP_V1_TABLE = "amonet-v2-twrp-v1-table"
    BBOE_V1_TWRP = "bboe-v1-twrp"
    BOOTED = "booted"
    EMOS = "emos"
    NONE = "none"
    ROOTED = "rooted"
    ROOTED_BBOE = "rooted-bboe"
    ROOTED_V1 = "rooted-v1"
    STARTING = "starting"
    STOCK_BOOTED = "stock-booted"
    STOCK_FASTBOOT = "stock-fastboot"
    LOCKED_V1_FASTBOOT = "locked-v1-fastboot"
    V1_FASTBOOT = "v1-fastboot"
    V2_BOOTED = "v2-booted"
    V2_FASTBOOT = "v2-fastboot"


GOALS = {
    "stock": State.STOCK_BOOTED,
    "v1": State.ROOTED_V1,
    "v1-bboe": State.ROOTED_BBOE,
    "v2": State.V2_BOOTED,
}
ROOTED = {State.ROOTED, State.ROOTED_BBOE, State.ROOTED_V1}


TWRPS = frozenset({
    State.AMONET_V1_TWRP,
    State.AMONET_V2_TWRP,
    State.AMONET_V2_TWRP_V1_TABLE,
    State.BBOE_V1_TWRP,
})


class Step(NamedTuple):
    image: str
    partition: str
    label: str
    estimate: str


WRITES = (
    Step(
        estimate="4 min",
        image="system",
        label="write the system image to system_a",
        partition="system_a",
    ),
    Step(
        estimate="4 min",
        image="system",
        label="write the system image to system_b",
        partition="system_b",
    ),
    Step(
        estimate="10 s",
        image="boot",
        label="write boot image to boot_a",
        partition="boot_a",
    ),
    Step(
        estimate="10 s",
        image="boot",
        label="write boot image to boot_b",
        partition="boot_b",
    ),
    Step(
        estimate="5 s",
        image="misc",
        label="write misc, slot a marked good",
        partition="misc",
    ),
)


class WireType(enum.IntEnum):
    VARINT = 0
    FIXED64 = 1
    LEN = 2
    FIXED32 = 5


def _die(*, message: str, prefix: str = "ERROR: ") -> NoReturn:
    if PROGRESS.halt():
        print()
    if SESSION.shown not in {None, Kind.ERROR}:
        print()
    SESSION.shown = Kind.ERROR
    text = prefix + message
    if "\n" not in text:
        text = textwrap.fill(text, 79)
    if SESSION.writing and ERASED.exists():
        text += "\n\n" + textwrap.fill(BOOT0_EMPTY + ".", 79)
    if prefix and color(sys.stderr):
        text = f"\033[{ANSIColor.RED.value}m{text}\033[0m"
    raise SystemExit(text)


def adb_script(*, body: str, name: str, work: pathlib.Path) -> bool:
    local = work / name
    with local.open("w", newline="\n") as f:
        f.write(body)
    remote = DOT_TMP / "root-step.sh"
    push_checked(local=local, remote=remote)
    command = f"sh {remote}; s=$?; rm -f {remote}; exit $s"
    return run(args=["adb", "shell", command], timeout=300).returncode == 0


def adb_shell(*, command: str, timeout: float = 300) -> str:
    out = run(args=["adb", "shell", "-n", command], timeout=timeout).stdout
    return "\n".join(
        line for line in out.split("\n") if not line.startswith("__bionic_open_tzdata")
    ).strip()


def again() -> str:
    return f"Run {shlex.join(['dot_firmware.py', *sys.argv[1:]])} again."


def amonet_chain() -> None:
    part = partitions()
    if "lk_a" not in part:
        _die(message="the Dot's partition table has no lk_a. " + again())
    node = f"{DISK}p{part['lk_a'][0]}"
    for download in (AMONET_V2, AMONET_V1):
        local = unpack(download) / "bin" / "lk.bin"
        want = digest(kind="md5", path=local)
        for attempt in range(PUSH_TRIES):
            read = adb_shell(
                command=f"[ -b {node} ] && dd if={node} bs={local.stat().st_size}"
                " count=1 2>/dev/null | md5sum",
                timeout=120,
            )
            held = read.split("\n")[-1].split(" ")[0]
            if held == want:
                return
            if len(held) == MD5_DIGITS:
                break
            if attempt + 1 < PUSH_TRIES:
                reconnect(DISK)
        if len(held) != MD5_DIGITS:
            _die(
                message=f"{node} did not answer with an md5 of its first"
                f" {local.stat().st_size} bytes: {held or 'nothing'}. Nothing"
                " was written. " + again() + " " + asked_for()
            )
    _die(
        message="lk_a holds neither amonet v2.0.0's nor v1.1.0's LK, so this Dot"
        " is locked and the recovery it started is one amonet left behind."
        " Nothing was written. Unlock it first: unplug the USB cable, hold the"
        " action button, plug it back in, and let go when the ring turns green. "
        + again()
    )


def asked_for() -> str:
    return (
        "If it fails the same way, run dot_firmware.py --report and paste what"
        " it prints into an issue: it says what this Dot's eMMC and partitions"
        " are. dot_firmware.py --write-test then tells a failing card from a"
        " failing cable."
    )


def boot_image(*, fireos: pathlib.Path, magisk: pathlib.Path) -> bytes:
    with zipfile.ZipFile(fireos) as z:
        stock = z.read("boot.img")
    with zipfile.ZipFile(magisk) as z:
        magiskinit = z.read("arm/magiskinit")
    kernel_size, ramdisk_size, page = (
        struct.unpack_from("<I", stock, offset)[0] for offset in (8, 16, 36)
    )
    header = bytearray(stock[:page])
    cmdline = bytes(header[64:576]).split(b"\0")[0]
    header[64:576] = (cmdline + b" androidboot.selinux=permissive").ljust(
        CMDLINE_SIZE, b"\0"
    )
    kernel = stock[page : page + kernel_size]
    start = page + -(-kernel_size // page) * page
    files = cpio_files(gzip.decompress(stock[start : start + ramdisk_size]))
    mode, prop = files[b"default.prop"]
    prop = re.sub(rb"(?m)^ro\.secure=1$", b"ro.secure=0", prop)
    prop = re.sub(rb"(?m)^ro\.debuggable=0$", b"ro.debuggable=1", prop)
    prop = re.sub(
        rb"(?m)^persist\.sys\.usb\.config=.*", b"persist.sys.usb.config=mtp,adb", prop
    )
    files[b"default.prop"] = (mode, prop)
    for name, (mode, body) in files.items():
        if name.startswith(b"fstab"):
            files[name] = (mode, body.replace(b",verify", b"").replace(b"verify,", b""))
    files[b".backup"] = (0o40000, b"")
    files[b".backup/.magisk"] = (
        0o100000,
        b"KEEPVERITY=false\nKEEPFORCEENCRYPT=false\n\0",
    )
    files[b".backup/init"] = files[b"init"]
    files[b".backup/verity_key"] = files.pop(b"verity_key")
    files[b"init"] = (0o100750, magiskinit)
    return boot_pack(
        header=header,
        kernel=magisk_kernel(kernel),
        ramdisk=gzip.compress(cpio(files), compresslevel=9, mtime=0),
    )


def boot_pack(*, header: bytes | bytearray, kernel: bytes, ramdisk: bytes) -> bytes:
    page = len(header)
    sizes = struct.pack("<I", len(kernel)), struct.pack("<I", len(ramdisk))
    out = bytearray(header)
    out[8:12], out[16:20] = sizes
    sha1 = hashlib.sha1(
        kernel + sizes[0] + ramdisk + sizes[1] + bytes(4), usedforsecurity=False
    )
    out[576:608] = sha1.digest().ljust(32, b"\0")
    for part in (kernel, ramdisk):
        out += part.ljust(-(-len(part) // page) * page, b"\0")
    return bytes(out)


def boot_root() -> pathlib.Path:
    CACHE.mkdir(exist_ok=True, parents=True)
    path = CACHE / "boot-root.zip"
    downloaded = pathlib.Path.home() / "Downloads" / path.name
    asked = False
    while not path.is_file() or digest(kind="sha256", path=path) != BOOT_ROOT_SHA256:
        if (
            downloaded.is_file()
            and digest(kind="sha256", path=downloaded) == BOOT_ROOT_SHA256
        ):
            shutil.copyfile(downloaded, path)
            continue
        if not asked:
            asked = True
            say(
                code=ANSIColor.YELLOW,
                text="root v2 needs boot-root.zip, and XDA serves it only to a"
                " browser. Download it in a browser from the address below. This"
                f" waits until it is in {downloaded.parent} or {CACHE}, with the"
                f" sha256 {BOOT_ROOT_SHA256}. Ctrl-C stops the script.",
            )
            show(text=BOOT_ROOT_URL)
        time.sleep(2)
    if asked:
        passed(f"{path.name} verified")
    return path


def bootrom(  # ruff: ignore[complex-structure, too-many-branches, too-many-statements]
    *,
    amonet: pathlib.Path,
    erase: Callable[[], str] | None,
    payload: pathlib.Path,
    wheel: pathlib.Path,
) -> bool:
    shutil.copyfile(payload, amonet / "brom-payload" / "build" / "payload.bin")
    log_path = CACHE / "bootrom.log"
    env = dict(os.environ, PYTHONPATH=str(wheel), PYTHONUNBUFFERED="1")
    if SESSION.short:
        env["OVERDUB_ERASED"] = str(ERASED)
    if not erase and not SESSION.short:
        env["OVERDUB_RESUME"] = "1"
        with contextlib.suppress(subprocess.TimeoutExpired):
            subprocess.run(
                child("reset"),
                check=False,
                env=env,
                stderr=subprocess.DEVNULL,
                stdin=subprocess.DEVNULL,
                stdout=subprocess.DEVNULL,
                timeout=30,
            )
    with log_path.open("w") as log:
        if ARGS.verbose:
            show(
                text=f"{clock()} $ {shlex.join(child('bootrom'))} (amonet v1.1.0"
                " bootrom step, 64 blocks per write)"
            )
        brom = subprocess.Popen(
            child("bootrom"),
            cwd=amonet / "modules",
            env=env,
            stderr=subprocess.STDOUT,
            stdin=subprocess.PIPE,
            stdout=log,
        )
        if not SESSION.short:
            brom.stdin.write(b"\n" * 5)
            brom.stdin.close()
        started = time.monotonic()
        while time.monotonic() - started < (30 if SESSION.short else 3):
            if brom.poll() is not None or (
                SESSION.short
                and "Waiting for bootrom" in log_path.read_text(errors="replace")
            ):
                break
            time.sleep(0.25)
        if brom.poll() is not None:
            _die(message=f"v1.1.0's bootrom step did not start; see {log_path}")
        if erase:
            ERASED.touch()
            try:
                failure = erase()
            except subprocess.TimeoutExpired:
                brom.kill()
                raise
            if failure:
                brom.kill()
                _die(message=failure)
        elif SESSION.short:
            say(
                code=ANSIColor.YELLOW,
                text="Short the Dot's test point and plug it in. The run waits for"
                " its bootrom, then says when the short may come off.",
            )
            status("Waiting for the bootrom.")
        else:
            PROGRESS.begin(estimate="40 s", label="waiting for the Dot to restart")
        deadline = time.monotonic() + (60 if erase else 600)
        missed = unopened = 0
        while brom.poll() is None and time.monotonic() < deadline:
            text = log_path.read_text(errors="replace")
            if "Found port" in text:
                break
            if not erase and not SESSION.short and "Ignoring the preloader" in text:
                brom.kill()
                ERASED.unlink(missing_ok=True)
                PROGRESS.note(
                    "The Dot started its preloader, so boot0 is intact and it"
                    " needs no bootrom step."
                )
                return False
            if SESSION.short and text.count("Ignoring the preloader") > missed:
                missed = text.count("Ignoring the preloader")
                if sys.stdout.isatty():
                    print()
                said = (
                    f"Short {missed} missed: the Dot started normally. Unplug, short,"
                    " and plug it in again."
                )
                show(kind=Kind.WARN, text=paint(code=ANSIColor.RED, text=said))
                status("Waiting for the bootrom.")
            if text.count("Cannot open") > unopened:
                unopened = text.count("Cannot open")
                if SESSION.short and sys.stdout.isatty():
                    print()
                said = "Cannot open" + text.split("Cannot open")[-1].splitlines()[0]
                said += ". The run keeps trying."
                if SESSION.short:
                    warn(said)
                    status("Waiting for the bootrom.")
                else:
                    PROGRESS.note(said)
            time.sleep(1)
        else:
            if brom.poll() is None:
                brom.kill()
                _die(
                    message="the Dot's bootrom did not show up as a serial port.\n"
                    + no_port_help()
                )
        if SESSION.short and brom.poll() is None:
            countdown()
            with contextlib.suppress(OSError):
                brom.stdin.write(b"\n" * 5)
                brom.stdin.close()
        if not erase:
            PROGRESS.begin(estimate="30 s", label="writing amonet v1.1.0's bootloader")
        deadline = time.monotonic() + 1800
        unanswered = 0
        while brom.poll() is None:
            if time.monotonic() > deadline:
                brom.kill()
                _die(message=f"v1.1.0's bootrom step did not finish; see {log_path}")
            text = log_path.read_text(errors="replace")
            if text.count("did not answer the handshake") > unanswered:
                unanswered = text.count("did not answer the handshake")
                if SESSION.short:
                    brom.kill()
                    _die(
                        message="the Dot's bootrom did not answer."
                        + ("" if ERASED.exists() else " Nothing was written.")
                        + " Unplug the Dot. "
                        + again()
                    )
                PROGRESS.note(
                    "The Dot's bootrom did not answer. Unplug the Dot and plug it"
                    " back in; the run goes on when its bootrom returns."
                )
            if text.count("Cannot open") > unopened:
                unopened = text.count("Cannot open")
                said = "Cannot open" + text.split("Cannot open")[-1].splitlines()[0]
                PROGRESS.note(said + ". The run keeps trying.")
            time.sleep(1)
    if brom.returncode != 0:
        said = log_path.read_text(errors="replace")
        if SESSION.short and (
            "The eMMC did not answer" in said or "expected pattern" in said
        ):
            _die(
                message="the Dot's eMMC did not answer, most likely because the"
                " short was still on."
                + ("" if ERASED.exists() else " Nothing was written.")
                + " Unplug the Dot. "
                + again()
            )
        _die(message=f"v1.1.0's bootrom step failed; see {log_path}")
    if "Reboot to unlocked fastboot" not in log_path.read_text(errors="replace"):
        _die(message=f"v1.1.0's bootrom step did not finish; see {log_path}")
    ERASED.unlink(missing_ok=True)
    for _ in range(30):
        try:
            if in_fastboot() and getvar("lk_build_desc") == LK.V1.value:
                break
        except subprocess.TimeoutExpired:
            pass
        time.sleep(2)
    else:
        _die(message="the Dot did not come back in v1.1.0's fastboot")
    return True


def build_system(target: pathlib.Path) -> None:
    part = CACHE / "system.part"
    shutil.rmtree(part, ignore_errors=True)
    part.mkdir()
    checksum = hashlib.md5(usedforsecurity=False)
    fireos = fetch(FIREOS)
    with zipfile.ZipFile(fireos) as z:
        words = z.read("system.transfer.list").decode().split()
        commands = dict(zip(words[4::2], words[5::2]))
        if words[0] != "3" or set(commands) != {"erase", "new"}:
            _die(message=f"{FIREOS.name} has a transfer list this does not read")
        blocks = int(commands["erase"].split(",")[-1])
        bounds = [int(n) for n in commands["new"].split(",")[1:]]
        ranges = [*zip(bounds[::2], bounds[1::2]), (blocks, blocks)]
        with z.open("system.new.dat") as dat:
            slices = sliced(
                checksum=checksum,
                chunks=system_chunks(dat=dat, ranges=ranges),
                part=part,
            )
    (part / "md5").write_text(f"{checksum.hexdigest()} {blocks} {slices}\n")
    for path in part.iterdir():
        with path.open("rb+") as f:
            os.fsync(f.fileno())
    part.replace(target)


def cache_dir() -> pathlib.Path:
    if os.name == "nt":
        base = os.environ.get("LOCALAPPDATA") or pathlib.Path.home()
    else:
        base = os.environ.get("XDG_CACHE_HOME") or pathlib.Path.home() / ".cache"
    return pathlib.Path(base) / "overdub-firmware"


CACHE = cache_dir()


ERASED = CACHE / "boot0-erased"


def cache_note() -> None:
    if CACHE.is_dir():
        size = sum(f.stat().st_size for f in CACHE.rglob("*") if f.is_file())
        path, home = str(CACHE), str(pathlib.Path.home())
        if os.name != "nt" and path.startswith(home + os.sep):
            path = "~" + path[len(home) :]
        show(
            text=f"{path} holds {size // 1000000} MB of downloads and images for"
            " the next run. It is safe to delete."
        )


def chain_nodes() -> tuple[dict[str, str], dict[str, tuple[int, int, int]]]:
    part = partitions()
    for name in (*CHAIN_PARTS, "boot_a_x", "boot_b_x"):
        if name not in part:
            _die(
                message=f"the Dot's partition table has no {name}. Rebuild it"
                f" with dot_firmware.py stock {V2_BUILD}, then root again."
            )
    for name in ("boot_a", "boot_b"):
        held = part[name][2] - part[name][1] + 1
        if held != V1_BOOT_BLOCKS:
            _die(message=f"{name} holds {held} blocks, not {V1_BOOT_BLOCKS}")
    node = {name: f"{DISK}p{part[name][0]}" for name in CHAIN_PARTS}
    wrong = check_nodes(
        want={node[name]: (part[name][2] - part[name][1] + 1) * 512 for name in node}
    )
    if wrong:
        run(args=["adb", "reboot", "recovery"], check=False, timeout=60)
        _die(
            message="the kernel does not hold the table that is on the disk:"
            f" {wrong}. A write by that name could land in RAM and verify"
            " against itself, so the Dot is restarting into recovery. " + again()
        )
    if (
        adb_shell(command="[ -b /dev/block/mmcblk0boot0 ] && echo block").split("\n")[
            -1
        ]
        != "block"
    ):
        _die(
            message="/dev/block/mmcblk0boot0 is not a block device, so the"
            " preloader would be written to a file in RAM. " + again()
        )
    return node, part


def chain_writes() -> tuple[Step, ...]:
    live = (
        "lk_b" if adb_shell(command="getprop ro.boot.slot_suffix") == "_b" else "lk_a"
    )
    spare = "lk_a" if live == "lk_b" else "lk_b"
    first, last = CHAIN_TEE
    return (
        Step(
            estimate="5 s",
            image="lk",
            label=f"write LK image to {spare} (spare slot)",
            partition=spare,
        ),
        Step(
            estimate="5 s",
            image="tee",
            label=f"write TEE image to {first} (backup)",
            partition=first,
        ),
        Step(
            estimate="5 s",
            image="expdb",
            label="zero expdb (amonet kaeru payload)",
            partition="expdb",
        ),
        Step(
            estimate="5 s",
            image="lk",
            label=f"write LK image to {live} (live slot)",
            partition=live,
        ),
        Step(
            estimate="5 s",
            image="tee",
            label=f"write TEE image to {last} (primary)",
            partition=last,
        ),
    )


def check_adb() -> None:
    words = run(args=["adb", "version"], timeout=30).stdout.split()
    version = (
        words[4] if words[:4] == ["Android", "Debug", "Bridge", "version"] else "?"
    )
    parts = version.split(".")
    if not all(part.isdigit() for part in parts) or tuple(map(int, parts)) < (1, 0, 36):
        _die(
            message=f"adb reports version {version}; this needs 1.0.36"
            " (platform-tools r24) or newer"
        )


def check_nodes(*, want: dict[str, int]) -> str:
    command = Shell.NODES.value.format(
        pairs=" ".join(f"{node}:{size}" for node, size in want.items())
    )
    for attempt in range(PUSH_TRIES):
        said = adb_shell(command=command, timeout=60).split("\n")[-1]
        if said.endswith("nodes-ok"):
            read = [token.partition("=") for token in said[: -len("nodes-ok")].split()]
            if all(node in want for node, _, _ in read):
                return ", ".join(
                    f"{node} reads {got}, not {want[node]} bytes"
                    for node, _, got in read
                )
        if attempt + 1 < PUSH_TRIES:
            reconnect(DISK)
    _die(
        message="the Dot did not answer which of its partitions are block"
        f" devices: {said!r}. " + again()
    )
    return ""


def check_user() -> None:
    if os.name != "nt" and os.geteuid() == 0:
        _die(message=AS_ROOT)
    if not sys.platform.startswith("linux"):
        return
    import grp  # ruff: ignore[import-outside-top-level]
    import pwd  # ruff: ignore[import-outside-top-level]

    try:
        plugdev = grp.getgrnam("plugdev")
    except KeyError:
        _die(message=NO_ACCESS + rerun())
    if plugdev.gr_gid in os.getgroups():
        return
    if pwd.getpwuid(os.getuid()).pw_name in plugdev.gr_mem:
        _die(message=NEW_GROUP + rerun())
    _die(message=NO_ACCESS + rerun())


def child(name: str, *args: str) -> list[str]:
    return [
        sys.executable,
        str(pathlib.Path(__file__).resolve()),
        "_child",
        name,
        *args,
    ]


def child_bootrom() -> int:  # ruff: ignore[complex-structure, too-many-statements]
    sys.path.insert(0, str(pathlib.Path.cwd()))
    import common  # ruff: ignore[import-outside-top-level]
    import main as amonet  # ruff: ignore[import-outside-top-level]
    import serial  # ruff: ignore[import-outside-top-level]
    from logger import log  # ruff: ignore[import-outside-top-level]
    from serial.tools import list_ports  # ruff: ignore[import-outside-top-level]

    marker = os.environ.get("OVERDUB_ERASED")
    resume = os.environ.get("OVERDUB_RESUME")
    start_payload = amonet.load_payload

    def emmc_read(self: common.Device, idx: int) -> bytes:
        self.dev.write(struct.pack(">III", 0xF00DD00D, 0x1000, idx))
        return read_flushed(self, 0x200)

    def emmc_write_blocks(self: common.Device, idx: int, data: bytes) -> None:
        self.dev.write(
            struct.pack(">IIII", 0xF00DD00D, 0x1003, idx, len(data) // 0x200)
        )
        self.dev.write(data)
        if self.dev.read(4) != b"\xd0\xd0\xd0\xd0":
            msg = "device failure"
            raise RuntimeError(msg)

    def flash_data(
        dev: common.Device, data: bytes, start_block: int, max_size: int = 0
    ) -> None:
        if marker:
            pathlib.Path(marker).touch()
        data += b"\0" * (-len(data) % 0x200)
        if max_size and len(data) > max_size:
            msg = "data too big to flash"
            raise RuntimeError(msg)
        for x in range(0, len(data), 64 * 0x200):
            dev.emmc_write_blocks(start_block + x // 0x200, data[x : x + 64 * 0x200])

    def read_flushed(self: common.Device, size: int) -> bytes:
        self.dev.write(struct.pack(">IIII", 0xF00DD00D, 0x5000, 0x201000, 4))
        data = self.dev.read(size + 4)
        if len(data) != size + 4:
            msg = "read fail"
            raise RuntimeError(msg)
        return data[:size]

    def rpmb_read(self: common.Device) -> bytes:
        self.dev.write(struct.pack(">II", 0xF00DD00D, 0x2000))
        return read_flushed(self, 0x100)

    def find_device(self: common.Device, *_: object) -> None:
        seen = {
            p.device: p.pid
            for p in list_ports.comports()
            if p.vid == MEDIATEK_VID and not (resume and p.pid == BROM_PID)
        }
        failed = {}
        log("Waiting for bootrom")
        while True:
            pids = {
                p.device: p.pid for p in list_ports.comports() if p.vid == MEDIATEK_VID
            }
            seen = {port: pid for port, pid in seen.items() if port in pids}
            failed = {port: at for port, at in failed.items() if port in pids}
            for port, pid in sorted(pids.items()):
                if pid is None or seen.get(port) == pid:
                    continue
                if pid == BROM_PID:
                    try:
                        self.dev = serial.Serial(
                            port, common.BAUD, timeout=common.TIMEOUT
                        )
                    except serial.SerialException as e:
                        at = failed.setdefault(port, time.monotonic())
                        if at and time.monotonic() - at >= 1:
                            failed[port] = 0
                            log("Cannot open " + port + ": " + str(e))
                        continue
                    log("Found port = " + port)
                    return
                seen[port] = pid
                if pid == PRELOADER_PID:
                    log("Ignoring the preloader on " + port)
            time.sleep(0.25)

    def answered(self: common.Device) -> bool:
        deadline = time.monotonic() + HANDSHAKE_WAIT
        try:
            while time.monotonic() < deadline:
                if self._writeb(b"\xa0") == b"\x5f":
                    return True
                self.dev.flushInput()
        except serial.SerialException:
            pass
        return False

    def handshake(self: common.Device) -> None:
        while not answered(self):
            log("The bootrom did not answer the handshake")
            port = self.dev.port
            with contextlib.suppress(serial.SerialException):
                self.dev.close()
            self.dev = None
            while any(p.device == port for p in list_ports.comports()):
                time.sleep(0.25)
            find_device(self)
        self.check(self._writeb(b"\x0a"), b"\xf5")
        self.check(self._writeb(b"\x50"), b"\xaf")
        self.check(self._writeb(b"\x05"), b"\xfa")

    def load_payload(dev: common.Device, path: str) -> None:
        start_payload(dev, path)
        try:
            dev.emmc_switch(0)
            answered = dev.emmc_read(0)[510:512] == b"\x55\xaa"
        except RuntimeError:
            answered = False
        if not answered:
            log("The eMMC did not answer")
            raise SystemExit(3)

    common.Device.emmc_read = emmc_read
    common.Device.emmc_write_blocks = emmc_write_blocks
    common.Device.find_device = find_device
    common.Device.handshake = handshake
    common.Device.rpmb_read = rpmb_read
    amonet.flash_data = flash_data
    amonet.load_payload = load_payload
    amonet.main()
    return 0


def child_emos(action: str, want: str) -> int:
    import serial  # ruff: ignore[import-outside-top-level]
    from serial.tools import list_ports  # ruff: ignore[import-outside-top-level]

    ports = [
        port.device
        for port in list_ports.comports()
        if (port.vid, port.pid) == EMOS_USB_ID and want in {"", port.serial_number}
    ]
    if action == "find" or len(ports) != 1:
        print(len(ports))
        return int(action != "find")
    try:
        dev = serial.Serial(ports[0], 115200, timeout=0.2, write_timeout=1)
    except serial.SerialException:
        print("denied")
        return 1
    with dev:
        dev.reset_input_buffer()
        dev.write(b"\n")
        seen = b""
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            seen += dev.read(256)
            if seen.rstrip(b" ").endswith(b"password:"):
                print("password")
                return 1
            if seen.rstrip(b" ").endswith(b"#"):
                dev.write(b"/init recovery\n")
                print("recovery")
                return 0
    print("silent")
    return 1


def child_main(name: str, *args: str) -> int:
    if sys.path[0] == str(pathlib.Path(__file__).resolve().parent):
        del sys.path[0]
    children = {"bootrom": child_bootrom, "emos": child_emos, "reset": child_reset}
    return children[name](*args)


def child_reset() -> int:
    import serial  # ruff: ignore[import-outside-top-level]
    from serial.tools import list_ports  # ruff: ignore[import-outside-top-level]

    for port in list_ports.comports():
        if port.vid == MEDIATEK_VID:
            try:
                with serial.Serial(
                    port.device, 115200, timeout=1, write_timeout=1
                ) as dev:
                    dev.write(struct.pack(">II", 0xF00DD00D, 0x3000))
            except serial.SerialException:
                pass
    return 0


def clear_boot0() -> None:
    answer = adb_shell(command=Shell.CLEAR_BOOT0.value).split("\n")[-1].split()
    if answer == ["4096", "0"]:
        return
    read, *still_set = answer or [""]
    if read == "4096" and still_set:
        ERASED.unlink(missing_ok=True)
        restore_failed(
            "boot0's header did not clear, so a failure from here would brick"
            " rather than fall into the bootrom; nothing else was written"
        )
    restore_failed(
        "boot0 did not read back, so whether its header cleared is unknown;"
        " nothing else was written"
    )


def clock() -> str:
    return time.strftime("%H:%M:%S")


def color(stream: TextIO) -> bool:
    if os.environ.get("NO_COLOR") or os.environ.get("TERM") == "dumb":
        return False
    if os.name == "nt" and "WT_SESSION" not in os.environ:
        return False
    return stream.isatty()


def command(  # ruff: ignore[too-many-arguments]
    *,
    args: list[str | pathlib.PurePath],
    cwd: pathlib.Path | None = None,
    stderr: int | None = None,
    stdin: int | BinaryIO | None = None,
    stdout: int | None = None,
    timeout: float | None = None,
) -> subprocess.CompletedProcess[bytes]:
    loud = ARGS.verbose and not SESSION.probing
    if loud:
        show(text=f"{clock()} $ {' '.join(map(str, args))}")
    result = subprocess.run(
        args,
        check=False,
        cwd=cwd,
        stderr=stderr,
        stdin=stdin,
        stdout=stdout,
        timeout=timeout,
    )
    if loud:
        show(text=f"{clock()}   exit {result.returncode}")
    return result


def countdown() -> None:
    text = "The bootrom answered. The short may come off now; continuing{}."
    if not sys.stdout.isatty():
        warn(text.format(f" in {SHORT_WAIT} s"))
        time.sleep(SHORT_WAIT)
        return
    for left in range(SHORT_WAIT, 0, -1):
        status(text.format(f" in {left} s"))
        time.sleep(1)
    status(text.format(""))
    print()


def cpio(files: dict[bytes, tuple[int, bytes]]) -> bytes:
    out = bytearray()
    for inode, name in enumerate([*sorted(files), b"TRAILER!!!"], start=300000):
        mode, body = files.get(name, (0, b""))
        fields = (inode, mode, 0, 0, 1, 0, len(body), 0, 0, 0, 0, len(name) + 1, 0)
        out += b"070701" + b"".join(b"%08x" % field for field in fields) + name + b"\0"
        out += bytes(-len(out) % 4) + body
        out += bytes(-len(out) % 4)
    return bytes(out)


def cpio_files(data: bytes) -> dict[bytes, tuple[int, bytes]]:
    files = {}
    at = 0
    while True:
        if data[at : at + 6] != b"070701":
            _die(message=f"{FIREOS.name}'s ramdisk is not a newc cpio archive")
        fields = [int(data[at + 6 + 8 * i : at + 14 + 8 * i], 16) for i in range(13)]
        name = data[at + 110 : at + 109 + fields[11]]
        at = (at + 110 + fields[11] + 3) & ~3
        body = data[at : at + fields[6]]
        at = (at + fields[6] + 3) & ~3
        if name == b"TRAILER!!!":
            return files
        files[name] = (fields[1], body)


def devices(args: list[str]) -> str:
    for _ in range(5):
        out = run(args=args, timeout=30).stdout
        lines = out.splitlines()
        if not any("no permissions" in line for line in lines) or any(
            line.split()[1:2] in (["device"], ["recovery"], ["fastboot"])
            for line in lines
        ):
            return out
        time.sleep(1)
    _die(message=NO_ACCESS + rerun())


def digest(*, kind: str, limit: int = 0, path: pathlib.Path) -> str:
    h = hashlib.new(kind, usedforsecurity=False)
    left = limit or path.stat().st_size
    with path.open("rb") as f:
        while left > 0:
            block = f.read(min(1 << 20, left))
            if not block:
                break
            left -= len(block)
            h.update(block)
    return h.hexdigest()


def dot_details(*, asked: Callable[..., str]) -> list[str]:
    said = [
        labelled(label=name, value=asked(f"getprop {name}")) for name in REPORT_PROPS
    ]
    for name in EMMC_FIELDS:
        where = f"/sys/block/{node_held(target=DISK)}/device/{name}"
        said.append(
            labelled(
                label=f"eMMC {name}",
                value=asked(f"cat {where} 2>/dev/null || echo '<absent>'"),
            )
        )
    window = asked("dmesg | head -1 | tr -d '[]' | tr -s ' ' | cut -d' ' -f2")
    held = asked("cut -d' ' -f1 /proc/uptime")
    said.append(labelled(label="uptime", value=f"{held} s, dmesg from {window} s"))
    return said


def downgrade() -> None:
    amonet = unpack(AMONET_V1)
    wheel = fetch(PYSERIAL)
    if getvar("unlock_status").lower() != "true":
        _die(message="not in amonet's fastboot")
    lk = getvar("lk_build_desc")
    if not LK_DESC.fullmatch(lk):
        _die(
            message="the Dot did not report its bootloader version;"
            " boot0 was not erased. " + again()
        )
    if lk == LK.V1.value:
        _die(message="the Dot already runs amonet v1.1.0's bootloader. " + again())
    PROGRESS.begin(estimate="45 s", label="writing amonet v1.1.0's bootloader")
    bootrom(
        amonet=amonet,
        erase=erase_by_fastboot,
        payload=v2_payload(),
        wheel=wheel,
    )
    v1_recovery()


def download(build: str) -> pathlib.Path:
    b = BUILDS[build]
    CACHE.mkdir(exist_ok=True, parents=True)
    ota = (
        CACHE
        / f"update-kindle-biscuit_puffin-{b.ns}_user_{build}_{b.number.zfill(13)}.bin"
    )
    if ota in SESSION.verified:
        return ota
    if not ota.is_file() or digest(kind="sha256", path=ota) != b.sha256:
        page = (
            f"{FTVDB}{b.md5}-{b.number}-fire-os-{b.ftvdb_version}-{b.ns.lower()}"
            f"-{build}-{b.date}/"
        )
        request = urllib.request.Request(page, headers={"User-Agent": "Mozilla/5.0"})
        try:  # ruff: ignore[too-many-statements-in-try-clause]
            with urllib.request.urlopen(request, timeout=60) as response:
                links = re.findall(
                    r'https://[^"<> ]+\.bin', response.read().decode(errors="replace")
                )
            if not links:
                _die(message="no download link on " + page)
            part = CACHE / (ota.name + ".part")
            response = urllib.request.urlopen(links[0], timeout=60)
            with response, part.open("wb") as out:
                done, total = save(
                    label=f"downloading OTA {build}", out=out, response=response
                )
        except (OSError, http.client.HTTPException) as e:
            _die(message=f"the download failed: {e!r}")
        if total and done != total:
            _die(
                message=f"the download stopped after {done} of {total} bytes. "
                + again()
            )
        part.replace(ota)
    if digest(kind="sha256", path=ota) != b.sha256:
        _die(message=f"{ota} does not hash to {b.sha256}")
    SESSION.verified.add(ota)
    if threading.current_thread() is threading.main_thread():
        passed(f"OTA {build} verified")
    return ota


def emmc_leg(
    *,
    asked: Callable[..., str],
    blocks: int,
    line: Callable[[str, object], None],
    node: str,
) -> None:
    chunk = SESSION.carried
    if not blocks:
        line("over the eMMC", f"skipped: this Dot has no {EMMC_TARGET} partition")
        return
    if not chunk:
        line("over the eMMC", "skipped: nothing verified in RAM to write from")
        return
    rounds = max(blocks - EMMC_SPARE, 0) // chunk
    if not rounds:
        line(
            "over the eMMC",
            f"skipped: {EMMC_TARGET} holds under {chunk + EMMC_SPARE} MiB",
        )
        return
    written = rounds * chunk
    line(
        "writing over",
        f"{EMMC_TARGET}, {node}, {written} MiB behind a fresh filesystem"
        f" whose own blocks end by {EMMC_SPARE} MiB",
    )
    started = time.monotonic()
    PROGRESS.begin(
        estimate=f"{written // 3} s", label=f"writing and reading {EMMC_TARGET}"
    )
    said = asked(
        Shell.EMMC.value.format(
            blocks=written,
            chunk=chunk,
            dd=SESSION.dd,
            node=node,
            notrunc=" conv=notrunc" if SESSION.dd == "toybox dd" else "",
            rounds=rounds,
            source=DOT_TMP / "usb-test",
            spare=EMMC_SPARE,
        ),
        timeout=EMMC_TIMEOUT,
    )
    PROGRESS.end()
    took = time.monotonic() - started
    answer = said.split("\n")[-1]
    read = answer.partition("card:")[2].split(" ")[0] if "card:" in answer else ""
    want = repeat_md5(blocks=chunk, times=rounds)
    asked(f"rm -f {DOT_TMP / 'usb-test'}")
    line("md5 read back", read or "<nothing>")
    line("md5 of what was written", want)
    line(
        "moved",
        f"{2 * written} MiB written and read in {took:.0f} s,"
        f" {2 * written / max(took, 1):.0f} MiB a second",
    )
    if not read:
        line("the test did not run", masked(text=said) or "<nothing>")
    else:
        line(
            "verdict",
            f"the eMMC took {written} MiB and gave it back unchanged, with nothing"
            " crossing USB, so a failed install is the cable or the host"
            if read == want
            else "THE eMMC DID NOT GIVE BACK WHAT IT TOOK. Nothing crossed USB,"
            " so the card or its driver is at fault, not the cable",
        )
    formatted = asked(Shell.EMMC_FORMAT.value.format(node=node))
    line(
        "fresh filesystem",
        "yes"
        if formatted.split("\n")[-1] == "formatted"
        else f"NO, so {EMMC_TARGET} holds no filesystem: {formatted}",
    )


def emos(action: str) -> str:
    if action != "find" and ARGS.verbose:
        show(text=f"{clock()} $ {shlex.join(child('emos', action))}")
    out = subprocess.run(
        child("emos", action, USER_SERIAL or ""),
        capture_output=True,
        check=False,
        env=dict(os.environ, PYTHONPATH=str(fetch(PYSERIAL))),
        text=True,
        timeout=30,
    ).stdout.strip()
    if out.isdigit() and int(out) > 1:
        _die(message=MORE_THAN_ONE)
    return out


def emos_recovery() -> None:
    answer = emos("recovery")
    if answer == "password":
        _die(
            message="emOS's console asks for a password. Type /init recovery at"
            " the console, or clear the console password in EchoMuse's"
            " dashboard, then run this again."
        )
    if answer == "denied":
        _die(message=NO_ACCESS + rerun())
    if answer != "recovery":
        _die(message="emOS's serial console did not answer. Run this again.")


def emos_stage() -> None:
    emos_recovery()
    PROGRESS.begin(estimate="40 s", label="waiting for recovery to start")


def erase_by_fastboot() -> str:
    for args in (["fastboot", "erase", "boot0"], ["fastboot", "reboot"]):
        result = run(args=args, timeout=60)
        if result.returncode != 0:
            return f"{' '.join(args)} failed:\n{result.stdout}"
    return ""


def extract(*, ota: pathlib.Path, work: pathlib.Path) -> None:  # ruff: ignore[complex-structure, too-many-branches, too-many-locals, too-many-statements]
    with zipfile.ZipFile(ota) as z:  # ruff: ignore[too-many-nested-blocks]
        if "payload.bin" not in z.namelist():
            extract_blocks(work=work, z=z)
            return
        with z.open("payload.bin") as f:
            h = f.read(24)
            if h[:4] != b"CrAU" or struct.unpack(">Q", h[4:12])[0] != PAYLOAD_VERSION:
                _die(message="the OTA's payload.bin is not a version 2 update payload")
            msize = struct.unpack(">Q", h[12:20])[0]
            sig = struct.unpack(">I", h[20:24])[0]
            manifest = f.read(msize)
        base = 24 + msize + sig
        block = 4096
        partitions = {}
        for fn, _, v in fields(manifest):
            if fn == ManifestField.BLOCK_SIZE:
                block = v
            if fn == ManifestField.PARTITIONS:
                d, ops = {}, []
                for a, _, c in fields(v):
                    if a == PartitionField.OPERATIONS:
                        ops.append(c)
                    else:
                        d[a] = c
                partitions[d[PartitionField.NAME].decode()] = (
                    {a: c for a, _, c in fields(d[PartitionField.NEW_INFO])},
                    ops,
                )
        with z.open("payload.bin") as payload:
            for name in IMAGES:
                if name not in partitions:
                    _die(message=f"the OTA has no {name} image")
                info, ops = partitions[name]
                path = work / (name + ".img")
                if (
                    path.is_file()
                    and digest(kind="sha256", path=path) == info[InfoField.HASH].hex()
                ):
                    passed(f"{name:>{max(map(len, IMAGES))}} matches the manifest")
                    continue
                img = bytearray(info[InfoField.SIZE])
                for op in ops:
                    o, extents = {}, []
                    for a, _, c in fields(op):
                        if a == OperationField.DST_EXTENTS:
                            extents.append({x: y for x, _, y in fields(c)})
                        else:
                            o[a] = c
                    payload.seek(base + o.get(OperationField.DATA_OFFSET, 0))
                    blob = payload.read(o.get(OperationField.DATA_LENGTH, 0))
                    kind = o[OperationField.TYPE]
                    if kind == OperationType.REPLACE:
                        raw = blob
                    elif kind == OperationType.REPLACE_BZ:
                        raw = bz2.decompress(blob)
                    elif kind == OperationType.REPLACE_XZ:
                        raw = lzma.decompress(blob)
                    else:
                        _die(message=f"{name} has op type {kind}")
                    pos = 0
                    for e in extents:
                        n = e[ExtentField.NUM_BLOCKS] * block
                        at = e.get(ExtentField.START_BLOCK, 0) * block
                        img[at : at + n] = raw[pos : pos + n]
                        pos += n
                path.write_bytes(memoryview(img)[: info[InfoField.SIZE]])
                if digest(kind="sha256", path=path) != info[InfoField.HASH].hex():
                    _die(message=name + " does not match the manifest")
                passed(f"{name:>{max(map(len, IMAGES))}} matches the manifest")


def extract_blocks(*, work: pathlib.Path, z: zipfile.ZipFile) -> None:
    for name, member in BLOCK_IMAGES.items():
        data = z.read(member)
        if name != "preloader":
            data += bytes(-len(data) % 4096)
        (work / (name + ".img")).write_bytes(data)
        passed(f"{name:>{max(map(len, IMAGES))}} taken from the OTA")
    commands, size = transfer_list(z.read("system.transfer.list").decode())
    with (work / "system.img").open("wb") as out, z.open("system.new.dat") as dat:
        out.truncate(size)
        for verb, ranges in commands:
            if verb != "new":
                continue
            for start, end in ranges:
                out.seek(start)
                for offset in range(start, end, 1 << 20):
                    n = min(1 << 20, end - offset)
                    chunk = dat.read(n)
                    if len(chunk) != n:
                        _die(message="the OTA's system.new.dat is short")
                    out.write(chunk)
        if dat.read(1):
            _die(message="the OTA's system.new.dat outlasts its transfer list")
    passed(f"{'system':>{max(map(len, IMAGES))}} built from the transfer list")


def fastbrick() -> None:
    amonet = unpack(AMONET_V2)
    if getvar("product") != "BISCUIT":
        _die(message="fastboot reports a product other than BISCUIT")
    lk = getvar("lk_build_desc")
    if not lk:
        _die(
            message="fastboot did not report the bootloader version;"
            " the Dot was not modified"
        )
    image = "bin/fastbrick.img"
    if lk in {LK.V2.value, LK.V2_MIGRATE.value}:
        image = "bin/fastbrick-20221007.img"
    PROGRESS.begin(estimate="10 s", label="unlocking with amonet v2.0.0")
    for attempt in range(10):
        if attempt:
            time.sleep(2)
        args = ["fastboot", "-S", "256M", "flash", "brick", image]
        try:
            out = run(args=args, cwd=amonet, timeout=8).stdout
            started = False
        except subprocess.TimeoutExpired as e:
            out = (e.output or b"").decode("utf-8", "replace")
            started = True
        if "eMMC-RO" in out:
            _die(message="the Dot's eMMC is read-only; it was not modified")
        if "Device mismatch" in out:
            _die(message="the payload rejected this device; it was not modified")
        if started:
            PROGRESS.begin(
                estimate="40 s", label="waiting for v2.0.0 recovery to start"
            )
            return
    _die(
        message=f"the unlock did not start after 10 attempts, with {image} for"
        f" the bootloader {lk}; the Dot was not modified"
    )


def fetch(download: Download) -> pathlib.Path:
    name, want = download.name, download.sha256
    with hold(LOCKS[download]):
        CACHE.mkdir(exist_ok=True, parents=True)
        path = CACHE / name
        if path in SESSION.verified or (
            path.is_file() and digest(kind="sha256", path=path) == want
        ):
            SESSION.verified.add(path)
            return path
        part = CACHE / (name + ".part")
        try:
            with urllib.request.urlopen(download.url, timeout=60) as response:  # ruff: ignore[multiple-with-statements]
                with part.open("wb") as out:
                    done, total = save(
                        label="downloading " + name, out=out, response=response
                    )
        except (OSError, http.client.HTTPException) as error:
            _die(message=f"downloading {name} failed: {error!r}")
        if total and done != total:
            _die(
                message=f"downloading {name} stopped after {done} of {total} bytes. "
                + again()
            )
        if digest(kind="sha256", path=part) != want:
            _die(message=f"{name} does not hash to {want}")
        part.replace(path)
        SESSION.verified.add(path)
        return path


def fields(b: bytes) -> Iterator[tuple[int, int, int | bytes]]:
    i = 0
    while i < len(b):
        k, i = varint(b=b, i=i)
        fn, wt = k >> 3, k & 7
        if wt == WireType.VARINT:
            v, i = varint(b=b, i=i)
        elif wt == WireType.LEN:
            n, i = varint(b=b, i=i)
            v = b[i : i + n]
            i += n
        elif wt == WireType.FIXED64:
            v = b[i : i + 8]
            i += 8
        elif wt == WireType.FIXED32:
            v = b[i : i + 4]
            i += 4
        else:
            _die(message=f"the OTA's manifest has wire type {wt}")
        yield fn, wt, v


def getvar(name: str) -> str:
    try:
        out = run(args=["fastboot", "getvar", name], timeout=30).stdout
    except subprocess.TimeoutExpired:
        return ""
    for line in out.splitlines():
        if line.startswith(name + ":"):
            return line[len(name) + 1 :].strip()
    return ""


def gpt_intact(*, entries: bytes, hdr: bytes) -> bool:
    if hdr[:8] != b"EFI PART" or struct.unpack("<I", hdr[12:16])[0] != GPT_HEADER_SIZE:
        return False
    if struct.unpack("<II", hdr[80:88]) != (128, 128):
        return False
    h = bytearray(hdr[:92])
    h[16:20] = bytes(4)
    return (
        zlib.crc32(h) & 0xFFFFFFFF == struct.unpack("<I", hdr[16:20])[0]
        and zlib.crc32(entries[: 128 * 128]) & 0xFFFFFFFF
        == struct.unpack("<I", hdr[88:92])[0]
    )


def gzip_member(*, path: pathlib.Path) -> bool:
    try:
        with path.open("rb") as f:
            return f.read(len(GZIP_MAGIC)) == GZIP_MAGIC
    except OSError:
        return False


def hide_updater() -> None:
    def hidden() -> bool:
        out = adb_shell(command=f"su -c 'dumpsys package {UPDATER}'", timeout=60)
        return any(
            line.strip().startswith("User 0:") and "hidden=true" in line
            for line in out.split("\n")
        )

    if not hidden():
        adb_shell(command=f"su -c 'pm hide {UPDATER}'", timeout=60)
    if not hidden():
        _die(
            message=f"{UPDATER} is not hidden; an update would replace the"
            " boot image and remove root"
        )


@contextlib.contextmanager
def hold(lock: threading.Lock) -> Iterator[None]:
    while not lock.acquire(timeout=0.5):
        pass
    try:
        yield
    finally:
        lock.release()


def in_fastboot() -> bool:
    out = devices(["fastboot", "devices"])
    serials = [
        line.split()[0]
        for line in out.splitlines()
        if line.split()[1:2] == ["fastboot"]
    ]
    if USER_SERIAL:
        return USER_SERIAL in serials
    if len(serials) > 1:
        _die(message=MORE_THAN_ONE)
    return bool(serials)


def install_amonet_v2() -> None:
    zip_path = preloader_last()
    PROGRESS.begin(estimate="40 s", label="installing amonet v2.0.0")
    remote = DOT_TMP / AMONET_V2.name
    push_checked(local=zip_path, remote=remote)
    ERASED.touch()
    answer = adb_shell(command=Shell.CLEAR_BOOT0.value, timeout=60).split()
    if answer[-2:] != ["4096", "0"]:
        if answer[-2:-1] == ["4096"]:
            ERASED.unlink(missing_ok=True)
        _die(
            message="boot0's header did not read back as cleared, so amonet"
            " v2.0.0's zip was not installed. " + again()
        )
    out = ""
    with contextlib.suppress(subprocess.TimeoutExpired):
        out = adb_shell(command=f"twrp install {remote}", timeout=120)
    if "- Done" in out:
        ERASED.unlink(missing_ok=True)
    errors = [line for line in out.split("\n") if "(!)" in line]
    if errors:
        _die(message="amonet v2.0.0's zip failed: " + errors[0])
    PROGRESS.begin(estimate="40 s", label="waiting for v2.0.0 recovery to start")


def install_fireos(*, reboot: bool = True, slot: str = "") -> None:
    fireos = fetch(FIREOS)
    magisk = fetch(MAGISK)
    slot = slot or adb_shell(command="getprop ro.boot.slot_suffix", timeout=30)
    if slot not in {"_a", "_b"}:
        _die(message=f"TWRP reports the boot slot {slot!r}, not _a or _b")
    boot, system = f"{BY_NAME}/boot{slot}_x", f"{BY_NAME}/system{slot}"
    with tempfile.TemporaryDirectory(dir=CACHE) as tmp:
        work = pathlib.Path(tmp)
        PROGRESS.begin(estimate="5 s", label="formatting userdata")
        if not adb_script(body=Shell.DATA.value, name="data.sh", work=work):
            _die(message="userdata did not format and mount")
        PROGRESS.begin(estimate="100 s", label="writing Fire OS 5.5.5.4's /system")
        write_system(system)
        body = Shell.SYSTEM.value.format(hosts=" ".join(UPDATE_HOSTS), system=system)
        if not adb_script(body=body, name="system.sh", work=work):
            _die(message="patching /system failed")

        PROGRESS.begin(estimate="5 s", label="writing the boot image")
        data = boot_image(fireos=fireos, magisk=magisk)
        image = work / "boot.img"
        image.write_bytes(data.ljust(-(-len(data) // 4096) * 4096, b"\0"))
        final = DOT_TMP / "boot.img"
        push_checked(local=image, remote=final)
        adb_shell(
            command=Shell.WRITE.value.format(dst=boot, src=final),
            timeout=120,
        )
        blocks = image.stat().st_size // 4096
        read_back = (
            f"[ -b {boot} ] && dd if={boot} bs=4096 count={blocks} 2>/dev/null | md5sum"
        )
        want = digest(kind="md5", path=image)
        written = (
            adb_shell(command=read_back, timeout=120).split("\n")[-1].split(" ")[0]
        )
        if written != want:
            _die(
                message="the patched boot image did not verify on the Dot: read"
                f" {written or 'nothing'}, expected {want}"
            )

        PROGRESS.begin(estimate="5 s", label="installing Magisk 17.3")
        install_magisk(magisk=magisk, work=work)
    if reboot:
        PROGRESS.begin(estimate="4 min", label="waiting for rooted Fire OS 5 to boot")
        run(args=["adb", "reboot"])


def install_fireos6() -> None:
    amonet_chain()
    ota = download(V2_BUILD)
    zip_path = boot_root()
    if "boot_a_x" in partition_table():
        _die(
            message="v2.0.0's TWRP runs on amonet v1.1.0's partition table. " + again()
        )
    PROGRESS.begin(estimate="10 s", label="wiping cache and data")
    for part in ("cache", "data"):
        adb_shell(command="twrp wipe " + part, timeout=120)
    update = "/sdcard/update.zip"
    for index, slot in enumerate(("first", "second")):
        PROGRESS.begin(
            estimate="2 min",
            label=f"installing Fire OS 6 {V2_BUILD}, {slot} slot",
        )
        if index:
            run(args=["adb", "reboot", "recovery"], check=True, timeout=60)
            wait_for_twrp()
        push_checked(local=ota, remote=update)
        before = adb_shell(command="bcbtool get_active", timeout=30)
        adb_shell(command="twrp install " + update, timeout=600)
        after = adb_shell(command="bcbtool get_active", timeout=30)
        if {before, after} != {"a", "b"}:
            _die(
                message=f"installing Fire OS 6 {V2_BUILD} left the active slot"
                f" {after!r}, where it was {before!r}. " + again()
            )
    PROGRESS.begin(estimate="10 s", label="installing boot-root")
    push_checked(local=zip_path, remote="/sdcard/boot-root.zip")
    out = adb_shell(command="twrp install /sdcard/boot-root.zip", timeout=300)
    errors = [line for line in out.split("\n") if "(!) Error" in line]
    if errors:
        _die(message="boot-root.zip failed: " + errors[0])
    PROGRESS.begin(estimate="1 min", label="waiting for rooted Fire OS 6 to boot")
    run(args=["adb", "reboot"])


def install_magisk(*, magisk: pathlib.Path, work: pathlib.Path) -> None:
    db = work / "magisk.db"
    magisk_db(db)
    files = magisk_files(db=db.read_bytes(), magisk=magisk)
    archive = work / "magisk.cpio"
    archive.write_bytes(cpio(files))
    push_checked(local=archive, remote=DOT_TMP / "magisk.cpio")
    if not adb_script(body=Shell.MAGISK.value, name="magisk.sh", work=work):
        _die(message="Magisk 17.3 did not install")
    names = sorted(name for name, (mode, _) in files.items() if stat.S_ISREG(mode))
    want = hashlib.md5(
        b"".join(files[name][1] for name in names), usedforsecurity=False
    ).hexdigest()
    paths = " ".join(name.decode() for name in names)
    got = adb_shell(command=f"cd /; cat {paths} | md5sum").split("\n")[-1].split(" ")[0]
    if got != want:
        _die(message="Magisk 17.3's files did not verify on the Dot")


def into_recovery(*, line: Callable[[str, object], None]) -> bool:
    asked = False
    deadline = time.monotonic() + WAIT
    while time.monotonic() < deadline:
        if state() in TWRPS:
            PROGRESS.end()
            return True
        if in_fastboot():
            unlocked = getvar("unlock_status")
            for name, value in (
                ("product", getvar("product")),
                ("unlock_status", unlocked),
                ("lk_build_desc", getvar("lk_build_desc")),
            ):
                line("fastboot " + name, value or "<nothing>")
            if not unlocked:
                say(
                    text="This Dot's fastboot did not say whether it is unlocked,"
                    " so this stops rather than guess. Check the cable, and that no"
                    " other program holds the Dot, then run it again."
                )
                return False
            if unlocked.lower() != "true":
                say(
                    text="This Dot's bootloader is locked, so it holds no recovery"
                    " to collect from and nothing here can give it one. Root it"
                    " first: run this script again without --report, and follow"
                    " what it asks."
                )
                return False
            PROGRESS.begin(estimate="40 s", label="restarting the Dot in its recovery")
            run(args=["fastboot", "oem", "reboot-recovery"], timeout=60)
        elif run(args=["adb", "get-state"], timeout=30).stdout.strip() == "device":
            PROGRESS.begin(estimate="40 s", label="restarting the Dot in its recovery")
            run(args=["adb", "reboot", "recovery"], timeout=60)
        else:
            if not asked:
                asked = True
                say(
                    text="The rest of this report comes from the Dot's recovery."
                    " Start the Dot in fastboot mode and this goes on by itself. "
                    + FASTBOOT_MODE
                    + " A Dot already unlocked with amonet v2.0.0 has no fastboot:"
                    " hold the + button instead while you plug it back in, which"
                    " starts its TWRP. A Dot that shows nothing on USB is in its"
                    " bootrom, and only a root run brings it back. Ctrl-C stops"
                    " this."
                )
            status(text="Waiting for the Dot in fastboot mode, with a green ring.")
            time.sleep(2)
            continue
        try:
            run(args=["adb", "wait-for-recovery"], timeout=180)
        except subprocess.TimeoutExpired:
            PROGRESS.halt()
            return False
        time.sleep(5)
        PROGRESS.end()
        return state() in TWRPS
    PROGRESS.halt()
    return False


def labelled(*, label: str, value: object) -> str:
    return f"{label:<24} {value}"


def magisk_binary(magiskinit: bytes) -> bytes:
    at = magiskinit.find(b"\xfd7zXZ\0")
    while at != -1:
        with contextlib.suppress(lzma.LZMAError):
            binary = lzma.LZMADecompressor().decompress(magiskinit[at:])
            if binary.startswith(b"\x7fELF"):
                return binary
        at = magiskinit.find(b"\xfd7zXZ\0", at + 1)
    _die(message=f"{MAGISK.name}'s magiskinit holds no magisk binary")


def magisk_db(path: pathlib.Path) -> None:
    db = sqlite3.connect(path)
    db.execute(
        "CREATE TABLE policies (uid INT, package_name TEXT, policy INT, "
        "until INT, logging INT, notification INT)"
    )
    db.execute("INSERT INTO policies VALUES (2000, 'com.android.shell', 2, 0, 1, 0)")
    db.commit()
    db.close()


def magisk_files(*, db: bytes, magisk: pathlib.Path) -> dict[bytes, tuple[int, bytes]]:
    files = {
        b"data/adb": (0o40700, b""),
        b"data/adb/magisk": (0o40755, b""),
        b"data/adb/magisk/chromeos": (0o40755, b""),
        b"data/adb/magisk.db": (0o100600, db),
    }
    with zipfile.ZipFile(magisk) as z:
        for info in z.infolist():
            folder, _, name = info.filename.partition("/")
            if folder in {"arm", "common"}:
                path = name
            elif folder == "chromeos":
                path = info.filename
            else:
                continue
            files[f"data/adb/magisk/{path}".encode()] = (0o100755, z.read(info))
        script = z.read("META-INF/com/google/android/update-binary").decode()
    packed = re.search(r"^BB_ARM=(\S+)", script, re.MULTILINE)
    if not packed:
        _die(message=f"{MAGISK.name} has no busybox in its installer")
    files[b"data/adb/magisk/busybox"] = (
        0o100755,
        lzma.decompress(base64.b64decode(packed.group(1))),
    )
    files[b"data/adb/magisk/magisk"] = (
        0o100755,
        magisk_binary(files[b"data/adb/magisk/magiskinit"][1]),
    )
    return files


def magisk_kernel(kernel: bytes) -> bytes:
    stream = zlib.decompressobj(31)
    image = stream.decompress(kernel[512:])
    image = image.replace(b"skip_initramfs\0", b"want_initramfs\0")
    body = gzip.compress(image, compresslevel=9, mtime=0) + stream.unused_data
    return kernel[:4] + struct.pack("<I", len(body)) + kernel[8:512] + body


def main() -> None:
    parser = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter
    )
    parser.add_argument(
        "--verbose",
        action="store_true",
        help="print each adb and fastboot command, its exit status and the"
        " time, and each state the Dot reaches",
    )
    parser.add_argument(
        "--report",
        action="store_true",
        help="print what the Dot and this computer are, and the Dot's partition"
        " table, for an issue report; it starts the Dot's recovery to read them"
        " and writes no partition",
    )
    parser.add_argument(
        "--write-test",
        action="store_true",
        help=f"write a known pattern over the Dot's {EMMC_TARGET} partition and"
        " read it back, which tells a failing eMMC from a failing USB cable"
        " because nothing crosses USB; it leaves a fresh empty filesystem there",
    )
    parser.add_argument(
        "--short",
        action="store_true",
        help="for a Dot that shows no light and needs its test point shorted:"
        " wait for its bootrom, say when the short may come off, then go on",
    )
    parser.add_argument(
        "target",
        choices=tuple(GOALS),
        default="v1-bboe",
        help="v1-bboe (the default): rooted Fire OS 5.5.5.4 on amonet v1.1.0, with"
        f" TWRP {TWRP_VERSION}. v1: the same, with v1.1.0's own TWRP 3.2.3. v2:"
        f" amonet v2.0.0's own procedure, Fire OS 6 {V2_BUILD} with boot-root.zip's"
        " root adb, which overdub cannot run on. stock: Amazon's Fire OS 6 BUILD,"
        " which erases the whole Dot. Run again with another target to move the"
        " Dot to it",
        nargs="?",
    )
    parser.add_argument(
        "build",
        choices=sorted(BUILDS),
        help="with stock, and only with stock: the build to install, one of "
        + ", ".join(sorted(BUILDS)),
        metavar="BUILD",
        nargs="?",
    )
    options = parser.parse_intermixed_args()
    if (options.target == "stock") != (options.build is not None):
        parser.error(
            "stock takes a BUILD, and no other target does: "
            + ", ".join(sorted(BUILDS))
        )
    ARGS.build = options.build or ""
    ARGS.target = options.target
    ARGS.verbose = options.verbose
    for tool in ("adb", "fastboot"):
        if not shutil.which(tool):
            _die(message=tool + " not found: install Android platform-tools")
    check_user()
    check_adb()
    move_old_caches()
    SESSION.short = options.short
    if options.report:
        report()
        return
    if options.write_test:
        write_test()
        return
    root()


def mark() -> str:
    try:
        "\u2705".encode(sys.stdout.encoding or "ascii")
    except (LookupError, UnicodeEncodeError):
        return "done"
    return "\u2705"


def masked(*, text: str) -> str:
    text = re.sub(flags=re.IGNORECASE, pattern=GUID, repl="<guid>", string=text)
    serial = os.environ.get("ANDROID_SERIAL", "")
    return text.replace(serial, "<serial>") if serial else text


def md5_mismatch(*, command: str, want: str) -> str:
    got = [*adb_shell(command=command).split("\n")[-1].split(" "), ""][0]
    return "" if got == want else f": read {got or 'nothing'}, expected {want}"


def mmc_said(*, asked: Callable[..., str]) -> list[str]:
    logged = asked(MMC_LOG)
    if MMC_LOG_REACHES_BOOT in logged:
        return [logged]
    if not any(word in logged.lower() for word in ("mmc", "msdc")):
        unread = "\nThat is not the kernel's log, so it says nothing either way."
        return [logged, unread]
    gone = (
        "\nThat log no longer reaches the boot, so the card's own lines are gone."
        " They come back from a restart into this recovery, which is safe only"
        " when a run is not part-way through writing. Restart it yourself and"
        " run this again if they are wanted."
    )
    return [logged, gone]


def move_old_caches() -> None:
    for old in ("overdub-root", "overdub-stock"):
        source = CACHE.parent / old
        if not source.is_dir():
            continue
        CACHE.mkdir(exist_ok=True, parents=True)
        for path in source.iterdir():
            if not (CACHE / path.name).exists():
                with contextlib.suppress(OSError):
                    shutil.move(str(path), str(CACHE / path.name))
        with contextlib.suppress(OSError):
            source.rmdir()


def no_port_help() -> str:
    if sys.platform.startswith("linux"):
        return (
            "No new /dev/ttyACM* port could be opened. Stop ModemManager if it runs,\n"
            "and check that /etc/udev/rules.d/51-echo-dot.rules has this line:\n\n"
            'SUBSYSTEM=="tty", ATTRS{idVendor}=="0e8d", ATTRS{idProduct}=="0003",'
            ' MODE="0660", GROUP="plugdev", TAG+="uaccess"'
        )
    if sys.platform == "darwin":
        return "No new /dev/cu.usbmodem* port appeared."
    return "No new COM port appeared. Windows may need a driver for USB ID 0e8d:0003."


def node_held(*, target: str) -> str:
    return target.rsplit("/", maxsplit=1)[-1]


def node_names(*, listing: str) -> dict[str, str]:
    found = {}
    for row in listing.split("\n"):
        name, arrow, target = row.partition(" -> ")
        if arrow:
            found[name.split()[-1]] = target.strip()
    for name, target in tuple(found.items()):
        if not node_order(node=target):
            found[name] = found.get(node_held(target=target), target)
    return found


def node_order(*, node: str) -> int:
    disk, _, number = node_held(target=node).partition("p")
    return int(number) if disk.startswith("mmcblk") and number.isdigit() else 0


def node_sizes(*, partitions: str) -> dict[str, int]:
    return {
        field[-1]: int(field[2]) * 1024
        for field in (row.split() for row in partitions.split("\n"))
        if len(field) == PARTITION_FIELDS and field[2].isdigit()
    }


def on_usb(line: str) -> bool:
    if os.name != "nt":
        return " usb:" in line
    parts = line.split()
    return (
        parts[1:2] in (["device"], ["recovery"], ["unauthorized"])
        and ":" not in parts[0]
        and not parts[0].startswith("emulator-")
    )


def paint(*, code: ANSIColor, text: str) -> str:
    return f"\033[{code.value}m{text}\033[0m" if color(sys.stdout) else text


def partition_field(*, name: str, number: int) -> str:
    command = f"sgdisk --info={number} {DISK}; echo field-ok"
    for attempt in range(PUSH_TRIES):
        out = adb_shell(command=command, timeout=30)
        if out.split("\n")[-1] == "field-ok":
            for line in out.split("\n"):
                if line.startswith(name + ":"):
                    return line.split(":", 1)[1].strip().strip("'").split()[0]
            _die(message=f"sgdisk --info={number} printed no {name}:\n{out}")
        if attempt + 1 < PUSH_TRIES:
            reconnect(DISK)
    _die(message=f"sgdisk --info={number} did not answer in full. " + again())
    return ""


def partition_row(*, row: str) -> int:
    field = row.split()
    if not field or not field[0].isdigit():
        return 0
    return int(field[0]) if len(field) >= TABLE_FIELDS else 0


def partition_rows(*, names: dict[str, str], printed: str) -> list[str]:
    said = [row.rstrip() for row in printed.split("\n")]
    if said[-1:] != [TABLE_OK] or not any(partition_row(row=row) for row in said):
        return [*said, "", "That is not a whole table, so no name was matched to it."]
    said = said[:-1]
    held: dict[str, list[str]] = {}
    for name, target in sorted(names.items()):
        key = node_held(target=target) if node_order(node=target) else target
        held.setdefault(key, []).append(name)
    header = next((row for row in said if row.lstrip().startswith("Number ")), None)
    width = max(len(row) for row in said if partition_row(row=row) or row == header)
    rows = []
    for row in said:
        number = partition_row(row=row)
        if not number:
            rows.append(
                f"{row:<{width}}  {'node':<10}  alias" if row == header else row
            )
            continue
        node = f"{node_held(target=DISK)}p{number}"
        linked = held.pop(node, [])
        called = [name for name in linked if name != row.split()[-1]]
        alias = ", ".join(called) if called else "" if linked else "<no name>"
        rows.append(f"{row:<{width}}  {node:<10}  {alias}".rstrip())
    return rows + unreal_rows(held=held)


def partition_table() -> str:
    command = f"sgdisk --print {DISK}; echo {TABLE_OK}"
    for attempt in range(PUSH_TRIES):
        said = adb_shell(command=command, timeout=30).split("\n")
        if said[-1] == TABLE_OK and any(" userdata" in line for line in said):
            return "\n".join(said[:-1])
        if attempt + 1 < PUSH_TRIES:
            reconnect(DISK)
    _die(message="sgdisk did not print the Dot's partition table:\n" + "\n".join(said))
    return ""


def partitions() -> dict[str, tuple[int, int, int]]:
    found = {}
    for line in partition_table().split("\n"):
        field = line.split()
        if len(field) >= TABLE_FIELDS and all(f.isdigit() for f in field[:3]):
            found[field[-1]] = (int(field[0]), int(field[1]), int(field[2]))
    if "userdata" not in found:
        _die(message="no userdata in the Dot's partition table")
    return found


def passed(message: str) -> None:
    show(text=f"{mark()} {message}")


def pattern_chunks(*, blocks: int) -> Iterator[bytes]:
    buffer = EMMC_PATTERN * (MEBI // len(EMMC_PATTERN) + 1)
    left = blocks * MEBI
    while left:
        chunk = buffer[: min(len(buffer), left)]
        yield chunk
        left -= len(chunk)


def pattern_file(*, blocks: int, path: pathlib.Path) -> str:
    digest = hashlib.md5(usedforsecurity=False)
    with path.open("wb") as out:
        for chunk in pattern_chunks(blocks=blocks):
            out.write(chunk)
            digest.update(chunk)
    return digest.hexdigest()


def portions(
    *, checksum: hashlib._Hash, chunks: Iterator[bytes]
) -> Iterator[list[bytes]]:
    held: list[bytes] = []
    size = 0
    for chunk in chunks:
        checksum.update(chunk)
        view = memoryview(chunk)
        while view:
            took = min(SYSTEM_SLICE * MEBI - size, len(view))
            held.append(bytes(view[:took]))
            view, size = view[took:], size + took
            if size == SYSTEM_SLICE * MEBI:
                yield held
                held, size = [], 0
    if held:
        yield held


def prebuild() -> None:
    with contextlib.suppress(Exception, SystemExit):
        system_image()


def predownload() -> None:
    with contextlib.suppress(Exception, SystemExit):
        prefetch()


DOWNLOADER = threading.Thread(daemon=True, target=predownload)


def prefetch() -> None:
    if DOWNLOADER.is_alive() and threading.current_thread() is threading.main_thread():
        show(text="Finishing the downloads.")
    unpack(AMONET_V2)
    if ARGS.target in {"stock", "v2"}:
        build = ARGS.build or V2_BUILD
        with hold(LOCKS[build]):
            download(build)
        return
    unpack(AMONET_V1)
    fetch(PYSERIAL)
    fetch(FIREOS)
    fetch(MAGISK)
    fetch(TWRP)
    threading.Thread(daemon=True, target=prebuild).start()


def preloader_last() -> pathlib.Path:
    patched = CACHE / ("preloader-last-" + AMONET_V2.name)
    source = fetch(AMONET_V2)
    script = "META-INF/com/google/android/update-binary"
    anchor = "set_progress 1.00\n"
    with zipfile.ZipFile(source) as src:
        text = src.read(script).decode()
        start = text.find('ui_print "- Updating preloader"')
        end = text.find('ui_print "- Updating lk"')
        if not 0 < start < end or text.count(anchor) != 1:
            _die(message=f"{AMONET_V2.name}'s installer is not the one expected")
        rest = text[:start] + text[end:]
        moved = rest.replace(anchor, anchor + "\n" + text[start:end])
        part = patched.with_suffix(".part")
        with zipfile.ZipFile(part, "w") as dst:
            for info in src.infolist():
                data = moved.encode() if info.filename == script else src.read(info)
                dst.writestr(info, data)
    part.replace(patched)
    return patched


def probe() -> State:  # ruff: ignore[complex-structure, too-many-return-statements, too-many-branches]
    if in_fastboot():
        unlock = getvar("unlock_status").lower()
        if unlock == "false":
            if getvar("lk_build_desc") == LK.V1.value:
                return State.LOCKED_V1_FASTBOOT
            return State.STOCK_FASTBOOT
        if unlock != "true":
            return State.STARTING
        lk = getvar("lk_build_desc")
        if not lk:
            return State.STARTING
        if lk == LK.V1.value:
            return State.V1_FASTBOOT
        return State.V2_FASTBOOT
    adb_state = ""
    if usb_serial():
        adb_state = run(args=["adb", "get-state"], timeout=30).stdout
    if "unauthorized" in adb_state:
        return State.STOCK_BOOTED
    adb_state = adb_state.strip()
    if adb_state == "recovery":
        version = adb_shell(command="getprop ro.twrp.version", timeout=30)
        lk = adb_shell(command="getprop ro.boot.lk_build_desc", timeout=30)
        if not version[:1].isdigit() or not LK_DESC.fullmatch(lk):
            return State.STARTING
        if "mtp" not in adb_shell(command="getprop sys.usb.config", timeout=30):
            return State.STARTING
        if lk != LK.V1.value:
            if "boot_a_x" in partition_table():
                return State.AMONET_V2_TWRP_V1_TABLE
            return State.AMONET_V2_TWRP
        if version != TWRP_VERSION:
            return State.AMONET_V1_TWRP
        return State.BBOE_V1_TWRP
    if adb_state == "device":
        booted = adb_shell(command="getprop sys.boot_completed", timeout=30) == "1"
        if adb_shell(command="getprop ro.build.version.name", timeout=30).startswith(
            "Fire OS 6"
        ):
            if "uid=0" in adb_shell(command="id; su -c id", timeout=30):
                return State.V2_BOOTED
            return State.STOCK_BOOTED
        if booted and "uid=0" in adb_shell(command="su -c id", timeout=30):
            return rooted()
        return State.BOOTED
    return State.EMOS if emos("find") == "1" else State.NONE


def push_checked(*, local: pathlib.Path, remote: str | pathlib.PurePosixPath) -> None:
    for attempt in range(PUSH_TRIES):
        if attempt:
            reconnect(remote)
        try:  # ruff: ignore[too-many-statements-in-try-clause]
            result = run(args=["adb", "push", local, remote], timeout=600)
            if result.returncode != 0:
                said = result.stdout
                continue
            if adb_shell(command=f"md5sum {remote}", timeout=300).split(" ")[
                0
            ] == digest(kind="md5", path=local):
                return
            said = "its md5 read back did not match"
        except subprocess.TimeoutExpired as error:
            said = (
                f"{' '.join(map(str, error.cmd))} did not finish in"
                f" {error.timeout:.0f} seconds"
            )
    _die(
        message=f"{remote} did not arrive intact after {PUSH_TRIES} tries;"
        f" the last: {said}. " + asked_for()
    )


def read_recovery(size: int) -> bytes:
    return command(
        args=[
            "adb",
            "exec-out",
            f"su -c 'dd if={BY_NAME}/recovery bs=512 count={size // 512} 2>/dev/null'",
        ],
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        timeout=60,
    ).stdout


def read_sectors(*, count: int, start: int) -> bytes:
    raw = command(
        args=[
            "adb",
            "exec-out",
            f"{SESSION.dd} if={DISK} bs=512 skip={start} count={count} 2>/dev/null",
        ],
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        timeout=60,
    ).stdout
    if len(raw) != count * 512:
        _die(
            message=f"read {len(raw)} bytes of the Dot's partition table,"
            f" not {count * 512}"
        )
    return raw


def reboot_recovery(label: str) -> None:
    run(args=["adb", "reboot", "recovery"], check=True, timeout=60)
    PROGRESS.begin(estimate="40 s", label=label)


def reconnect(remote: str | pathlib.PurePosixPath) -> None:
    PROGRESS.note(
        f"{remote} did not arrive; waiting for the Dot to reconnect to try again."
    )
    try:
        run(args=["adb", "wait-for-recovery"], timeout=120)
    except subprocess.TimeoutExpired:
        _die(message="the Dot did not reconnect over USB in recovery")
    time.sleep(5)


def remaining(*, start: State, table: dict[State, Stage]) -> int | None:
    total = 0
    for _ in range(len(table) + 1):
        if start == GOALS[ARGS.target]:
            return total
        stage = table.get(start)
        if stage is None or stage.then is None:
            return None
        total += stage.steps
        start = stage.then
    return None


def repeat_md5(*, blocks: int, times: int) -> str:
    digest = hashlib.md5(usedforsecurity=False)
    for _ in range(times):
        for chunk in pattern_chunks(blocks=blocks):
            digest.update(chunk)
    return digest.hexdigest()


def replace_twrp() -> None:
    twrp = fetch(TWRP)
    PROGRESS.begin(estimate="40 s", label=f"waiting for TWRP {TWRP_VERSION}")
    remote = DOT_TMP / TWRP.name
    push_checked(local=twrp, remote=remote)
    recovery = f"{BY_NAME}/recovery"
    adb_shell(
        command=Shell.WRITE.value.format(dst=recovery, src=remote),
        timeout=120,
    )
    sectors = twrp.stat().st_size // 512
    read_back = (
        f"[ -b {recovery} ] && dd if={recovery} bs=512 count={sectors} 2>/dev/null"
        " | md5sum"
    )
    written = adb_shell(command=read_back, timeout=120).split("\n")[-1].split(" ")[0]
    if written != digest(kind="md5", path=twrp):
        _die(
            message=f"TWRP {TWRP_VERSION} did not verify in recovery. Leave the Dot"
            " running. " + again()
        )
    run(args=["adb", "reboot", "recovery"], check=True, timeout=60)


def report() -> None:
    def line(label: str, value: object) -> None:
        show(text=labelled(label=label, value=value))

    def asked(command: str, timeout: float = 60) -> str:
        try:
            said = adb_shell(command=command, timeout=timeout)
        except subprocess.TimeoutExpired:
            return "<timed out>"
        return masked(text=said) or "<nothing>"

    show(text="Paste everything below into the issue.\n")
    line("script", pathlib.Path(__file__).name)
    line("host", f"{sys.platform} ({os.name}), {platform.platform()}")
    line("python", sys.version.split()[0])
    said = run(args=["adb", "version"], timeout=30).stdout.split("\n")
    line("adb", ", ".join(part.strip() for part in said[:2] if part.strip()))
    line("state", state().value)
    if not into_recovery(line=line):
        show(text="\nThe report stops with what is above.")
        return
    line("state in recovery", state().value)
    for text in dot_details(asked=asked):
        show(text=text)
    held = CACHE / f"system-{FIREOS.sha256[:12]}" / "md5"
    recorded = held.read_text().split() if held.is_file() else []
    needs = 0
    if len(recorded) >= SYSTEM_FIELDS and recorded[1].isdigit():
        needs = int(recorded[1]) * 4096
        line("system image", f"{needs} bytes, md5 {recorded[0]}")
    else:
        line("system image", "not built yet, so its size is unknown")
    sizes = node_sizes(partitions=asked("cat /proc/partitions"))
    names = node_names(listing=asked(f"ls -l {BY_NAME}/"))
    holds = sizes.get(node_held(target=names.get("system_a", "")), 0)
    line("system_a", f"{names.get('system_a') or '<unknown>'}, {holds} bytes")
    if holds and needs:
        line("system_a spare", f"{holds - needs} bytes")
    show(text="\npartition table, with each name's node and any other name for it")
    printed = asked(f"sgdisk --print {DISK}; echo {TABLE_OK}", timeout=120)
    for row in partition_rows(names=names, printed=printed):
        show(text=row)
    show(text="\nwhat the kernel says about the eMMC")
    for text in mmc_said(asked=asked):
        show(text=text)


def rerun() -> str:
    return "sg plugdev -c " + shlex.quote(shlex.join([sys.executable, *sys.argv]))


def restore(
    *,
    backup_sector: int,
    build: str,
    files: dict[str, pathlib.Path],
    parts: dict[str, Partition],
    work: pathlib.Path,
) -> None:
    unmount()

    PROGRESS.begin(estimate="5 s", label="clear the preloader header (boot0)")
    ERASED.touch()
    clear_boot0()
    PROGRESS.end()

    for step in WRITES:
        write(
            estimate=step.estimate,
            label=step.label,
            number=parts[step.partition].number,
            path=files[step.image],
            sector=parts[step.partition].first,
        )
    write(
        estimate="5 s",
        label="write the stock table (backup)",
        path=files["gpt-backup.bin"],
        sector=backup_sector,
    )
    write(
        estimate="5 s",
        label="write the stock table (primary)",
        path=files["gpt-primary.bin"],
        sector=0,
    )

    PROGRESS.begin(estimate="30 s", label="format cache and userdata")
    if "No problems found" not in adb_shell(command="sgdisk --verify " + DISK):
        restore_failed("sgdisk does not accept the new table; do not reboot")
    unmount(started=True)
    adb_shell(command="blockdev --rereadpt " + DISK)
    if adb_shell(command='grep -c "mmcblk0p1[78]$" /proc/partitions') != "0":
        restore_failed(
            "the kernel still sees amonet's partitions; do not reboot,"
            " reread the table first"
        )
    userdata, cache = parts["userdata"], parts["cache"]
    kib = userdata.size // 1024
    if not adb_shell(
        command=f'grep " {kib} mmcblk0p{userdata.number}$" /proc/partitions'
    ):
        restore_failed("userdata is not its stock size; do not reboot")
    out = adb_shell(
        command=f"mke2fs -q -t ext4 {DISK}p{cache.number}"
        f" && mke2fs -q -t ext4 {DISK}p{userdata.number} && echo formatted"
    )
    if out.split("\n")[-1] != "formatted":
        restore_failed("cache and userdata did not format; do not reboot")
    PROGRESS.end()

    for step in chain_writes():
        write(
            estimate=step.estimate,
            label=step.label,
            number=parts[step.partition].number,
            path=files[step.image],
            sector=parts[step.partition].first,
        )

    PROGRESS.begin(estimate="5 s", label="write preloader to boot0")
    preloader = work / "preloader.img"
    staged = DOT_TMP / "pl.img"
    if run(args=["adb", "push", preloader, staged], timeout=120).returncode != 0:
        restore_failed("the preloader did not reach the Dot; do not reboot")
    adb_shell(command=Shell.BOOT0.value.format(src=staged))
    wrong = md5_mismatch(
        command="md5sum /dev/block/mmcblk0boot0",
        want=digest(kind="md5", path=preloader),
    )
    if wrong:
        restore_failed(
            f"boot0 does not match the {build} preloader{wrong}; do not reboot"
        )
    ERASED.unlink(missing_ok=True)
    PROGRESS.end()

    PROGRESS.begin(estimate="90 s", label=f"waiting for stock {build} to start")
    with contextlib.suppress(subprocess.TimeoutExpired):
        run(args=["adb", "shell", "-n", "reboot"], timeout=60)


def restore_failed(message: str) -> NoReturn:
    _die(message=message)


def restore_stage() -> None:  # ruff: ignore[complex-structure, too-many-branches, too-many-locals, too-many-statements]
    build = ARGS.build
    work = CACHE / ("stock-" + build)
    work.mkdir(exist_ok=True, parents=True)
    extract(ota=download(build), work=work)
    deadline = time.monotonic() + 30
    version = ""
    while not version or "mtp" not in adb_shell(command="getprop sys.usb.config"):
        if time.monotonic() > deadline:
            _die(message="TWRP did not finish starting within 30 seconds")
        time.sleep(1)
        version = adb_shell(command="getprop ro.twrp.version")
        if not version[:1].isdigit():
            version = ""
        elif not version.startswith(TWRP_VERSIONS):
            _die(
                message="this needs a TWRP for this Dot: v1.1.0's 3.2.3,"
                f" v2.0.0's 3.7.0, or the {TWRP_VERSION} that dot_firmware.py"
                " installs"
            )
    if (
        adb_shell(command="toybox dd --help >/dev/null 2>&1 && echo yes").split("\n")[
            -1
        ]
        == "yes"
    ):
        SESSION.dd = "toybox dd"
    tools = adb_shell(command=Shell.TOOLS.value).split("\n")[-1]
    if not tools.startswith("tools:"):
        _die(message="the Dot did not answer which tools it has: " + tools)
    if tools != "tools:":
        _die(message="this TWRP has no" + tools[len("tools:") :])
    device = adb_shell(command="getprop ro.product.device")
    if device != "biscuit":
        _die(
            message="this is not an Echo Dot (2nd Gen): TWRP reports the"
            f" device {device!r}"
        )

    raw = read_sectors(count=34, start=0)
    if not gpt_intact(entries=raw[1024:], hdr=raw[512:1024]):
        size = adb_shell(command=f"blockdev --getsize64 {DISK}")
        if not size.isdigit():
            _die(message="could not read the Dot's disk size")
        tail = read_sectors(count=33, start=int(size) // 512 - 33)
        raw = raw[:512] + tail[-512:] + tail[:-512]
        show(text="the primary partition table is damaged; using the backup")
    saved = work / f"current-gpt-{os.environ['ANDROID_SERIAL']}.bin"
    if not saved.exists():
        saved.write_bytes(raw)
    primary, backup, backup_sector, parts = stock_gpt(raw)
    files = {}
    for name, data in (("gpt-primary.bin", primary), ("gpt-backup.bin", backup)):
        files[name] = work / name
        files[name].write_bytes(data)
    boot = work / "boot.img"
    boot_size = parts["boot_a"].size
    files["boot"] = work / "boot16.img"
    files["boot"].write_bytes(boot.read_bytes().ljust(boot_size, b"\0"))
    files["expdb"] = work / "expdb.zero"
    files["expdb"].write_bytes(b"\0" * parts["expdb"].size)
    misc = bytearray(parts["misc"].size)
    misc[BCB_OFFSET : BCB_OFFSET + len(BCB)] = BCB
    files["misc"] = work / "misc.img"
    files["misc"].write_bytes(misc)
    for image in ("system", "tee", "lk"):
        files[image] = work / (image + ".img")
    for step in (*WRITES, *chain_writes()):
        if files[step.image].stat().st_size > parts[step.partition].size:
            _die(message=f"{files[step.image].name} does not fit {step.partition}")
    if (
        adb_shell(command="[ -b /dev/block/mmcblk0boot0 ] && echo block").split("\n")[
            -1
        ]
        != "block"
    ):
        _die(
            message="/dev/block/mmcblk0boot0 is not a block device, so the"
            " preloader would be written to a file and read back from it"
        )
    boot0 = adb_shell(command="cat /sys/block/mmcblk0boot0/size")
    if (
        not boot0.isdigit()
        or (work / "preloader.img").stat().st_size != int(boot0) * 512
    ):
        _die(message=f"preloader.img is not the size of boot0 ({boot0} sectors)")
    passed("stock partition table built from this Dot's own")
    warn(
        "About to overwrite this Dot's bootloaders, system and data with"
        f" stock {build}."
    )
    warn("Root is gone afterwards; dot_firmware.py puts it back.")
    try:
        for left in range(10, 0, -1):
            show(
                end="",
                flush=True,
                kind=Kind.WARN,
                text="\r"
                + paint(
                    code=ANSIColor.YELLOW,
                    text=f"Starting in {left:2d} s. Ctrl-C cancels.",
                ),
            )
            time.sleep(1)
    except KeyboardInterrupt:
        print()
        _die(message="stopped; nothing was written", prefix="")
    show(
        kind=Kind.WARN,
        text="\r"
        + paint(code=ANSIColor.YELLOW, text="Starting now.                     "),
    )

    try:
        restore(
            backup_sector=backup_sector,
            build=build,
            files=files,
            parts=parts,
            work=work,
        )
    except subprocess.TimeoutExpired as error:
        restore_failed(
            f"{' '.join(map(str, error.cmd))} did not finish in {error.timeout:.0f}"
            " seconds. Do not reboot. " + again()
        )
    except KeyboardInterrupt:
        if PROGRESS.step == PROGRESS.steps:
            _die(message=f"stopped; stock {build} is in place", prefix="")
        restore_failed("stopped part way. Do not reboot. " + again())


def root() -> None:  # ruff: ignore[complex-structure, too-many-branches, too-many-locals, too-many-statements]
    SESSION.writing = True
    usage = run(args=["fastboot", "--help"], timeout=30).stdout
    if not any(line.split()[:1] == ["-S"] for line in usage.splitlines()):
        _die(
            message="this fastboot has no -S option: install a newer"
            " Android platform-tools"
        )
    done: set[State] = set()
    table = stages()
    guided = False
    resumed = False
    seen = None
    shown = None
    deadline = None
    while True:
        current = state()
        installed = ARGS.target == "v2" and State.AMONET_V2_TWRP in done
        restored = ARGS.target == "stock" and bool(done & TWRPS)
        gesture = ARGS.target in {"stock", "v2"} and current == State.V2_FASTBOOT
        if (
            DOWNLOADER.ident is None
            and current not in {State.BOOTED, State.STARTING, *ROOTED}
            and not (
                ARGS.target == "stock"
                and current in {State.STOCK_BOOTED, State.STOCK_FASTBOOT}
            )
        ):
            DOWNLOADER.start()
        if current not in {State.NONE, State.STARTING, *TWRPS}:
            ERASED.unlink(missing_ok=True)
        if current != State.NONE:
            SESSION.short = False
        if current == State.NONE and (ERASED.exists() or SESSION.short) and not resumed:
            if ARGS.target == "v2":
                boot_root()
            prefetch()
            resumed = True
            done.difference_update(TWRPS)
            done.add(State.V1_FASTBOOT)
            if not SESSION.short:
                say(
                    code=ANSIColor.YELLOW,
                    text="The last run stopped after it erased boot0, so the Dot"
                    " cannot start. This run writes amonet v1.1.0 through the"
                    " bootrom, then goes on from v1.1.0's TWRP. If nothing"
                    " happens within a minute, unplug the Dot and plug it back"
                    " in.",
                )
            left = remaining(start=State.BBOE_V1_TWRP, table=table)
            if left is not None:
                PROGRESS.steps = PROGRESS.step + (2 if SESSION.short else 3) + left
            if bootrom(
                amonet=unpack(AMONET_V1),
                erase=None,
                payload=v2_payload(),
                wheel=fetch(PYSERIAL),
            ):
                v1_recovery()
            else:
                done.discard(State.V1_FASTBOOT)
                resumed = False
            seen = None
            continue
        if current != seen:
            seen = current
            deadline = time.monotonic() + WAIT
            if ARGS.verbose and current != shown:
                shown = current
                show(text=f"{clock()} state: {current.value}")
            if (
                current not in done
                and current not in {State.NONE, State.STARTING, State.BOOTED}
                and not (current == State.STOCK_BOOTED and installed)
            ):
                PROGRESS.end()
            if current == State.BOOTED and not PROGRESS.open and ARGS.target != "stock":
                show(text="The Dot is starting Fire OS. Waiting for it to finish.")
            elif current == State.STOCK_BOOTED and installed:
                deadline = time.monotonic() + MINUTE
            elif current == State.STOCK_BOOTED and ARGS.target != "stock":
                guided = True
                say(
                    text="This Dot appears to be unmodified. To unlock and root it,"
                    " start it in fastboot mode. "
                    + FASTBOOT_MODE
                    + " A Dot already unlocked with amonet v2.0.0 has no fastboot:"
                    " hold the + button instead while you plug it back in, which"
                    " starts its TWRP."
                )
            elif current == State.EMOS and current not in done:
                say(
                    text="This Dot runs EchoMuse's emOS. Rebooting it into recovery"
                    " through its serial console."
                )
            elif gesture:
                say(
                    text="This Dot is in amonet v2.0.0's fastboot. Unplug it, and"
                    " hold the + button while you plug it back in: that starts"
                    " v2.0.0's TWRP."
                )
            elif (
                current == State.V2_BOOTED
                and current not in done
                and ARGS.target != "v2"
            ):
                say(
                    text="This Dot runs rooted Fire OS 6 on amonet v2.0.0."
                    " Rebooting it into recovery."
                )
            elif current == State.NONE and not done and not PROGRESS.open and guided:
                show(text="Waiting for the Dot in fastboot mode, with a green ring.")
            elif current == State.NONE and not done and not PROGRESS.open:
                guided = True
                say(
                    text="Waiting for a Dot on USB. Connect it with a USB cable."
                    " A Dot that runs Amazon's own software needs fastboot mode. "
                    + FASTBOOT_MODE
                    + " Ctrl-C stops the script."
                )
        if ARGS.target == "v2" and current == State.V2_BOOTED:
            version = adb_shell(command="getprop ro.build.version.name")
            warn(f"The Dot is rooted: {version}, with root adb.")
            warn(
                "overdub cannot run on Fire OS 6. dot_firmware.py with no target"
                " roots it on Fire OS 5.5.5.4, which overdub runs on."
            )
            cache_note()
            return
        if ARGS.target == "stock" and current in {
            State.STOCK_BOOTED,
            State.STOCK_FASTBOOT,
        }:
            if done:
                passed(f"The Dot runs stock {ARGS.build}.")
                warn(
                    "If you will root it again, do not set it up in the Alexa app"
                    " first: on Wi-Fi it can take an update to a build"
                    " dot_firmware.py has not met."
                )
            else:
                say(
                    text="This Dot appears to run stock Fire OS 6, so there is"
                    " nothing to restore. However, if you unlocked it with amonet"
                    " v2.0.0, hold its + button while you plug it in. That starts"
                    " its TWRP. Then run this again."
                )
            cache_note()
            return
        if current == GOALS[ARGS.target]:
            hide_updater()
            version = adb_shell(command="getprop ro.build.version.name")
            selinux = adb_shell(command="getenforce")
            warn(f"The Dot is rooted: {version}, SELinux {selinux}, {UPDATER} hidden.")
            warn("Install overdub with deploy/install.py <name>.")
            cache_note()
            return
        if (
            gesture
            or current in done
            or current in {State.NONE, State.STOCK_BOOTED, State.STARTING}
            or (current == State.BOOTED and ARGS.target != "stock")
        ):
            waits = {State.NONE} if installed else {State.NONE, State.STOCK_BOOTED}
            if restored:
                waits = set()
            if not gesture and current not in waits and time.monotonic() > deadline:
                if restored:
                    _die(
                        message=f"stock {ARGS.build} has not started"
                        f" {WAIT // 60} minutes after the restore"
                    )
                if current == State.STOCK_BOOTED:
                    _die(
                        message="Fire OS 6 started without root adb, so"
                        " boot-root.zip did not take. " + again()
                    )
                if current == State.BOOTED:
                    _die(
                        message="Fire OS has not finished booting with root."
                        " Reboot to recovery. " + again()
                    )
                _die(
                    message=f"the Dot has been {current.value} for"
                    f" {WAIT // 60} minutes. " + again()
                )
            time.sleep(2)
            continue
        if not done:
            if ARGS.target == "v2":
                boot_root()
            if ARGS.target in {"stock", "v2"} or current not in ROOTED:
                prefetch()
            warn("Keep the Dot plugged in until dot_firmware.py finishes.")
        done.add(current)
        stage = table.get(current)
        if stage:
            left = remaining(start=current, table=table)
            PROGRESS.steps = 0 if left is None else PROGRESS.step + left
            stage.run()
            done.update(stage.passes)
        seen = None


def rooted() -> State:
    images = {
        State.ROOTED_BBOE: fetch(TWRP),
        State.ROOTED_V1: unpack(AMONET_V1) / "bin" / "twrp.img",
    }
    data = read_recovery(max(image.stat().st_size for image in images.values()))
    for found, image in images.items():
        size = image.stat().st_size
        if hashlib.md5(data[:size], usedforsecurity=False).hexdigest() == digest(
            kind="md5", path=image
        ):
            return found
    return State.ROOTED


def run(
    *,
    args: list[str | pathlib.PurePath],
    check: bool = False,
    cwd: pathlib.Path | None = None,
    stdin: BinaryIO | int = subprocess.DEVNULL,
    timeout: float | None = None,
) -> subprocess.CompletedProcess[str]:
    result = command(
        args=args,
        cwd=cwd,
        stderr=subprocess.STDOUT,
        stdin=stdin,
        stdout=subprocess.PIPE,
        timeout=timeout,
    )
    result.stdout = result.stdout.decode("utf-8", "replace").replace("\r", "")
    if check and result.returncode != 0:
        _die(message=f"{' '.join(map(str, args))} failed:\n{result.stdout}")
    return result


def save(
    *, label: str, out: BinaryIO, response: http.client.HTTPResponse
) -> tuple[int, int]:
    total = int(response.headers.get("Content-Length") or 0)
    div, unit = (1e3, "KB") if 0 < total < MEGA else (MEGA, "MB")

    def meter(done: int) -> str:
        if total:
            return (
                f"{done / div:.1f} of {total / div:.1f} {unit} ({100 * done // total}%)"
            )
        return f"{done / div:.1f} {unit}"

    room = 78 - (len(meter(total)) if total else 12)
    if len(label) > room:
        label = label[: room - 3] + "..."
    done = 0
    loud = threading.current_thread() is threading.main_thread() and sys.stdout.isatty()
    if loud:
        show(end="", flush=True, text=f"{label:<{room}} {meter(0):>{78 - room}}")
    for block in iter(lambda: response.read(1 << 20), b""):
        out.write(block)
        done += len(block)
        if loud:
            show(
                end="", flush=True, text=f"\r{label:<{room}} {meter(done):>{78 - room}}"
            )
    if loud:
        print()
    return done, total


def say(*, code: ANSIColor | None = None, text: str) -> None:
    text = textwrap.fill(text, 79)
    show(
        kind=Kind.WARN if code else Kind.INFO,
        text=paint(code=code, text=text) if code else text,
    )


def show(*, kind: Kind = Kind.INFO, text: str, **options: str | bool) -> None:
    if SESSION.shown not in {None, kind}:
        print()
    SESSION.shown = kind
    print(text, **options)


def since(start: float) -> str:
    seconds = int(time.monotonic() - start)
    if seconds < MINUTE:
        return f"{seconds}s"
    return f"{seconds // 60}m {seconds % 60:02d}s"


def slice_path(*, index: int, part: pathlib.Path) -> pathlib.Path:
    return part / f"system.{index:02d}.gz"


def sliced(
    *, checksum: hashlib._Hash, chunks: Iterator[bytes], part: pathlib.Path
) -> int:
    index = 0
    for index, portion in enumerate(portions(checksum=checksum, chunks=chunks)):
        path = slice_path(index=index, part=part)
        with gzip.open(path, "wb", compresslevel=6) as out:
            for chunk in portion:
                out.write(chunk)
    return index + 1


def sliced_system(*, slices: list[pathlib.Path], system: str) -> str:
    held = DOT_TMP / "system-slice.gz"
    for index, piece in enumerate(slices):
        push_checked(local=piece, remote=held)
        result = run(
            args=[
                "adb",
                "shell",
                "-n",
                Shell.SYSTEM_SLICE.value.format(
                    dd=SESSION.dd,
                    held=held,
                    notrunc=" conv=notrunc" if SESSION.dd == "toybox dd" else "",
                    seek=index * SYSTEM_SLICE,
                    system=system,
                ),
            ],
            timeout=900,
        )
        if GUNZIP_FAILED in result.stdout:
            return f"{piece.name} did not unpack on the Dot"
        if result.returncode:
            return result.stdout.strip() or f"it exited with {result.returncode}"
        if SLICE_OK not in result.stdout:
            return result.stdout.strip() or f"the Dot said nothing about {piece.name}"
    return ""


def stages() -> dict[State, Stage]:
    downgrades = frozenset({
        State.AMONET_V2_TWRP,
        State.AMONET_V2_TWRP_V1_TABLE,
        State.V1_FASTBOOT,
        State.V2_FASTBOOT,
    })
    table = {
        State.EMOS: Stage(emos_stage, 1, None),
        State.LOCKED_V1_FASTBOOT: Stage(fastbrick, 2, State.AMONET_V2_TWRP_V1_TABLE),
        State.STOCK_FASTBOOT: Stage(fastbrick, 2, State.AMONET_V2_TWRP),
        State.V1_FASTBOOT: Stage(v1_recovery, 1, State.BBOE_V1_TWRP),
    }
    to_twrp = "waiting for recovery"
    if ARGS.target == "stock":
        return (
            table
            | {
                twrp: Stage(restore_stage, STOCK_STEPS, State.STOCK_BOOTED, TWRPS)
                for twrp in TWRPS
            }
            | {
                State.BOOTED: Stage(lambda: reboot_recovery(to_twrp), 1, None),
                State.ROOTED: Stage(
                    lambda: reboot_recovery(to_twrp), 1, State.AMONET_V1_TWRP
                ),
                State.ROOTED_BBOE: Stage(
                    lambda: reboot_recovery(to_twrp), 1, State.BBOE_V1_TWRP
                ),
                State.ROOTED_V1: Stage(
                    lambda: reboot_recovery(to_twrp), 1, State.AMONET_V1_TWRP
                ),
                State.V2_BOOTED: Stage(
                    lambda: reboot_recovery("waiting for v2.0.0 recovery to start"),
                    1,
                    State.AMONET_V2_TWRP,
                ),
            }
        )
    if ARGS.target == "v2":
        return table | {
            State.AMONET_V1_TWRP: Stage(install_amonet_v2, 2, State.AMONET_V2_TWRP),
            State.AMONET_V2_TWRP: Stage(install_fireos6, 5, State.V2_BOOTED),
            State.AMONET_V2_TWRP_V1_TABLE: Stage(
                install_amonet_v2, 2, State.AMONET_V2_TWRP
            ),
            State.BBOE_V1_TWRP: Stage(install_amonet_v2, 2, State.AMONET_V2_TWRP),
            State.ROOTED: Stage(
                lambda: reboot_recovery(to_twrp), 1, State.AMONET_V1_TWRP
            ),
            State.ROOTED_BBOE: Stage(
                lambda: reboot_recovery(to_twrp), 1, State.BBOE_V1_TWRP
            ),
            State.ROOTED_V1: Stage(
                lambda: reboot_recovery(to_twrp), 1, State.AMONET_V1_TWRP
            ),
        }
    goal = GOALS[ARGS.target]
    return (
        table
        | {
            State.AMONET_V1_TWRP: Stage(replace_twrp, 1, State.BBOE_V1_TWRP),
            State.AMONET_V2_TWRP: Stage(v1_append, 2, State.AMONET_V2_TWRP_V1_TABLE),
            State.AMONET_V2_TWRP_V1_TABLE: Stage(v1_chain, 9, goal),
            State.BBOE_V1_TWRP: Stage(install_fireos, 5, State.ROOTED_BBOE),
            State.V2_BOOTED: Stage(
                lambda: reboot_recovery("waiting for v2.0.0 recovery to start"),
                1,
                State.AMONET_V2_TWRP,
            ),
            State.V2_FASTBOOT: Stage(downgrade, 2, State.BBOE_V1_TWRP, downgrades),
        }
        | {rooted: Stage(swap_twrp, 1, goal) for rooted in ROOTED - {goal}}
    )


def state() -> State:
    SESSION.probing = True
    try:
        current = probe()
    except subprocess.TimeoutExpired:
        current = State.STARTING
    finally:
        SESSION.probing = False
    return current


def status(text: str) -> None:
    if sys.stdout.isatty():
        show(
            end="",
            flush=True,
            kind=Kind.WARN,
            text="\r" + paint(code=ANSIColor.YELLOW, text=text.ljust(79)),
        )
    else:
        warn(text)


def stdin_carries() -> bool:
    if SESSION.carries is None:
        probe = CACHE / "stdin-probe.bin"
        probe.write_bytes(b"\x1a" * 16 + os.urandom(1008))
        remote = DOT_TMP / "stdin-probe"
        try:
            with probe.open("rb") as f:
                sent = run(args=["adb", "shell", f"cat > {remote}"], stdin=f)
            read = adb_shell(command=f"md5sum {remote}; rm -f {remote}", timeout=60)
            cut = read.split("\n")[-1].split(" ")[0] != digest(kind="md5", path=probe)
            SESSION.carries = not cut and not sent.returncode
        finally:
            probe.unlink(missing_ok=True)
        if sent.returncode:
            say(
                text="This computer's adb did not carry a 1 KiB probe either way,"
                " so the image goes over in pieces, which needs no stream."
            )
    return SESSION.carries


def stock_gpt(  # ruff: ignore[too-many-locals]
    raw: bytes,
) -> tuple[bytes, bytes, int, dict[str, Partition]]:
    mbr, hdr, entries = (
        raw[:512],
        bytearray(raw[512:1024]),
        raw[1024 : 1024 + 128 * 128],
    )
    if not gpt_intact(entries=entries, hdr=hdr):
        _die(message="neither copy of the Dot's partition table is intact")
    last_usable = struct.unpack("<Q", hdr[48:56])[0]
    backup_lba = max(struct.unpack("<QQ", hdr[24:40]))
    names = [
        entries[i * 128 + 56 : (i + 1) * 128].decode("utf-16le").rstrip("\0")
        for i in range(128)
    ]
    amonet = any(name.endswith("_x") for name in names)
    new = bytearray(128 * 128)
    k = 0
    for i in range(128):
        e = bytearray(entries[i * 128 : (i + 1) * 128])
        if e[:16] == b"\0" * 16:
            continue
        name = names[i]
        if amonet and name in {"boot_a", "boot_b"}:
            continue
        if name.endswith("_x"):
            name = name[:-2]
            e[56:128] = name.encode("utf-16le").ljust(72, b"\0")
        if name == "userdata":
            e[40:48] = struct.pack("<Q", last_usable)
        new[k * 128 : (k + 1) * 128] = e
        k += 1
    if k != STOCK_PARTITIONS:
        _die(
            message=f"the stock table would have {k} partitions, not {STOCK_PARTITIONS}"
        )
    entries_crc = zlib.crc32(new) & 0xFFFFFFFF

    def header(*, alternate: int, at: int, my: int) -> bytes:
        h = bytearray(hdr[:92])
        h[16:20] = b"\0" * 4
        h[24:32] = struct.pack("<Q", my)
        h[32:40] = struct.pack("<Q", alternate)
        h[72:80] = struct.pack("<Q", at)
        h[88:92] = struct.pack("<I", entries_crc)
        h[16:20] = struct.pack("<I", zlib.crc32(h) & 0xFFFFFFFF)
        return bytes(h).ljust(512, b"\0")

    parts = {}
    for i in range(k):
        e = new[i * 128 : (i + 1) * 128]
        first, last = struct.unpack("<QQ", e[32:48])
        parts[e[56:128].decode("utf-16le").rstrip("\0")] = Partition(
            first=first, number=i + 1, sectors=last - first + 1
        )
    primary = mbr + header(alternate=backup_lba, at=2, my=1) + bytes(new)
    backup = bytes(new) + header(alternate=1, at=backup_lba - 32, my=backup_lba)
    return primary, backup, backup_lba - 32, parts


def streamed_system(*, slices: list[pathlib.Path], system: str) -> str:
    stream = Shell.SYSTEM_STREAM.value.format(
        dd=SESSION.dd,
        notrunc=" conv=notrunc" if SESSION.dd == "toybox dd" else "",
        system=system,
    )
    with subprocess.Popen(
        ["adb", "shell", stream],
        stderr=subprocess.STDOUT,
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
    ) as adb:
        try:
            for piece in slices:
                adb.stdin.write(piece.read_bytes())
        except OSError:
            pass
        try:
            said = (adb.communicate(timeout=SYSTEM_TIMEOUT)[0] or b"").decode(
                errors="replace"
            )
        except subprocess.TimeoutExpired:
            adb.kill()
            return f"adb shell did not finish in {SYSTEM_TIMEOUT} seconds"
    if GUNZIP_FAILED in said:
        return "gunzip could not read the stream"
    if adb.returncode:
        return said.strip() or f"it exited with {adb.returncode}"
    if STREAM_OK not in said:
        return said.strip() or "the Dot said nothing about the write"
    return ""


def swap_twrp() -> None:
    image, label = unpack(AMONET_V1) / "bin" / "twrp.img", "TWRP 3.2.3"
    if ARGS.target == "v1-bboe":
        image, label = fetch(TWRP), f"TWRP {TWRP_VERSION}"
    PROGRESS.begin(estimate="10 s", label=f"writing {label} to recovery")
    remote = "/data/local/tmp/recovery.img"
    run(args=["adb", "push", image, remote], check=True, timeout=120)
    write = Shell.WRITE.value.format(dst=f"{BY_NAME}/recovery", src=remote)
    adb_shell(command="su -c " + shlex.quote(f"{write}; rm -f {remote}"), timeout=120)
    size = image.stat().st_size
    if hashlib.md5(read_recovery(size), usedforsecurity=False).hexdigest() != digest(
        kind="md5", path=image
    ):
        _die(
            message=f"{label} did not verify in recovery. Leave the Dot running. "
            + again()
        )


def system_chunks(*, dat: IO[bytes], ranges: list[tuple[int, int]]) -> Iterator[bytes]:
    at = 0
    for start, end in ranges:
        for offset in range(at * 4096, start * 4096, 1 << 20):
            yield bytes(min(1 << 20, start * 4096 - offset))
        for offset in range(start * 4096, end * 4096, 1 << 20):
            n = min(1 << 20, end * 4096 - offset)
            chunk = dat.read(n)
            if len(chunk) != n:
                _die(message=f"{FIREOS.name}'s system.new.dat is short")
            yield chunk
        at = end


def system_image() -> tuple[list[pathlib.Path], str, int]:
    target = CACHE / f"system-{FIREOS.sha256[:12]}"
    with hold(SESSION.system_lock):
        if not system_sliced(target=target):
            shutil.rmtree(target, ignore_errors=True)
            build_system(target)
    want, blocks, slices = (target / "md5").read_text().split()
    return (
        [slice_path(index=index, part=target) for index in range(int(slices))],
        want,
        int(blocks),
    )


def system_sliced(*, target: pathlib.Path) -> bool:
    try:
        _, _, slices = (target / "md5").read_text().split()
    except (OSError, ValueError):
        return False
    return all(
        gzip_member(path=slice_path(index=index, part=target))
        for index in range(int(slices))
    )


def transfer_command(words: list[str]) -> tuple[str, list[tuple[int, int]]]:
    numbers = words[1].split(",") if len(words) == TRANSFER_FIELDS else []
    if words[0] not in {"erase", "new", "zero"} or not all(
        n.isdigit() for n in numbers
    ):
        _die(message=f"the OTA's transfer list has a {words[0]!r} command")
    bounds = [int(n) * 4096 for n in numbers[1:]]
    ranges = list(zip(bounds[::2], bounds[1::2]))
    if any(start >= end for start, end in ranges):
        _die(message="the OTA's transfer list has an empty or reversed range")
    return words[0], ranges


def transfer_list(text: str) -> tuple[list[tuple[str, list[tuple[int, int]]]], int]:
    lines = text.split("\n")
    if lines[0] not in {"3", "4"}:
        _die(message=f"the OTA's transfer list is version {lines[0]}")
    commands = [
        transfer_command(words)
        for words in (line.split() for line in lines[4:])
        if words
    ]
    size = 0
    for start, end in sorted(r for _, ranges in commands for r in ranges):
        if start > size:
            _die(message="the OTA's transfer list leaves a gap")
        size = max(size, end)
    if not size:
        _die(message="the OTA's transfer list writes nothing")
    written = 0
    for start, end in sorted(
        r for verb, ranges in commands if verb != "erase" for r in ranges
    ):
        if start < written:
            _die(message="the OTA's transfer list writes a block twice")
        written = end
    return commands, size


def unmount(*, started: bool = False) -> None:
    left = adb_shell(command=Shell.UNMOUNT.value).split("\n")[-1]
    if left.startswith("left:"):
        if not left[len("left:") :].strip():
            return
        message = "still mounted: " + left[len("left:") :].strip()
    else:
        message = "the Dot did not answer what is mounted: " + left
    if started:
        restore_failed(message + "; do not reboot")
    _die(message=message + "; nothing was written")


def unpack(download: Download) -> pathlib.Path:
    with hold(LOCKS[download.folder]):
        archive = fetch(download)
        target = CACHE / download.folder
        if not target.is_dir():
            part = CACHE / (download.folder + ".part")
            shutil.rmtree(part, ignore_errors=True)
            with zipfile.ZipFile(archive) as z:
                z.extractall(part)
            part.replace(target)
        return target / "amonet"


def unreal_rows(*, held: dict[str, list[str]]) -> list[str]:
    if not held:
        return []
    pairs = sorted((", ".join(called), target) for target, called in held.items())
    width = max(len(called) for called, _ in pairs)
    return ["", "names that point at no partition"] + [
        f"{called:<{width}}  {target}" for called, target in pairs
    ]


def usb_leg(*, asked: Callable[..., str], line: Callable[[str, object], None]) -> None:
    fields = asked(f"df -k {DOT_TMP}").split("\n")[-1].split()
    available = fields[3] if len(fields) > USB_TEST_COLUMN else ""
    spare = int(available) // 1024 if available.isdigit() else 0
    blocks = min(USB_TEST_BLOCKS, spare - USB_TEST_SPARE)
    if blocks < USB_TEST_LEAST:
        line("over USB", f"skipped: {DOT_TMP} has only {spare} MiB free")
        return
    remote = DOT_TMP / "usb-test"
    local = CACHE / "usb-test.bin"
    want = pattern_file(blocks=blocks, path=local)
    started = time.monotonic()
    try:
        pushed = run(args=["adb", "push", local, remote], timeout=EMMC_TIMEOUT)
        failed = pushed.stdout if pushed.returncode else ""
    except subprocess.TimeoutExpired:
        failed = f"it did not finish in {EMMC_TIMEOUT} seconds"
    took = time.monotonic() - started
    read = (
        ""
        if failed
        else asked(f"md5sum {remote}", timeout=300).split("\n")[-1].split(" ")[0]
    )
    local.unlink(missing_ok=True)
    carried = read == want and not failed
    SESSION.carried = blocks if carried else 0
    line("over USB", f"{blocks} MiB pushed into {DOT_TMP}, which is RAM")
    line("md5 read back", read or "<nothing>")
    line("md5 pushed", want)
    line("rate", f"{blocks / max(took, 1):.0f} MiB a second, over {took:.0f} s")
    if failed:
        line("adb push said", masked(text=failed).split("\n")[-1])
    line(
        "verdict",
        "USB carried it intact, so a failed install is the eMMC, not the cable"
        if carried
        else "USB DID NOT CARRY IT. No eMMC was written, so the cable, the port"
        " or the host is at fault",
    )


def usb_serial() -> str | None:
    if USER_SERIAL:
        if ":" in USER_SERIAL:
            _die(
                message="ANDROID_SERIAL names a network device;"
                " this needs the Dot on USB"
            )
        devices(["adb", "devices", "-l"])
        return USER_SERIAL
    out = devices(["adb", "devices", "-l"])
    usb = [
        line.split()[0]
        for line in out.splitlines()[1:]
        if on_usb(line) and "no permissions" not in line
    ]
    if len(usb) > 1:
        _die(message=MORE_THAN_ONE)
    if usb:
        os.environ["ANDROID_SERIAL"] = usb[0]
        return usb[0]
    os.environ.pop("ANDROID_SERIAL", None)
    return None


def v1_append() -> None:
    amonet_chain()
    part = partitions()
    if "boot_a_x" in part:
        reboot_recovery("waiting for v2.0.0 recovery to start")
        return
    number, start, end = part["userdata"]
    if number != max(held[0] for held in part.values()):
        _die(message="userdata is not the last partition. " + again())
    shrunk = ((end // V1_ALIGN) * V1_ALIGN) - V1_APPEND - 1
    first, second = shrunk + 1, shrunk + 1 + V1_BOOT_BLOCKS
    if shrunk <= start:
        _die(message="userdata cannot give up room for amonet v1.1.0's boot images")
    code = partition_field(name="Partition GUID code", number=number)
    guid = partition_field(name="Partition unique GUID", number=number)
    a, b = number + 1, number + 2
    PROGRESS.begin(estimate="5 s", label="making room for amonet v1.1.0")
    adb_shell(
        command=f"sgdisk --set-alignment=1 --delete={number}"
        f" --new={number}:{start}:{shrunk} --typecode={number}:{code}"
        f" --partition-guid={number}:{guid} --change-name={number}:userdata"
        f" --new={a}:{first}:{first + V1_BOOT_BLOCKS - 1} --typecode={a}:{code}"
        f" --new={b}:{second}:{second + V1_BOOT_BLOCKS - 1} --typecode={b}:{code}"
        f" --change-name={part['boot_a'][0]}:boot_a_x"
        f" --change-name={part['boot_b'][0]}:boot_b_x"
        f" --change-name={a}:boot_a --change-name={b}:boot_b {DISK}",
        timeout=60,
    )
    left = partitions()
    for name, want in (
        ("userdata", (number, start, shrunk)),
        ("boot_a", (a, first, first + V1_BOOT_BLOCKS - 1)),
        ("boot_b", (b, second, second + V1_BOOT_BLOCKS - 1)),
    ):
        if left.get(name) != want:
            _die(
                message=f"sgdisk left {name} as {left.get(name)}, not {want}. "
                + again()
            )
    for name in ("boot_a_x", "boot_b_x"):
        if name not in left:
            _die(message=f"sgdisk did not leave a {name}. " + again())
    for field, holds in (
        ("Partition GUID code", code),
        ("Partition unique GUID", guid),
    ):
        if partition_field(name=field, number=number) != holds:
            _die(message=f"sgdisk left userdata a different {field}. " + again())
    reboot_recovery("waiting for v2.0.0 recovery to start")


def v1_chain() -> None:
    amonet_chain()
    amonet = unpack(AMONET_V1)
    twrp = amonet / "bin" / "twrp.img"
    if ARGS.target == "v1-bboe":
        twrp = fetch(TWRP)
    node, part = chain_nodes()
    with tempfile.TemporaryDirectory(dir=CACHE) as tmp:
        work = pathlib.Path(tmp)
        PROGRESS.begin(estimate="5 s", label="clear the preloader header (boot0)")
        ERASED.touch()
        answer = adb_shell(command=Shell.CLEAR_BOOT0.value).split("\n")[-1].split()
        if answer != ["4096", "0"]:
            read, *still_set = answer or [""]
            if read == "4096" and still_set:
                ERASED.unlink(missing_ok=True)
            _die(message="boot0's header did not read back as cleared. " + again())
        PROGRESS.begin(estimate="20 s", label="writing amonet v1.1.0's bootchain")
        for name, source, seek in (
            ("boot_a", "boot.hdr", 0),
            ("boot_a", "boot.payload", V1_PAYLOAD_SEEK),
            ("boot_b", "boot.hdr", 0),
            ("boot_b", "boot.payload", V1_PAYLOAD_SEEK),
            ("tee1", "tz.img", 0),
            ("tee2", "tz.img", 0),
            ("lk_a", "lk.bin", 0),
            ("lk_b", "lk.bin", 0),
        ):
            write_checked(
                local=amonet / "bin" / source,
                node=node[name],
                seek=seek,
                work=work,
            )
        write_checked(local=twrp, node=node["recovery"], seek=0, work=work)
        PROGRESS.begin(estimate="5 s", label="write misc, slot a marked good")
        if adb_shell(command=Shell.FLUSH.value).split("\n")[-1] != "flushed":
            _die(message="the Dot did not flush its caches. " + again())
        block = bytearray(read_sectors(count=1, start=part["misc"][1] + 1))
        block[BCB_OFFSET - 512 : BCB_OFFSET - 512 + len(BCB)] = BCB
        staged = work / "misc.block"
        staged.write_bytes(bytes(block))
        write_checked(local=staged, node=node["misc"], seek=1, work=work)
        install_fireos(reboot=False, slot="_a")
        write_preloader(image=amonet / "bin" / "preloader.img")
        ERASED.unlink(missing_ok=True)
    run(args=["adb", "reboot"], check=True, timeout=60)
    PROGRESS.begin(estimate="4 min", label="waiting for rooted Fire OS 5 to boot")


def v1_recovery() -> None:
    amonet = unpack(AMONET_V1)
    twrp = fetch(TWRP)
    PROGRESS.begin(estimate="30 s", label=f"waiting for TWRP {TWRP_VERSION}")
    run(
        args=["fastboot", "-S", "256M", "flash", "tee2", "bin/tz.img"],
        check=True,
        cwd=amonet,
        timeout=120,
    )
    run(
        args=["fastboot", "-S", "256M", "flash", "recovery", twrp],
        check=True,
        timeout=120,
    )
    run(args=["fastboot", "oem", "reboot-recovery"], check=True, cwd=amonet, timeout=60)


def v2_payload() -> pathlib.Path:
    amonet = unpack(AMONET_V2)
    return amonet / "brom-payload" / "build" / "payload.bin"


def varint(*, b: bytes, i: int) -> tuple[int, int]:
    r = s = 0
    while True:
        x = b[i]
        i += 1
        r |= (x & 0x7F) << s
        s += 7
        if x < VARINT_MORE:
            return r, i


def wait_for_twrp() -> None:
    time.sleep(15)
    try:
        run(args=["adb", "wait-for-recovery"], timeout=300)
    except subprocess.TimeoutExpired:
        _die(message="the Dot did not reach recovery (TWRP) within 5 minutes")
    deadline = time.monotonic() + 30
    while not adb_shell(command="getprop ro.twrp.version", timeout=30)[
        :1
    ].isdigit() or "mtp" not in adb_shell(command="getprop sys.usb.config"):
        if time.monotonic() > deadline:
            _die(message="TWRP did not finish starting within 30 seconds")
        time.sleep(1)


def warn(text: str) -> None:
    show(
        kind=Kind.WARN, text=paint(code=ANSIColor.YELLOW, text=textwrap.fill(text, 79))
    )


PROGRESS = Progress()


def write(
    *,
    estimate: str,
    label: str,
    number: int | None = None,
    path: pathlib.Path,
    sector: int,
) -> None:
    n = path.stat().st_size
    if sector % 8 == 0 and n % 4096 == 0:
        bs, offset, count = 4096, sector // 8, n // 4096
    else:
        bs, offset, count = 512, sector, n // 512
    verify = (
        f"{SESSION.dd} if={DISK} bs={bs} skip={offset} count={count} 2>/dev/null"
        " | md5sum"
    )
    want = digest(kind="md5", path=path)
    head = min(n, HEAD_CHECK)
    PROGRESS.begin(estimate=estimate, label=label)
    same_head = head == n or not md5_mismatch(
        command=f"{SESSION.dd} if={DISK} bs={bs} skip={offset} count={head // bs}"
        " 2>/dev/null | md5sum",
        want=digest(kind="md5", limit=head, path=path),
    )
    if same_head and not md5_mismatch(command=verify, want=want):
        PROGRESS.end(skipped=True)
        return
    if number is not None:
        start = adb_shell(command=f"cat /sys/class/block/mmcblk0p{number}/start")
        if start.split("\n")[-1].strip() != str(sector):
            restore_failed(
                f"{DISK}p{number} starts at {start or 'nothing'}, not {sector},"
                " so the running partition table is not the one this expects"
            )
        pushed = run(args=["adb", "push", path, f"{DISK}p{number}"], timeout=1800)
        if pushed.returncode != 0:
            restore_failed(f"{label} failed; do not reboot:\n{pushed.stdout}")
    else:
        staged = DOT_TMP / path.name
        pushed = run(args=["adb", "push", path, staged], timeout=120)
        if pushed.returncode != 0:
            restore_failed(
                f"{label} did not reach the Dot; do not reboot:\n{pushed.stdout}"
            )
        notrunc = " conv=notrunc" if SESSION.dd == "toybox dd" else ""
        done = adb_shell(
            command=f"{SESSION.dd} if={staged} of={DISK} bs={bs} seek={offset}"
            f"{notrunc} && rm -f {staged} && echo written"
        )
        if done.split("\n")[-1] != "written":
            restore_failed(f"{label} failed; do not reboot:\n{done}")
    if adb_shell(command=Shell.FLUSH.value).split("\n")[-1] != "flushed":
        restore_failed(label + " could not be flushed; do not reboot")
    wrong = md5_mismatch(command=verify, want=want)
    if wrong:
        restore_failed(f"{label} did not verify{wrong}; do not reboot")
    PROGRESS.end()


def write_checked(
    *, local: pathlib.Path, node: str, seek: int, work: pathlib.Path
) -> None:
    data = local.read_bytes()
    padded = work / local.name
    padded.write_bytes(data.ljust(-(-len(data) // 512) * 512, b"\0"))
    remote = DOT_TMP / local.name
    push_checked(local=padded, remote=remote)
    adb_shell(
        command=Shell.SEEK_WRITE.value.format(dst=node, seek=seek, src=remote),
        timeout=600,
    )
    blocks = padded.stat().st_size // 512
    want = digest(kind="md5", path=padded)
    for attempt in range(PUSH_TRIES):
        read = adb_shell(
            command=f"[ -b {node} ] && dd if={node} bs=512 skip={seek}"
            f" count={blocks} 2>/dev/null | md5sum",
            timeout=600,
        )
        if read.split("\n")[-1].split(" ")[0] == want:
            return
        if attempt + 1 < PUSH_TRIES:
            reconnect(DISK)
    _die(message=f"{local.name} did not read back from {node}. " + again())


def write_preloader(*, image: pathlib.Path) -> None:
    PROGRESS.begin(estimate="5 s", label="write preloader to boot0")
    staged = DOT_TMP / image.name
    push_checked(local=image, remote=staged)
    adb_shell(command=Shell.BOOT0.value.format(src=staged))
    blocks = image.stat().st_size // 512
    want = digest(kind="md5", path=image)
    for attempt in range(PUSH_TRIES):
        read = adb_shell(
            command="d=dd; toybox dd --help >/dev/null 2>&1 && d='toybox dd';"
            f" $d if=/dev/block/mmcblk0boot0 bs=512 count={blocks} 2>/dev/null | md5sum"
        )
        if read.split("\n")[-1].split(" ")[0] == want:
            return
        if attempt + 1 < PUSH_TRIES:
            reconnect(DISK)
    _die(
        message="the preloader did not read back, so boot0 is still cleared"
        " and the Dot restarts into its bootrom. " + again()
    )


def write_system(system: str) -> None:
    slices, want, blocks = system_image()
    ready = adb_shell(
        command="umount /system_root /tmp/fireos-system 2>/dev/null;"
        f' d=$(readlink -f {system}); [ -b "$d" ]'
        f' && ! grep -q -e "^$d " -e "^{system} " /proc/mounts && echo ready'
    )
    if ready.split("\n")[-1] != "ready":
        _die(message=f"{system} is not a block device, or it stayed mounted")
    read_back = (
        "sync; echo 3 > /proc/sys/vm/drop_caches;"
        f" {SESSION.dd} if={system} bs=4096 count={blocks} 2>/dev/null | md5sum"
    )
    carry = streamed_system if stdin_carries() else sliced_system
    for attempt in range(PUSH_TRIES):
        if attempt:
            reconnect(system)
        try:
            said = carry(slices=slices, system=system)
            if not said:
                if adb_shell(command=read_back, timeout=300).split(" ")[0] == want:
                    return
                said = "its md5 read back did not match"
        except subprocess.TimeoutExpired as error:
            said = (
                f"{' '.join(map(str, error.cmd))} did not finish in"
                f" {error.timeout:.0f} seconds"
            )
    if said == "its md5 read back did not match":
        slices[0].unlink(missing_ok=True)
        said += ". The cached image was discarded, so the next run rebuilds it"
    _die(
        message=f"{system} was not written intact after {PUSH_TRIES} tries;"
        f" the last: {said}. " + asked_for()
    )


def write_test() -> None:
    def line(label: str, value: object) -> None:
        show(text=labelled(label=label, value=value))

    def asked(command: str, timeout: float = 60) -> str:
        try:
            return masked(text=adb_shell(command=command, timeout=timeout))
        except subprocess.TimeoutExpired:
            return "<timed out>"

    show(text="Paste everything below into the issue.\n")
    line("state", state().value)
    if not into_recovery(line=line):
        show(text="\nThe test needs a recovery, so it stops here.")
        return
    if asked("toybox dd --help >/dev/null 2>&1 && echo yes").split("\n")[-1] == "yes":
        SESSION.dd = "toybox dd"
    line("dd on the Dot", SESSION.dd)
    node = node_names(listing=asked(f"ls -l {BY_NAME}/")).get(EMMC_TARGET, "")
    sizes = node_sizes(partitions=asked("cat /proc/partitions"))
    blocks = sizes.get(node_held(target=node), 0) // MEBI
    usb_leg(asked=asked, line=line)
    emmc_leg(asked=asked, blocks=blocks, line=line, node=node)
    show(text="\nwhat the kernel says about the eMMC")
    for text in mmc_said(asked=asked):
        show(text=text)


if __name__ == "__main__":
    if sys.argv[1:2] == ["_child"]:
        sys.exit(child_main(*sys.argv[2:]))
    try:
        main()
    except KeyboardInterrupt:
        _die(message="stopped", prefix="")
    except subprocess.TimeoutExpired as error:
        _die(
            message=f"{' '.join(map(str, error.cmd))} did not finish in"
            f" {error.timeout:.0f} seconds"
        )
