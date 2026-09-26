# Rooting

`deploy/dot_root.py` takes a Dot from stock Fire OS 6 to rooted Fire OS 5.5.5.4.
It is a state machine: it finds where the Dot is, does the next stage, and
polls every 2 seconds until the Dot reaches another state. It stops when the
Dot is rooted.

| state | how it is recognised | what the run does |
|---|---|---|
| stock-booted | `adb get-state` says `unauthorized`, or Fire OS 6 without root | prints the fastboot gesture |
| stock-fastboot | `unlock_status` is `false` | amonet v2.0.0 fastbrick |
| v2-twrp, v2-fastboot | unlocked, `lk_build_desc` is not v1's | downgrade to amonet v1.1.0 |
| v1-fastboot | `lk_build_desc` is `f379dba-20170906_000423` | v1 TEE and TWRP, then recovery |
| v1-twrp | recovery, `ro.twrp.version` 3.2.x, `mtp` in `sys.usb.config` | Fire OS 5.5.5.4, boot and /system patches, Magisk 17.3 |
| rooted | `sys.boot_completed` is 1 and `su -c id` answers uid 0 | hides the updater and checks it |

- A probe can land part way through a boot. TWRP answers adb before it sets
  `ro.twrp.version`, and an empty version would read as v2's TWRP and start a
  downgrade. When USB drops during TWRP's MTP switch, `adb shell` answers with
  adb's error text instead. So a version that does not start with a digit is
  `starting`, and the script waits.
- A fastboot read can also come back empty, or time out, while the Dot leaves
  fastboot. An empty `lk_build_desc` would read as v2's and repeat the
  downgrade, `boot0` erase included. So an empty or timed-out read is
  `starting` too, and so is any poll that times out.
- `su` answers about 27 seconds into Fire OS 5's first boot, before the
  package manager runs. So the Dot counts as rooted only once
  `sys.boot_completed` is 1 as well. Otherwise `dumpsys package` answers
  `Can't find service: package` and the updater cannot be hidden.
- The script acts on each state at most once per run, so a stale read cannot
  repeat a stage. The downgrade ends by doing v1-fastboot's stage, so it counts
  as having done both.
- It waits without limit for a Dot to appear, and for the fastboot gesture. Any
  other state that stays unchanged for 10 minutes stops the script, and the
  next run starts that stage again.
- One stage leaves no state a probe can see. After `fastboot erase boot0` the
  Dot has no preloader and shows up only as the bootrom's serial port. So the
  script creates `boot0-erased` in its cache just before the erase, and deletes
  it once v1.1.0's fastboot answers. A later run that finds the file and no Dot
  runs the bootrom step again, without the erase, and asks for the Dot to be
  unplugged and plugged back in. That comes after amonet starts: the bootrom's
  port may already be there, and amonet records the ports it finds at start,
  then waits for one to go and come back. A run that sees the Dot in any state
  deletes the file.

## Recording a run

- `--verbose` prints each `adb` and `fastboot` command before it runs, then
  its exit status, each with the time, and each state the Dot reaches. The
  polls every 2 seconds are not printed.
- `--delay [SECONDS]` counts down before each stage, 10 seconds by default,
  and implies `--verbose`. A recording then shows the Dot settled in each
  state, and the time on each line matches a frame to the last command.
- The delay goes only where a stage begins. Inside a stage, some commands must
  follow each other in time: the fastbrick relies on an 8-second timeout, and
  amonet v1.1.0's bootrom step starts before `fastboot erase boot0` and gets
  60 seconds to find the bootrom's port. A delay there would change what is
  recorded.
- In verbose mode a stage prints its line when it starts and when it ends,
  with no running count, so the command lines stay readable.

What the ring showed, filmed in daylight during `dot_root.py` from stock 8138,
14 min 49 s in all:

| ring | when |
|---|---|
| green | stock fastboot, from the button held at power-on |
| off 1 s, then yellow, shrinking to an arc over about 10 s | the fastbrick flashed |
| red-orange, about 7 s, then green, about 5 s, then off, about 5 s | the exploit, before TWRP |
| blue, about 9 s | v2.0.0's TWRP starting |
| a white flash, a blink off, a cyan arc | v2.0.0's TWRP up; the script reboots it at once |
| off, about 7 s | `adb reboot bootloader` |
| a white flash, then every colour, turning, about 5 s | the exploit's unlocked fastboot |
| off, about 5 min | the bootrom step: `boot0` erased, then amonet v1.1.0 |
| blue, about 10 s, then every colour, turning, about 3 s | v1.1.0's fastboot, while `tee2` and `recovery` are flashed |
| off, about 5 s | `fastboot oem reboot-recovery` |
| deep blue, about 13 s | v1.1.0's TWRP starting |
| blinks off, a cyan arc turning, about 5 s | adb answers, MTP not yet on |
| cyan, whole ring, steady | from MTP on, through the Fire OS push and install |
| off, green, off, green, off, about 5 s | the Fire OS install finishing |
| cyan, whole ring, steady | the boot image, /system and Magisk |
| off, about 6 s | the first reboot into Fire OS 5 |
| deep blue, about 35 s | Fire OS 5 booting |
| cyan and blue, turning, about 3 min | the first boot, until `sys.boot_completed` |
| orange | setup mode, as the script finishes |

A run from stock 5041, filmed at night, showed the same sequence. Its fastbrick
ring read white rather than yellow.

## The host

- The scripts need Python 3.9 or later. 3.9 is the floor, because stock
  macOS's `/usr/bin/python3` is 3.9.6.
  CI byte-compiles the script under 3.9's old parser, which rejects syntax
  only 3.10 documents, and runs its `--help`.
- The script uses the standard library only, so it runs on stock macOS, stock
  Linux and Windows. adb and fastboot are the only external tools.
- adb must be 1.0.36, from platform-tools r24, or newer. 1.0.32 answers
  `wait-for-recovery` with `unknown host service` and passes `shell -n` to the
  device's shell. Ubuntu's apt ships 1.0.41 on 22.04 and 24.04.
- fastboot must accept `-S`, which every flash here uses. Every release back
  to r19 does.
- The script checks both before it starts: `adb version`, and `-S` in
  `fastboot --help`. Old releases were run with no device attached to find
  the floor.
- Downloads go to `$XDG_CACHE_HOME/overdub-root` (default
  `~/.cache/overdub-root`), or `%LOCALAPPDATA%\overdub-root` on Windows. Each
  one is checked against a pinned SHA-256 before use, and on every later run.
- Nothing deletes the cache, so a stopped run, or a later one, downloads
  nothing again. Once the Dot is rooted, the script prints the cache's path
  and size and says it is safe to delete.
- amonet v1.1.0's bootrom step needs pyserial. The script fetches the pure
  wheel from PyPI, pinned by SHA-256, and puts the `.whl` itself on the
  child's `PYTHONPATH`: Python imports a pure wheel straight from the zip.
  Nothing is installed.
- `adb get-state` reports `unauthorized` as an error, on stderr, not on stdout.
  The script reads both.
- Without `ANDROID_SERIAL`, the script uses the one Dot `adb devices -l`
  lists on USB, and ignores network adb: a rooted Dot with tcp/5555 open would
  otherwise make `adb get-state` answer `more than one device/emulator`. It
  picks again on every poll, because the Dot leaves USB at each reboot. With
  two Dots on USB, in adb or in fastboot, it stops and asks for
  `ANDROID_SERIAL`, which fastboot follows too.
- Windows adb may print no `usb:` field. There a line counts as USB unless its
  serial is `host:port` or `emulator-*`. This is not tested.
- An `ANDROID_SERIAL` of the form `host:port` is refused. TWRP starts no
  Wi-Fi, so a Dot rebooted to recovery leaves network adb for good, and
  fastboot and the bootrom are USB-only.

## Unlock: amonet v2.0.0

- The fastbrick image is `fastbrick-20221007.img` when `lk_build_desc` is
  `63cb91b-20221007_072309`, else `fastbrick.img`.
- A flash that returns means the exploit did not start, so it is retried, up to
  10 times. A flash still running after 8 seconds means the exploit is running:
  the script stops fastboot and leaves the Dot to reach TWRP.
- `eMMC-RO` and `Device mismatch` in the output mean the payload refused, and
  the Dot was not modified.

## Downgrade: amonet v2.0.0 to v1.1.0

- v1.1.0's `modules/main.py` must start **before** the Dot reboots: it records
  the serial ports that exist, then waits for a new one. The erase runs only
  if main.py is still running 3 seconds after it starts.
- From v2's TWRP the script reboots to fastboot first, and stops there if
  `lk_build_desc` is already v1.1.0's. A second erase would gain nothing.
- Then `fastboot erase boot0` and `fastboot reboot` drop the Dot into its
  bootrom. main.py reads newlines from stdin at its prompts; the script feeds
  them. It is done when its log says `Reboot to unlocked fastboot`.
- The bootrom is `0e8d:0003`. On macOS it is `/dev/cu.usbmodem*`. On Linux it
  is `/dev/ttyACM*`, which needs the `dialout` group or root, and
  ModemManager can grab it first. On Windows it is a COM port, and the driver
  it needs is not known.
- main.py's `serial_ports()` skips a port it cannot open, silently, and waits
  forever. So the script gives it 60 seconds after the reboot to log
  `Found port`, then stops it and says why a port may be missing.
- v1.1.0's own `fastboot-step.sh` ships a Linux-only fastboot. The script runs
  its three commands with the host's fastboot instead: `bin/tz.img` to `tee2`,
  `bin/twrp.img` to `recovery`, then `fastboot oem reboot-recovery`.

## Install: v1's TWRP 3.2.3

- TWRP 3.2.3 prints `__bionic_open_tzdata...` into every `adb shell` output.
  Those lines are dropped before anything is parsed.
- adb answers before TWRP turns on MTP: 1.5 seconds before in one boot, 5 in
  another. Then TWRP moves USB to `mtp,adb`, the Dot drops off USB, and it
  comes back as a new adb transport. A push under way fails with `failed to
  read copy response: EOF`. `dot_root.py` waits for `mtp` in `sys.usb.config`,
  which is empty until then. Pushes are still tried 3 times.
- Its `adb shell` exit status is unreliable. So each device-side step is a
  pushed script that ends by printing `root-step-ok`, and the script checks
  that marker. The scripts are written with `\n` line endings on every host.
- TWRP 3.2.3 has no `twrp format data`. userdata is formatted with
  `mke2fs -t ext4 -b 4096 <userdata> <blocks - 256>`, which leaves 1 MiB for
  the crypto footer. busybox's fstab has no type, so the mount is
  `mount -t ext4 <dev> /data`.
- Then `/data` must be checked as a mountpoint. Otherwise the 397 MB Fire OS
  push lands in TWRP's RAM, TWRP dies, and the Dot reboots.
- Never `umount -a` in TWRP: it unmounts `/proc`.
- Every image, zip and database push is read back by md5 on the device.

## The boot image

- Magisk 17.3's `arm/magiskboot` is dynamically linked. `/system` is mounted
  first, and `LD_LIBRARY_PATH=/system/lib` is set on the magiskboot calls
  only. Set globally, it breaks TWRP's 64-bit tools.
- The ramdisk loses `verify` from every fstab, and `default.prop` gets
  `ro.secure=0`, `ro.debuggable=1` and `persist.sys.usb.config=mtp,adb`.
- The cmdline is the 512-byte header field at offset 64. Stock is
  `bootopt=64S3,32N2,64N2`; the script appends
  ` androidboot.selinux=permissive`.
- The patched image is padded to a 4096-byte multiple and written with `dd`.
  After `sync` the page cache is dropped, and the partition is read back by
  md5 over the image's length.

## /system

- `/system/etc/init.fosflags.sh` strips adb from the USB configuration on
  every boot after the first. The script forces its `FOS_FLAGS_ADB_ON` branch
  true and neutralizes its `unset_adb_persistent_property` calls, then checks
  both edits took. Without them adb is gone from the second boot on.
- The 4 Amazon update hosts go into `/system/etc/hosts` as `127.0.0.1`.

## Magisk

- Magisk 17.3 installs through `twrp install`.
- Its database is seeded so the adb shell gets root without a prompt the Dot
  cannot show: `/data/adb/magisk.db`, mode 600, with
  `policies(uid INT, package_name TEXT, policy INT, until INT, logging INT,
  notification INT)` holding `(2000, 'com.android.shell', 2, 0, 1, 0)`.

## The updater

- An update would replace the boot image and remove root. On the first rooted
  run the script runs `pm hide com.amazon.device.software.ota`, then reads
  `hidden=true` back from `dumpsys package`.
