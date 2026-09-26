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
  its exit status, each with the time. `dot_root.py` also prints each state
  the Dot reaches. Its polls every 2 seconds are not printed.
- `--delay [SECONDS]` counts down before each stage, 10 seconds by default in
  `dot_root.py` and 5 in `dot_restore_stock.py`, and implies `--verbose`. A
  recording then shows the Dot settled in each state, and the time on each
  line matches a frame to the last command.
- The delay goes only where a stage begins. Inside a stage, some commands must
  follow each other in time: the fastbrick relies on an 8-second timeout, and
  amonet v1.1.0's bootrom step starts before `fastboot erase boot0` and gets
  60 seconds to find the bootrom's port. A delay there would change what is
  recorded.
- In verbose mode a stage prints its line when it starts and when it ends,
  with no running count, so the command lines stay readable.

What the ring showed, filmed during `dot_restore_stock.py 6302` from a rooted
Dot that was not set up:

| ring | when |
|---|---|
| purple | rooted Fire OS 5, setup timed out (`anim_OOBE_start_error`) |
| off, about 12 s | `adb reboot recovery` |
| deep blue, about 10 s, a brighter segment turning in its last 3 | v1.1.0's TWRP starting, before adb answers |
| a blink off, then a cyan arc turning, about 3 s | TWRP up: `adb wait-for-recovery` returns |
| cyan, whole ring, steady | through all 15 steps |
| off, about 8 s | the reboot into stock |
| deep blue, about 50 s | stock Fire OS 6 booting |
| cyan and blue, turning | Fire OS 6 starting Alexa |
| orange | setup mode, about 90 s after the reboot |

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
- Both scripts check `adb version` before they start, and `dot_root.py` checks
  for `-S` in `fastboot --help`. Old releases were run with no device attached
  to find the floor.
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
- Without `ANDROID_SERIAL`, both scripts use the one Dot `adb devices -l`
  lists on USB, and ignore network adb: a rooted Dot with tcp/5555 open would
  otherwise make `adb get-state` answer `more than one device/emulator`.
  `dot_root.py` picks again on every poll, because the Dot leaves USB at each
  reboot. With two Dots on USB, in adb or in fastboot, the scripts stop and ask
  for `ANDROID_SERIAL`, which fastboot follows too.
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
  read copy response: EOF`. Both scripts wait for `mtp` in `sys.usb.config`,
  which is empty until then. `dot_root.py` also tries each push 3 times.
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

## Back to stock: dot_restore_stock.py

`deploy/dot_restore_stock.py <build>` returns a Dot on amonet v1.1.0 to stock
Fire OS 6, so that `dot_root.py` can be tested from a clean start. It follows
`dot_root.py`'s host rules: Python 3.9 or later, the standard library, and adb
as the only external tool. CI compiles it and runs its `--help` in the same
job.

- It starts from v1's TWRP 3.2.3, and reboots a Dot booted with adb into it.
  Any other TWRP stops it: the table surgery below reads v1's layout. It
  waits up to 30 seconds for TWRP to report a version and turn on MTP, and
  reads a version that does not start with a digit as not yet set.
- It overwrites the whole eMMC of the one Dot on USB, or of `ANDROID_SERIAL`.
  It never picks among several.
- It knows 6 builds: 4405 (6.5.5.6, the oldest with a `payload.bin`), 5041
  (6.5.0.5), 6302 (6.4.6.6), and 8138, 8142 and 8146 (6.5.7.4.1). All ship the
  same LK, `63cb91b-20221007_072309`. 6.5.5.5 (4310M) and Fire OS 5 ship a
  block image (`system.new.dat`) instead, which the script cannot write.
- The OTA comes from Amazon's CloudFront, through the build's FTVDB page.
  FTVDB keys each build by the OTA's md5, so the md5 finds the page. The pin
  is the OTA's SHA-256, taken from a download that matched that md5.
  Downloads and images go to `overdub-stock` beside `dot_root.py`'s cache.
  After the reboot the script prints its path and size, as `dot_root.py`
  does.
- The images come out of the OTA's `payload.bin`: a protobuf manifest, then
  `REPLACE`, `REPLACE_BZ` and `REPLACE_XZ` operations. Each image is checked
  against the SHA-256 the manifest gives it, and a cached image is checked
  again on every run.
- Each operation's data is read from `payload.bin` where it lies, so the peak
  memory is the image being built plus one operation: about 1 GB, for the
  768 MB system image.

### The partition table

- amonet v1.1.0 renames the stock `boot_a` and `boot_b` to `boot_a_x` and
  `boot_b_x`, and adds its own `boot_a` and `boot_b` as partitions 17 and 18,
  cut from the end of userdata.
- The stock table is built from the Dot's own: drop amonet's two, strip the
  `_x`, and extend userdata to the last usable LBA. That leaves 16 partitions,
  and the script stops on any other count.
- Before it is rebuilt, the table must have a 92-byte header, 128 entries of
  128 bytes, and both CRCs right. A run cut during the primary table's write
  leaves it damaged, with a mix of stock and amonet entries. The backup, which
  goes in first, is then read instead. If neither is intact, the script stops.
- A table with no `_x` name is already stock, and is kept as it is. Its
  `boot_a` and `boot_b` are the stock ones. So a run stopped after the table
  went in can be run again, and it redoes every step.
- Every offset and size the script writes comes from that table, not from
  constants. The backup table goes to the last 33 sectors, before the primary.
- After both, `sgdisk --verify` must report no problems, and the kernel rereads
  the table. p17 and p18 must be gone from `/proc/partitions`, and userdata
  must be its stock size, before cache and userdata are formatted. Against the
  old table, `mke2fs` would size userdata to amonet's cut.

### The writes

- system, boot, TEE and LK go to both slots, so the Dot boots stock whichever
  slot it picks.
- boot is padded with zeros to its 16 MiB partition, so no byte of amonet's
  boot image survives, and the read-back covers the whole partition.
- expdb holds amonet's payload and misc holds the slot metadata. Both are
  zeroed.
- Each write is `adb exec-in` into `dd`, then `sync`, then the page cache is
  dropped, then an md5 read back from the same range. Without the drop, the
  read comes from the kernel's cache of the write. It then proves that adb
  delivered the bytes, not that the flash holds them. `dd` uses 4096-byte
  blocks where the offset and size allow, else 512.
- Nothing is written before every image is built and checked to fit its
  partition. Then a 10-second countdown runs, and Ctrl-C there stops the
  script with nothing written. No input is needed to go on.
- The preloader goes to `boot0` last, with `force_ro` lifted for the write and
  set again after it. Every failure after the countdown says "do not reboot":
  the Dot is part way between amonet and stock, and TWRP is still up to fix it.
- A run stopped part way, by Ctrl-C or a failed check, leaves TWRP up. Do not
  reboot: run the script again. It redoes every write and reads each one back.
- Keep the restored Dot off Wi-Fi until `dot_root.py` finishes. On Wi-Fi it
  can take an update, to a build the script has not met or the one under
  test.
