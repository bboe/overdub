# Rooting

`deploy/dot_firmware.py` takes a Dot from stock Fire OS 6 to rooted Fire OS
5.5.5.4. It polls the Dot every 2 seconds, does the stage for the state it
finds, and stops when the Dot is in the state its optional target names. The
table gives the stages for the default, `v1-bboe`; [Targets](#targets) has the
others, and [Back to stock](#back-to-stock-the-stock-target) the `stock`
target.

| state | how it is recognised | what the run does |
|---|---|---|
| stock-booted | `adb get-state` says `unauthorized`, or Fire OS 6 without root | prints the fastboot gesture, and the + gesture for a Dot already on amonet v2.0.0 |
| stock-fastboot | `unlock_status` is `false` | amonet v2.0.0 fastbrick |
| locked-v1-fastboot | `unlock_status` is `false`, and `lk_build_desc` is v1's: v1's own fastboot, which is locked | the same |
| emos | no adb or fastboot, and one serial port with USB ID `1949:2007` | `/init recovery` at emOS's serial console |
| v2-booted | Fire OS 6, and `id` or `su -c id` answers uid 0 | `adb reboot recovery`, into v2.0.0's TWRP; ends a `v2` run |
| amonet-v2-twrp | unlocked, `lk_build_desc` is not v1's, `mtp` in `sys.usb.config` | makes room for amonet v1.1.0 in the table, then reboots so the kernel rereads it |
| amonet-v2-twrp-v1-table | amonet-v2-twrp, and `sgdisk --print` lists `boot_a_x` | writes amonet v1.1.0's chain and Fire OS 5.5.5.4, preloader last |
| v2-fastboot | unlocked, `lk_build_desc` is not v1's | downgrade to amonet v1.1.0 through the bootrom |
| v1-fastboot | `lk_build_desc` is `f379dba-20170906_000423` | v1 TEE and TWRP 3.7.0_9-bboe2, then recovery |
| amonet-v1-twrp | recovery, v1's `lk_build_desc`, another `ro.twrp.version`, `mtp` in `sys.usb.config` | writes TWRP 3.7.0_9-bboe2 to `recovery`, reboots into it |
| bboe-v1-twrp | recovery, v1's `lk_build_desc`, `ro.twrp.version` 3.7.0_9-bboe2, `mtp` in `sys.usb.config` | Fire OS 5.5.5.4, boot and /system patches, Magisk 17.3 |
| rooted-bboe, rooted-v1, rooted | `sys.boot_completed` is 1 and `su -c id` answers uid 0; `recovery` hashes as TWRP 3.7.0_9-bboe2, as v1.1.0's TWRP 3.2.3, or as neither | the target's state: hides the updater and checks it; another: writes the target's TWRP to `recovery` |

- A probe can land part way through a boot. TWRP answers adb before it sets
  `ro.twrp.version`, and while USB drops for MTP, `adb shell` answers with
  adb's error text. Either would read as v2's and start a downgrade. So an
  `lk_build_desc` not shaped like `f379dba-20170906_000423`, or a version not
  starting with a digit, reads as `starting`. The downgrade checks the shape
  too.
- An empty `lk_build_desc` would read as v2's and repeat the downgrade, `boot0`
  erase included. So an empty or timed-out fastboot read, and any poll that
  times out, reads as `starting` too.
- A rooted Fire OS 6 Dot is on amonet v2.0.0, because Fire OS 6 has no root
  without the unlock. Its root is Magisk's `su`, or an `adbd` that runs as
  root and no `su` at all, which the XDA thread's `boot-root.zip` leaves. So
  the probe asks both. Before this state, the first read as rooted and the
  second as stock-booted, which asked for a gesture `adb` can replace.
- A Dot unlocked with amonet v2.0.0 that boots Fire OS 6 without root also
  answers `unauthorized`, so it reads as stock-booted. amonet v2 removes
  stock fastboot, and the action-button gesture on a v2 Dot was never tried.
  So the message also names + at power-on, which starts v2.0.0's TWRP.
- `su` answers about 27 seconds into Fire OS 5's first boot, while
  `dumpsys package` still answers `Can't find service: package`. So rooted
  also needs `sys.boot_completed` to be 1.
- Each state is acted on at most once per run, so a stale read cannot repeat a
  stage. The downgrade ends with v1-fastboot's stage, and counts as both.
- That stage needs amonet's fastboot, and the bootrom step does not always
  leave the Dot in it. On a resumed run reported from Windows the step wrote
  v1.1.0 and the Dot came back in v1's own **locked** fastboot: a solid green
  ring, `unlock_status` `false`, and `restricted on locked hw` from the first
  `fastboot flash`, which is all a locked LK answers. So the stage reads
  `unlock_status` first and returns where it is `false`, and a stage that
  returns that way counts none of the states it would otherwise pass. That
  leaves locked-v1-fastboot's own stage to unlock the Dot again, where the
  once-per-run rule would otherwise have the run wait out its 10 minutes on a
  state already counted. The chain then goes
  in from v2.0.0's TWRP, which is the route a rerun took to rooted Fire OS 5
  in 7 min 45 s.
- Why the Dot came back locked is not known. Its table already held
  `boot_a_x`, so the surgery had finished, and the preloader and LK were
  v1.1.0's. What that leaves is the microloader in amonet's own `boot_a` and
  `boot_b`, or the boot control block.
- The script waits without limit for a Dot and for the fastboot gesture. Any
  other state unchanged for 10 minutes stops it, and the next run redoes that
  stage.
- Rooting formats userdata. A registered Dot loses its Wi-Fi and its Alexa
  registration, and finishes in setup mode.
- After boot0 is erased the Dot shows up only as the bootrom's serial
  port, which no probe sees. So the script creates `boot0-erased` in its cache
  before the erase. It deletes the file once amonet logs `Reboot to unlocked
  fastboot`, when boot0 is written, and whenever it sees the Dot booted or in
  fastboot, which only a preloader in boot0 reaches. TWRP does not count: it
  keeps running from RAM after the restore to stock clears boot0.
- A run that finds the file and no Dot runs the bootrom step again, without
  the erase. A run stopped inside amonet's payload leaves the Dot there, and
  the payload never restarts. So the resume first sends every MediaTek port
  the payload's reboot command, `0xf00dd00d` then `0x3000`; the Dot came back
  as its bootrom within 5 seconds. Sent to a live bootrom on a Dot, it did
  no harm: the handshake that followed went through.
- The command goes out before amonet starts. amonet records the ports at
  start and takes any port that appears later as the Dot, so a command sent
  after it starts could reach a Dot it is already talking to. On a resume it
  also takes a bootrom port, `0e8d:0003`, that was there at start: that is
  the Dot, whose window may close before any other port appears.
- On macOS, a run stopped 12 seconds into amonet's `tz` write resumed this
  way: amonet found the bootrom 4 seconds after the run started, and the root
  finished.
- From Linux, with boot0 erased, the resume went on after 30 seconds, twice,
  with no one touching the Dot. From macOS the bootrom gives one window per
  power-on: after `adb reboot` from TWRP it appeared 4 s later, stayed 40 s,
  and then showed nothing on USB for the 2.5 minutes watched. Only a replug
  opened another window.
- So a resume started inside that window needs no replug: a Dot was
  replugged, and a resume started 8 s after its bootrom appeared took that
  port. The reset command had reached the bootrom first, and the handshake
  still went through. A resume started after the window closes asks for the
  replug.
- A run stopped while amonet sends its payload, before the payload runs,
  leaves the bootrom inside its write command, waiting for the rest of the
  payload's words. The reset command reaches it as data. amonet's handshake
  sends `0xa0` and waits for `0x5f` with no limit, and the bootrom echoes a
  word only after 4 bytes, so 3 reads in 4 wait out amonet's 5-second
  timeout. Stopped 0.3 s into a 0.8 s send, about 12 KB were left: 12 hours
  of handshake. The run had disabled the watchdog, so only a power cycle
  clears it.
- So the handshake gives up after 10 seconds without `0x5f`, where a live
  bootrom answers in about 2 ms. The run then asks for a replug, waits for
  the port to go and come back, and tries again. Under `--short` it stops
  instead: a replug without the short brings up the preloader, and the
  Enter that ends amonet's short prompt has already been sent, so a second
  short would get no countdown. Before this, the replug
  failed the run with `Device not configured`, and a rerun inside the new
  window went on through the root. With the limit, on a Dot stopped the same
  way, the resume asked for the replug 10 s after it took the port, and the
  same run finished the root to v1-bboe in 9 min 6 s.

## A Dot on emOS

- EchoMuse's emOS replaces Fire OS's boot image with its own init. It runs no
  adbd, so adb and fastboot see nothing. Its init offers a root shell on a
  USB serial port instead: `1949:2007`, named `EchoMuse` and `emOS`, with the
  Dot's serial number. Measured on a Dot with emOS 0.10 on amonet v2.0.0.
- That shell's `/init recovery` reboots into the bootloader's TWRP, and the
  port goes away within a second. From v2.0.0's TWRP, 20 s later, the root
  goes on as from amonet-v2-twrp. `stock` takes the same path: from emOS
  0.10 on amonet v1.1.0 with TWRP 3.7.0_9-bboe2 it restored 8146 in 3 min
  3 s.
- Finding the port by USB ID needs pyserial, so the probe runs a child with
  the bootrom step's pinned wheel. It runs only when adb lists no Dot.
- A port that will not open, as on Linux without the udev `tty` line, stops
  the run with the rules and the commands that add them.
- The child opens the port and sends a newline. A prompt ending in `#` gets
  `/init recovery`. One ending in `password:` stops the run: EchoMuse's
  dashboard sets that password, and the user types the command or clears
  it. Nothing that matches in 5 seconds stops the run too.
- pyserial opens a port raw. A port left to echo, as a terminal opens one,
  feeds the shell its own output and every command returns 127.

## A Dot that shows no light

- A Dot whose preloader runs but never starts LK shows no light and offers
  no fastboot: only `0e8d:2000`, the preloader, for about a second every
  30 seconds. Its boot0 is intact, so only the eMMC test-point short reaches
  the bootrom. `--short` runs the bootrom step for it, with no erase.
- Neither the gesture nor `FACTFACT` sent to the preloader reached fastboot.
  mtkclient's four ways to crash a preloader into the bootrom all failed on
  Amazon's: each came back as the preloader. Its register writes echoed but
  took no effect.
- The bootrom step takes only a port it can identify as the bootrom,
  `0e8d:0003`, vendor and product both, and skips a stock preloader's, which
  `--short` reports as a missed short. amonet would otherwise take whichever
  port appears first.
- It reads ports from pyserial's USB list and opens only the bootrom's, where
  amonet opened every port to see it. On Linux the preloader's
  `/dev/ttyACM*` is `dialout`'s, and the udev rule covers only `0e8d:0003`,
  so amonet's way would never see a missed short.
- It remembers each port by name and USB ID, and checks every port on each
  poll. The bootrom usually reuses the preloader's name: on Linux both are
  `/dev/ttyACM0`, and macOS names a port by its USB location. A port whose ID
  cannot be read yet, or that will not open yet, is looked at again on the
  next poll.
- `--short` applies only to a Dot first seen with nothing on USB. Any other
  state turns it off, so a later reboot cannot start the short flow. That
  includes `starting`, which covers a probe that timed out as well as a Dot
  half started; after a timeout the run asks for fastboot, and a rerun with
  `--short` goes on.
- amonet takes any port that appears after it lists the ports at start. So
  `--short` asks for the short only once amonet logs that it is waiting, and
  sends no reset first: a Dot plugged in earlier would be in that list and
  never count as new.
- The short must be off before the payload starts the eMMC, its first act.
  Nothing before that touches the eMMC, and the handshake disables the
  watchdog. So the run holds amonet at its "Remove the short" prompt, counts
  down 5 seconds, then lets it go on.
- amonet's first write clears boot0's header. Under `--short` its bootrom
  step creates `boot0-erased` just before each write, after amonet's first
  boot0 check, as the erase path creates it before the erase. A run that
  stops before that leaves boot0 intact and no file. A run that fails or is
  stopped after it is resumed by a plain rerun.
- The bootrom step logs the error for a port it still cannot open after 1
  second, once, and the run shows it at once. udev can set a new port's mode
  a moment after it appears, so a first failure alone means nothing. A
  missing udev rule or ModemManager would otherwise hold a `--short` run
  silent for 10 minutes.
- The payload starts the eMMC once. With the short still on, either it never
  comes up and amonet times out waiting for it, or the bootrom step's first
  read, the partition table's `55 AA`, fails. Both stop the run before amonet
  writes anything, and a new short starts over.

## Recording a run

- `--verbose` prints each `adb` and `fastboot` command and its exit status
  with the time, and each state the run sees. The 2-second polls are not
  printed.
- In verbose mode, or when the output is not a terminal, a stage prints a line
  when it starts and when it ends, with no running count. In verbose mode
  both lines start with the time. Output that is not a terminal also gets
  no download meter. Either would fill a log with `\r`
  frames.
- A running stage shows a Braille spinner where its mark will go, and a
  finished stage prints ✅. Where the output's encoding cannot represent them,
  the spinner is `|/-\` and the mark is `done`. On Windows that is output
  redirected to a file: since Python 3.6 a console reports UTF-8 whatever its
  code page.
- A finished line is at most 79 columns, so it does not wrap on an 80-column
  terminal. That caps a stage label at 36 characters.
- Steps are numbered from 1 in each run, against the total that
  [Targets](#targets) says how the run counts. The restore is 16 of them. A
  restore write whose target already reads back the right md5 ends `skip`,
  in place of its seconds.
- Each stage's estimate is an upper bound: the slowest time measured for it,
  rounded up to the next 5 seconds, over roots on macOS and Windows 11. One
  case is left out: a first run on a computer that finds the Dot already in
  TWRP waits up to 35 s more for the system image.
- The estimates and the rings are from TWRP 3.2.3. One root on macOS with
  TWRP 3.7.0_9-bboe1 came in under every estimate. Its replace step took 37 s
  on a Dot.

The ring during `dot_firmware.py stock 6302`, from a rooted Dot not set up:

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

The ring during `dot_firmware.py root` from stock 8138, in daylight, 14 min
49 s in all:

| ring | when |
|---|---|
| green | stock fastboot, from the button held at power-on |
| off 1 s, then yellow, shrinking to an arc over about 10 s | the fastbrick flashed |
| red-orange, about 7 s, then green, about 5 s, then off, about 5 s | the exploit, before TWRP |
| blue, about 9 s | v2.0.0's TWRP starting |
| a white flash, a blink off, a cyan arc | v2.0.0's TWRP up; the script reboots it at once |
| off, about 7 s | `adb reboot bootloader` |
| a white flash, then every color, turning, about 5 s | the exploit's unlocked fastboot |
| off, about 5 min | the bootrom step: `boot0` erased, then amonet v1.1.0 |
| blue, about 10 s, then every color, turning, about 3 s | v1.1.0's fastboot, while `tee2` and `recovery` are flashed |
| off, about 5 s | `fastboot oem reboot-recovery` |
| deep blue, about 13 s | v1.1.0's TWRP starting |
| blinks off, a cyan arc turning, about 5 s | adb answers, MTP not yet on |
| cyan, whole ring, steady | from MTP on, through the Fire OS push and install |
| off, green, off, green, off, about 5 s | the Fire OS install finishing |
| cyan, whole ring, steady | the boot image, /system and Magisk |
| off, about 6 s | the first reboot into Fire OS 5 |
| deep blue, about 35 s | Fire OS 5 booting |
| cyan and blue, turning, about 3 min | the first boot, until the setup app starts |
| orange | setup mode, about 1.5 minutes before the script finishes |

A run from stock 5041, at night, showed the same, except that the fastbrick
ring read white.

- The ring turns orange before the boot is done. On a Dot, 2026-10-04, the
  setup app started and the ring turned orange 30 s before
  `BOOT_COMPLETED` went out. The first `su` came 13 s after that. The last
  step ended about a minute later, 322 s after `adb reboot`. The end is an
  estimate from the Dot's uptime, because the step logged no time. The
  minute held Magisk's first `su`, the 2-second probes, and
  `hide_updater`'s `dumpsys package`.

## The host

- Python 3.9 or later, because stock macOS's `/usr/bin/python3` is 3.9.6. CI
  byte-compiles the script under 3.9's parser and runs its `--help`.
- The standard library only, so the script runs on stock macOS, Linux and
  Windows. adb and fastboot are the only external tools.
- On a terminal, `ERROR:` lines are red and warnings yellow. `NO_COLOR`,
  `TERM=dumb`, a pipe or a file turns that off. On Windows only Windows
  Terminal (`WT_SESSION`) gets color; the old console prints the escape codes
  as text. A blank line separates one kind of message from another.
- adb must be 1.0.36 (platform-tools r24) or newer: 1.0.32 answers
  `wait-for-recovery` with `unknown host service` and passes `shell -n` to the
  device. Ubuntu 22.04 and 24.04 ship 1.0.41. fastboot must accept `-S`, as
  every release back to r19 does. Every run checks `adb version` and
  `fastboot --help` for `-S`, the stock target included, because a route to
  stock can pass through fastboot. Old releases, run with no device, set
  these floors.
- Downloads go to `$XDG_CACHE_HOME/overdub-firmware` (default
  `~/.cache/overdub-firmware`), or `%LOCALAPPDATA%\overdub-firmware` on
  Windows. The script moves what the old `overdub-root` and `overdub-stock`
  hold into it before it reads the cache, so an interrupted downgrade's
  marker survives. A name already in the new folder stays in the old. Each
  is checked against a pinned SHA-256 before use and on every run. The amonet
  trees unpacked from the two zips are not, and neither is the system image
  built from Fire OS's.
- The system image's folder is named after the zip's hash, so a new zip builds
  a new image. Its `md5` file is written last and synced with the image before
  the folder is renamed into place, so the script trusts a folder only when
  `md5` is in it, and rebuilds otherwise. If the md5 read back still fails
  after the last try, the script deletes `md5` alone, which works even when
  another process holds the image open.
- The run starts the downloads in a background thread at the first probe
  that does not find the Dot booted, rooted or starting. So they overlap the
  wait for a Dot and for the fastboot gesture. A Dot found rooted needs no
  download for `v1` or `v1-bboe`. For `stock` and `v2` the main thread
  downloads the build before the first stage.
- The main thread waits for that thread's locks half a second at a time.
  On Windows, Ctrl-C does not interrupt a blocking lock wait, so one long wait
  would ignore it until a 397 MB download ended.
- The downloads are about 476 MB. With the unpacked trees and the system
  image, the cache holds about 1.3 GB.
- Before it changes the Dot, the script waits for every download and checks
  it. A bad download then stops the run with the Dot unchanged.
- The downloads run one at a time, amonet v2.0.0's zip first, because the
  fastbrick needs it first. Fire OS is 397 MB of the 476, and one download
  filled the line at about 40 MB/s, so parallel downloads would only delay
  that zip.
- Nothing deletes a download, so a later run downloads nothing. A rooted run
  prints the cache's path and size and says it is safe to delete.
- The emOS probe and the bootrom step need pyserial, and the probe runs on
  every poll that lists no Dot, so every run fetches the wheel. The script puts
  the pinned pure wheel from PyPI on the child's `PYTHONPATH`, and Python
  imports it straight from the zip. Nothing is installed.
- Each child is the script itself, run as `dot_firmware.py _child <name>`.
  pyserial is on the child's path only, and the bootrom child also needs
  amonet's `modules` directory, so their imports wait until the child runs.
- A child drops the script's own directory from its import path first. A
  script downloaded to `~/Downloads` would otherwise import any `serial.py`
  there in place of the pinned wheel. It checks the entry first: under
  `PYTHONSAFEPATH` Python adds no such directory, and the first entry is the
  wheel.
- `adb get-state` reports `unauthorized` on stderr, so the script reads both
  streams.
- Without `ANDROID_SERIAL`, the run uses the one Dot on USB in
  `adb devices -l`, and ignore network adb: a rooted Dot with tcp/5555 open
  would make `adb get-state` answer `more than one device/emulator`.
  The run picks again on every poll, because each reboot drops the Dot
  off USB. With two Dots on USB the run stops and asks for
  `ANDROID_SERIAL`, which fastboot follows too.
- An `ANDROID_SERIAL` of the form `host:port` is refused: TWRP starts no
  Wi-Fi, and fastboot and the bootrom are USB-only.
- Windows 11's adb prints no `usb:` field, so there a line counts as USB unless
  its serial is `host:port` or `emulator-*`.

### Linux permissions

- adb, fastboot and the bootrom's serial port need the Dot's USB devices open
  to the user. Ubuntu's `android-sdk-platform-tools-common` and
  android-udev-rules, which LineageOS points to, list other Lab126 products
  but not a booted Dot, `1949:0112`. Other guides for this Dot use `sudo`.
  These rules, in `/etc/udev/rules.d/51-echo-dot.rules`, cover every state,
  the serial port included:

  ```
  SUBSYSTEM=="usb", ATTR{idVendor}=="1949", MODE="0660", GROUP="plugdev", TAG+="uaccess"
  SUBSYSTEM=="usb", ATTR{idVendor}=="18d1", ATTR{idProduct}=="4ee2", MODE="0660", GROUP="plugdev", TAG+="uaccess"
  SUBSYSTEM=="usb", ATTR{idVendor}=="18d1", ATTR{idProduct}=="d001", MODE="0660", GROUP="plugdev", TAG+="uaccess"
  SUBSYSTEM=="usb", ATTR{idVendor}=="0bb4", ATTR{idProduct}=="0c01", MODE="0660", GROUP="plugdev", TAG+="uaccess"
  SUBSYSTEM=="usb", ATTR{idVendor}=="0e8d", ATTR{idProduct}=="0003", MODE="0660", GROUP="plugdev", TAG+="uaccess"
  SUBSYSTEM=="tty", ATTRS{idVendor}=="0e8d", ATTRS{idProduct}=="0003", MODE="0660", GROUP="plugdev", TAG+="uaccess"
  SUBSYSTEM=="tty", ATTRS{idVendor}=="1949", ATTRS{idProduct}=="2007", MODE="0660", GROUP="plugdev", TAG+="uaccess"
  ```

  Then `sudo udevadm trigger`. Fire OS 5 is `1949:0112` (`mtp,adb`, set by its
  own init scripts, the same on every Dot), TWRP `18d1:d001` and
  `18d1:4ee2`, fastboot `0bb4:0c01`, the bootrom `0e8d:0003`, and emOS's
  serial console `1949:2007`. The `usb` line for `1949` covers emOS's USB
  device but not its `/dev/ttyACM*` node, which is `dialout`'s without the
  `tty` line.
- `uaccess` covers a user at the machine; over SSH the user must be in
  `plugdev`. A new group reaches only new processes, and a running adb server
  keeps its old permissions: with the group added and the old server up,
  `adb devices` listed nothing. So after `adb kill-server`, `sg plugdev -c`
  runs a script with the group, without a new login.
- The script refuses to run as root: the downloads would belong to root, and
  the rules make `sudo` needless.
- It checks at start that the process has `plugdev`, even for a desktop user
  covered by `uaccess`, so the check is one question. A user not in the group
  gets the rules and the commands to add them, starting with
  `groupadd -f plugdev` because Fedora and Arch have no such group. A user in
  `/etc/group` whose shell predates that gets `adb kill-server` and the `sg`
  line. The `sg` line repeats the command as run, interpreter, path and flags
  included, because `./dot_firmware.py` is wrong from another directory.
- Without the rules adb lists the Dot as `no permissions`. When that lasts 5
  seconds and nothing else listed is usable, the run stops and prints
  the same rules and commands. A shorter spell is udev still setting
  permissions. A phone the user cannot open, beside a Dot that answers, does
  not stop them.

## Unlock: amonet v2.0.0

- The fastbrick image is `fastbrick-20221007.img` when `lk_build_desc` is
  `63cb91b-20221007_072309` or `41fb3ce-20221007_151724`, else
  `fastbrick.img`. That one payload takes both bootloaders, where amonet
  v2.0.0's own map lists only the first. The second ships in 4315 alone,
  which the default image does not unlock.
- A flash that returns means the exploit did not start, so it is retried, up to
  10 times. A flash still running after 8 seconds means the exploit is running:
  the script stops fastboot and leaves the Dot to reach TWRP.
- `eMMC-RO` or `Device mismatch` means the payload refused, and the Dot is
  unchanged.

## Targets

| target | ends in | stops at |
|---|---|---|
| `v1-bboe`, the default | amonet v1.1.0, TWRP 3.7.0_9-bboe2, rooted Fire OS 5.5.5.4 | rooted-bboe |
| `v1` | the same, with v1.1.0's own TWRP 3.2.3 | rooted-v1 |
| `v2` | amonet v2.0.0's TWRP, Fire OS 6 8146 in both slots, `boot-root.zip` | v2-booted |
| `stock BUILD` | Amazon's Fire OS 6 `BUILD`, the whole Dot erased | stock-booted |

- Each poll picks the stage for the state found and the target, so a run
  goes from any state to any target, a rooted Dot included. Every state has
  at most one stage per target, so a table does what a search over the
  stages would.
- Each stage in the table says how many steps it prints and which state it
  leaves the Dot in. Before each stage the run walks the table from the
  current state to the target's and shows that total. A walk that meets a
  state it cannot predict past, such as emOS, whose TWRP depends on the
  chain under it, shows `?`. A Dot that lands somewhere else is walked
  again from there, so the total changes only when the Dot surprised the
  run. Step numbers only go forward.
- `v1` and `v1-bboe` write the same /system, boot image and Magisk. Only
  `recovery` differs, and the write from v2.0.0's TWRP puts the target's own
  TWRP there. A rooted Dot moves between the two with that one write, from
  Fire OS through `su`.
- Booted Fire OS 5 has no `md5sum`. So the probe and the write read
  `recovery` back through `adb exec-out` and `su`, and hash it on the host.
  On a Dot, 13,953,024 bytes took 2.2 s. One read, at the larger image's
  length, answers for both TWRPs.
- overdub cannot run on `v2`: Fire OS 6 on the Dot has no `app_process`,
  AudioFlinger or OpenSL ES.

### v2

- In v2.0.0's TWRP it runs `twrp wipe cache`, `twrp wipe data`, and `twrp
  install` of the OTA twice, with a reboot into recovery between. Then it
  runs `twrp install` of `boot-root.zip` and reboots.
- It pushes the OTA to `/sdcard` before each install. A `/sdcard` on a
  `/data` that did not mount is TWRP's RAM: the push and its read-back pass,
  and the reboot loses the file.
- The XDA procedure flips the active slot with `bcbtool` between the two
  installs. The run does not: a TWRP install of an A/B OTA writes the
  inactive slot and makes it active, so the flip would make the second
  install write the same slot again. The run reads `bcbtool get_active`
  before and after each install, and stops unless it changed.
- `boot-root.zip` patches both slots. It is an XDA attachment, and XDA sends
  a script a JavaScript challenge in place of the file. So the run waits for
  the user to download it in a browser, before it touches the Dot. It takes
  the file from `~/Downloads` or the cache, and only with sha256
  `de49cc88...`, which other projects pin too.
- Fire OS 6 without root for a minute after the install means
  `boot-root.zip` did not take, and the run stops. A single such read during
  the first boot does not stop it.
- From amonet v1.1.0 the run installs amonet v2.0.0's zip from either v1
  TWRP, the XDA route from v1. The zip restores the stock partition table
  and writes the preloader, LK, TZ, payload and TWRP, then reboots into
  v2.0.0's TWRP. A rooted Dot first goes to recovery.
- The zip writes the preloader first. A stop after that left a valid
  preloader in front of a mixed LK and TEE: no bootrom, and maybe no LK,
  so only the eMMC short reached it. So the run installs a copy whose
  installer writes the preloader last, after the `misc` wipe, and clears
  boot0's header before the install, with `boot0-erased`, as the restore
  does. A stop before the preloader then lands in the bootrom, and the
  resume writes amonet v1.1.0 and installs the zip again. Every other file
  in the copy is the pinned zip's. The copy is rebuilt on every run, in
  about 2 s, so a change to the edit cannot leave an old copy installed.
- The marker goes when the zip prints `- Done`, which it does after the
  preloader. Before that, the zip's own reboot read as a Dot with nothing on
  USB and the marker set, and the run started a resume for a Dot that was
  booting. The check comes before the zip's `(!)` lines, some of which it
  prints and goes on past.
- A TWRP that does not pass the zip's output on would still leave the
  marker, and TWRP 3.2.3 was not tried here. So a resume without an erase
  or `--short` that sees the preloader, `0e8d:2000`, takes boot0 as
  intact: it stops the bootrom step, deletes the marker, and goes on
  polling. On a Dot rebooted from Fire OS 6 with the marker set by hand,
  the run said so 2 s later and ended at v2-booted, with nothing written.
- Measured on a Dot: TWRP 3.7.0_9-bboe2 installs the copy in 19 s, as it
  did the pinned zip, and the Dot came up in v2.0.0's TWRP with no resume.
- Cut on a Dot: a reboot from the Dot itself when the zip logged
  `Updating tz`, after LK and before the preloader. The bootrom appeared 4 s
  later, the same run's resume took it with no one touching the Dot, wrote
  amonet v1.1.0, installed the zip again from bboe2, and went on to v2:
  `[1/8]` to `[13/13]`, 6 min 43 s in all.
  This TWRP has no `/proc/sysrq-trigger` and its `reboot` takes no `-f`, so
  the cut was a plain `reboot`.
- The zip checks `ro.build.product` in `/default.prop` and needs `/sbin/sh`
  and `sgdisk`. Both v1 TWRPs say `biscuit` and ship both.
- A v1 Dot started in fastboot reads as locked-v1-fastboot, and the
  fastbrick leaves it in v2.0.0's TWRP on v1's partition table. The probe
  reads that as amonet-v2-twrp-v1-table, and for `v2` the run installs the
  zip there first. The v2 install itself stops if `boot_a_x` is still there,
  or if `sgdisk` prints no `userdata`.
- In v2.0.0's fastboot the run asks for the + button at power-on, which
  starts v2.0.0's TWRP, and waits for it without limit.
- `--short` goes with any target. Its bootrom step writes amonet v1.1.0, and
  the run goes on from v1's TWRP like any other.
- Measured on a Dot, 2026-10-04, each with no one touching the Dot:

| from | to | took |
|---|---|---|
| rooted-bboe | v1 | 9 s |
| rooted-v1 | v1-bboe | 9 s |
| rooted-bboe | v2 | 5 min 22 s: recovery 30 s, the zip 19 s and its reboot 19 s, the wipes 3 s, the installs 96 s and 117 s, `boot-root.zip` 8 s, Fire OS 6's boot 24 s |
| v2-booted | v1 | 8 min 28 s through the bootrom, which this starting state no longer uses, kept to compare against: recovery 19 s, the downgrade 43 s, TWRP 27 s, the install 86 s, Fire OS 5's first boot 321 s, TWRP 3.2.3 7 s |
| v2-booted | v1-bboe | 7 min 42 s, with no bootrom: recovery 19 s, the table 1 s, recovery 19 s, boot0 under 1 s, the chain 6 s, `misc` under 1 s, userdata 4 s, `/system` 79 s, the boot image 3 s, Magisk 1 s, the preloader under 1 s, Fire OS 5's first boot 321 s |
| stock-booted | stock 8146 | 3 min 11 s: recovery 30 s, `system_a` 81 s, the rest under 7 s each, stock's first boot 32 s |

- `bcbtool get_active` printed a bare `a` or `b`, and each OTA install
  changed it. The downgrade ran on the stock partition table the zip left.
- Not run: the zip from TWRP 3.2.3, the zip on v1's partition table in
  v2.0.0's TWRP, and `--short` to v2.

## Writing amonet v1.1.0 from v2.0.0's TWRP

- The run writes v1.1.0's chain and Fire OS 5.5.5.4 from v2.0.0's TWRP. The
  bootrom step stays for `--short` and for v2's fastboot, where no TWRP is up.
- The fastbrick appears to zero RPMB itself: its payload holds `Zeroing RPMB
  block 0...` and `RPMB block 0 cleared and verified`. If so, a Dot that has
  never seen v1 needs no bootrom either. **This is read from the payload's
  strings, not measured.** Every Dot here already had a zeroed RPMB, and the
  bootrom step it replaces verified the zero where this route assumes it. RPMB
  is not readable from TWRP, so nothing checks it at run time. A wrong
  assumption here is the one failure on this page that costs the eMMC short.
- The table goes first, then a reboot, then boot0's header, then the rest, and
  the preloader last. `BLKRRPART` is `EBUSY` while TWRP is up, so the appended
  partitions need that reboot, and clearing boot0 before it would land there.
- `sgdisk` writes the table, because it writes both GPT copies. Everything
  else goes to a partition node: `dd` answers `EINVAL` past block 4194304, and
  `boot_a` (7199744), `boot_b` (7425024) and the backup GPT (7651295) are all
  above it. `losetup -o` reads 0 bytes there too.
- The chain's writes name `dd`. They need no `conv`, which `/sbin/dd` refuses,
  and a `seek` write to a block device does not truncate: after a root,
  `boot.hdr` at block 0 of `boot_a` and `boot.payload` at 223207 both read back
  whole. `SESSION.dd` is set only on the `stock` path.
- Each target is checked as a block device, at the length its table entry
  gives, before anything is written, boot0 included. `dd` to a name that is not
  one writes a file in RAM `/dev` that reads back and matches.
- The by-name bootloader nodes are decoys on both TWRPs, so the chain names
  `mmcblk0pN`. bboe2 repoints by-name `boot_a` at `boot_a_x` and v2.0.0's TWRP
  does not, so the boot image names `boot<slot>_x`.
- `lk_build_desc` does not say a Dot is unlocked. amonet v2.0.0 patches the
  stock LK and keeps its build string, so a v2 Dot and a locked stock 8146 both
  read `63cb91b-20221007_072309`; `unlock_status` is in fastboot only. The
  `stock` target writes no `recovery`, so a restored Dot keeps the TWRP it had,
  and the probe would read that as amonet-v2-twrp.
- A locked Dot cannot reach that state, so the reading above is a gap in the
  logic and not a route. Measured twice on a Dot restored to stock 8146 with an
  amonet TWRP left in `recovery`, v2.0.0's and 3.7.0_9-bboe2's:
  `fastboot reboot recovery` answers `OKAY` and then nothing appears on USB for
  90 s, in any mode. A locked LK refuses an unsigned recovery.
- `v1_append`, `v1_chain` and `install_fireos6` still hash `lk_a` against
  amonet's `lk.bin`, v2's then v1's, and refuse the rest, so a chain write
  needs proof the Dot is amonet's rather than a build string it shares with
  stock. v1's LK passes, so a part-written chain still resumes. Every stock LK
  is 241664 or 245760 bytes, against 359744 and 372368, so none can match. The
  check is defence in depth, not a fix for a reachable failure.
- The probe waits for `mtp` on every TWRP, not only v1's. adb answers first,
  and the switch to `mtp,adb` re-enumerates USB, so a stage starting in that
  window reads truncated output with exit 255. So each device-side answer this
  route reads ends in a marker of its own, `sgdisk --print` included, because
  adb cuts the tail: a marker at the front leaves a cut line parsing as an
  empty, and therefore passing, result. An md5 needs none, since a cut digest
  is shorter than 32 characters.
- v2.0.0's TWRP costs the install nothing. It returns `adb shell`'s exit status
  and has every tool the install needs, and `/system` took 81 s there, as under
  bboe2: `gunzip` and the eMMC are the limit, not adb.
- `misc`'s boot control block is written, as a read of block 1, 7 bytes patched
  at 0x160, and a write back, which is what v1.1.0's `reset_bcb` does. Leaving
  it inherits whatever ran before: after a `v2` install a Dot read
  `00 41 42 42 01 af 3e`, slot b at prio 14 with 3 tries, a live fallback to
  the Fire OS 6 images a v1 chain cannot boot. The boot image goes to slot a to
  match, not to `ro.boot.slot_suffix`, which here names the slot v2 booted.
- Measured on a Dot: the timings table above has the figures.

## Downgrade: amonet v2.0.0 to v1.1.0

- v1.1.0's `modules/main.py` must start **before** the Dot reboots: it records
  the serial ports that exist, then waits for a new one. The erase runs only
  if main.py is still running 3 seconds after it starts.
- This route now runs only from v2's fastboot, for `--short`. From v2's TWRP
  the targets write the chain in place instead; the section above has it.
- The script stops if `lk_build_desc` is already v1.1.0's: a second erase
  gains nothing. It reads `getvar`, and refuses a Dot whose `unlock_status` is
  not `true`.
- It runs `fastboot erase boot0` and `fastboot reboot`, and the Dot drops into
  its bootrom.
- The script feeds main.py the newlines its prompts read, and is done when
  main.py logs `Reboot to unlocked fastboot`.
- The bootrom is `0e8d:0003`. On macOS it is `/dev/cu.usbmodem*`. On Linux it
  is `/dev/ttyACM*`, owned by `dialout` unless the udev `tty` line gives it to
  `plugdev`, and ModemManager can grab it first. On Windows it is a COM port
  that needs MediaTek's VCOM driver (`cdc-acm.inf`, class Ports), installed by
  hand; without it `0e8d:0003` is an unknown device.
- No other mode needs a hand-installed driver. On a Windows machine that rooted
  a Dot, the driver store held only MediaTek's and Amazon's
  `FireDevicesUsbDeviceClass`, which serves amonet fastboot (`0bb4:0c01`) and
  can come through Windows Update. Windows bound adb (`1949:0112`) and
  v1.1.0's TWRP (`18d1:4ee2`) itself. TWRP 3.7.0_9-bboeN is untried on
  Windows. macOS and Linux need no driver.
- main.py's `serial_ports()` silently skips a port it cannot open, and waits
  forever. So the script gives it 60 seconds after the reboot to log
  `Found port`, then stops it and says why a port may be missing.
- The script runs main.py through `child_bootrom`, with v2.0.0's payload from
  the zip the fastbrick already uses. It writes 64 blocks per command,
  `0x1003`. On macOS and Windows the step takes 18 s from `Found port` to the
  reboot.
- v2.0.0's payload ends a 512-byte block read and a 256-byte RPMB read on a
  full packet, and Windows' VCOM driver waits for a short packet. So
  `child_bootrom` follows each read with a 4-byte read, `0x5000`, and drops
  those 4 bytes.
- v1.1.0's `fastboot-step.sh` ships a Linux-only fastboot. The script runs its
  three commands with the host's fastboot instead: `bin/tz.img` to `tee2`,
  TWRP 3.7.0_9-bboe2 to `recovery` in place of v1.1.0's `bin/twrp.img`, then
  `fastboot oem reboot-recovery`.

## Install: TWRP 3.7.0_9-bboe2

- The image is the `biscuit` build of
  [bboe/twrp_device_amazon_echo-mt8163](https://github.com/bboe/twrp_device_amazon_echo-mt8163),
  on [bboe/android_kernel_amazon_biscuit](https://github.com/bboe/android_kernel_amazon_biscuit).
  GitHub Actions builds and attests both. The script pins the SHA-256.
- bboe2 adds v1.1.0's TWRP 3.2.3 links: `/dev/block/other-boot` and
  `other-system` name the current slot's kernel and system, and `other-lk` is
  `/dev/null`. EchoMuse's installer finds the kernel through `other-boot`, and
  stopped on bboe1 without it. Measured on a Dot: `other-boot` was
  `mmcblk0p10` and `other-system` `mmcblk0p13`, and both followed a slot
  change.
- v1.1.0's TWRP 3.2.3 moved 5.1 MB/s over adb, and 7.0 MB/s with cpu0 at
  `performance`. This one moves 21.5 to 22.2 MB/s with either governor.
- v2.0.0's TWRP cannot replace 3.2.3. Its kernel is 32-bit, and v1.1.0's LK
  starts only a 64-bit one.
- The kernel is Amazon's 5.5.5.4 one with two back-ports for Android 9's
  `init`. Without SELinux ioctl permissions, `init` fails with `avtab: invalid
  type or class`. Without `mmap_rnd_bits`, it fails with `Unable to set
  adequate mmap entropy value!`.
- amonet v1.1.0 keeps its image in `boot_a` and `boot_b`. Fire OS boots from
  `boot_a_x` and `boot_b_x`. This build points `/boot` at `_x`. The script
  does not rely on that. It writes `boot<slot>_x` and `system<slot>` by name.
- `dd` to a by-name path that does not exist writes a file in TWRP's RAM
  `/dev`, and the read-back of that file matches. So each target must be a
  block device.
- A Dot in TWRP 3.2.3 gets this TWRP written to `recovery` and checked by
  md5. Then the script runs `adb reboot recovery`. The script drops 3.2.3's
  `__bionic_open_tzdata...` lines from `adb shell` output.
- adb answers before MTP is on, as `18d1:d001` in this TWRP. The switch to
  `mtp,adb` puts the Dot back on USB under a new ID. In 3.2.3 the switch
  failed a push with `failed to read copy response: EOF`. So the run waits
  for `mtp` in `sys.usb.config`, and each push gets 3 tries.
- This TWRP's `adb shell` returns the exit status. So each device-side step
  is a pushed script, judged by its status. The scripts have `\n` line
  endings.
- The userdata script unmounts `/sdcard` and `/data`. It formats userdata
  with `mke2fs -t ext4 -b 4096 <userdata> <blocks - 256>`, which leaves 1 MiB
  for the crypto footer. Then it mounts it with `mount -t ext4`. The script
  sets the size, block size and type itself, so the result does not depend on
  the TWRP's fstab.
- `/data` is then checked as a mountpoint. Otherwise Magisk's database lands
  in TWRP's RAM and is gone after the reboot.
- This TWRP mounts system at `/system_root`. So the script patches Fire OS's
  system at `/tmp/fireos-system`.
- A failed patch leaves that mount. So the write first unmounts both.
  `/proc/mounts` keeps the name `mount` was given. So the check looks for the
  by-name link and the `mmcblk0p` node.
- Never `umount -a` in TWRP: it unmounts `/proc`.
- Every push is read back by md5 on the device.

## Writing Fire OS

- The OTA zip's installer does 2 writes that matter: `system.new.dat` to
  `other-system` and `boot.img` to `other-boot`. In TWRP 3.2.3 those name
  the current slot's `system` and `boot_x`. Its LK write goes nowhere:
  amonet's TWRP links `other-lk` to `/dev/null`. Its TEE and preloader are byte
  for byte the ones v1.1.0 already wrote to `tee1` and boot0. Its last write,
  `target.blocklist` to `/cache/recovery/last_blocklist`, is skipped: it is a
  file in `/cache`, not a partition the Dot boots from.
- So the zip never goes to the Dot. The host builds the partition image from
  `system.transfer.list` once, into the cache: 805 MB, 386 MB gzipped. It
  takes 22 s on macOS and 31 s on Windows 11, in a background thread, while
  the Dot unlocks and downgrades.
- The built image matched `system_a` after a `twrp install` in every MiB that
  the root's own edits and ext4's mount metadata leave alone.
- It is built as 128 MiB slices, each its own gzip member, because a
  concatenation of members is one stream to any decompressor. So one cache
  serves both routes.
- Where `adb shell` carries a stream, the slices go out back to back on its
  stdin, `gunzip -c` reads them and `dd` writes the partition, which overlaps
  the transfer with the write and needs no room on the Dot. Where it does not,
  each slice is pushed to `/tmp` and unpacked at its own offset: 35 MB at a
  time against a 240 MiB tmpfs, so nothing is staged on the eMMC.
- The route is chosen by sending 1 KiB holding 0x1a and hashing it on the Dot,
  not by naming an operating system. A probe that does not complete takes the
  pieces, which need no stream.
- A Windows adb cannot carry it. Measured on bryce in amonet v2.0.0's TWRP,
  from Windows 11 with adb 36.0.1: `gunzip` read 12 bytes and stopped with
  `gzclose: Illegal seek`, having written 12 bytes, while macOS carried 64 MiB
  through the same recovery minutes apart. `adb push` of the same bytes is
  intact from both, at 21.5 MB/s, so the sync protocol is the one that crosses
  hosts. docs/pitfalls.md has the byte and the counts.
- Nothing reported it. The pipeline returned `dd`'s status, not `gunzip`'s, so
  a failed `gunzip` read as success and only the md5 said anything was wrong.
  On Fire OS 6, which has no `gunzip` at all, the stream exited 0 having
  written nothing. So `gunzip` touches a file when it fails, `dd`'s own status
  ends the pipeline, and the command's last word says `stream-ok`, `slice-ok`
  or `gunzip-failed`. A write is accepted on that word, not on silence.
- TWRP 3.2.3 used `adb exec-in`: 65 s on macOS, 76 s on Windows 11. It is
  binary-safe but returns before the Dot has finished, with no status of its
  own, so a read taken when it returns can differ from what lands.
- On a Dot the gzip reaches `cat > /dev/null` in 17.5 s. Through `gunzip`,
  with the output discarded, it takes 36.4 to 39.3 s. So `gunzip` on the Dot
  is the limit. The whole stage, with the eMMC and the md5 read-back, took
  80 s while it streamed, and about 77 s with TWRP 3.2.3. Slicing costs
  23.4 s against 15.9 s for 256 MiB; the sliced route has not been timed
  across a whole install.
- The partition is then read back by md5: 12 s with 3.2.3.

## The boot image

- The host builds it from the zip's `boot.img` and Magisk 17.3's zip, in
  about 2 s. Nothing runs magiskboot or Magisk's installer.
- 17.3 is the last 17.x. Up to v25.2 support Android 5.1, but from v18 the
  boot patch and `service.d` path change, and overdub needs only `su` and
  `service.d`.
- The ramdisk loses `verify` from every fstab, and `default.prop` gets
  `ro.secure=0`, `ro.debuggable=1` and `persist.sys.usb.config=mtp,adb`.
- The cmdline is the 512-byte header field at offset 64. Stock is
  `bootopt=64S3,32N2,64N2`; the script appends
  ` androidboot.selinux=permissive`.
- Then Magisk's patch, as its `boot_patch.sh` does it with `KEEPVERITY` and
  `KEEPFORCEENCRYPT` false. `init` becomes `magiskinit` and `verity_key`
  goes; both originals go into `.backup`, with `.magisk`. In the kernel,
  `skip_initramfs` becomes `want_initramfs`.
- Magisk's installer also kept a gzipped copy of the image as it found it,
  `/data/stock_boot_<sha1>.img.gz`, and named it in `.backup/.sha1`. Only
  Magisk Manager's image restore reads them, and `stock` is
  the way back to stock here, so neither is written.
- The kernel is a 512-byte MTK header, a gzip stream and the dtb. Only the
  stream and the header's size field change. The ramdisk's cpio is written as
  magiskboot writes one: sorted, inodes from 300000, every mtime 0.
- The stock image ends in a 2,048-byte signature. It is dropped, as magiskboot
  dropped it: the unlocked LK does not check it.
- Compared with the image that `twrp install` of Magisk wrote on a Dot, every
  header field, the kernel, the dtb and every ramdisk file are the same, except
  in 3 places. There is no `.backup/.sha1`. 3 symlinks keep their stock modes,
  where busybox's `cpio` had made them 0777. The MTK header keeps the stock 0xff padding, where
  magiskboot wrote zeros.
- The image is padded to a 4096-byte multiple and written with `dd`.
  After `sync` the page cache is dropped, and the partition is read back by
  md5 over the image's length.
- The host's copies go in a folder under the cache, not in `%TEMP%`. On
  Windows 11, reading a newly written boot image from `%TEMP%` stalled for
  10.3 s in 10 of 15 trials, and in 0 of 23 trials elsewhere.

## /system

- `/system/etc/init.fosflags.sh` strips adb from the USB configuration on
  every boot after the first. The script forces its `FOS_FLAGS_ADB_ON` branch
  true and neutralizes its `unset_adb_persistent_property` calls, then checks
  both edits.
- The 4 Amazon update hosts go into `/system/etc/hosts` as `127.0.0.1`.

## Magisk

- The zip's installer is not run. Its writes go to the Dot as one cpio
  archive, which TWRP's `cpio` unpacks into `/data`. The files are then read
  back as one md5, over all of them in name order.
- `/data/adb/magisk` gets `arm/`, `common/` and `chromeos/` from the zip, mode
  755, and the installer's busybox: base64 of xz, in `update-binary` as
  `BB_ARM`.
- It also gets `magisk`, which `boot_patch.sh` wrote with
  `magiskinit -x magisk`, and which module installs and `--unlock-blocks` run.
  It is the xz stream in `magiskinit` that unpacks to an ELF. Unpacked on the
  host and by `magiskinit -x` on a Dot, its md5 is the same. `/sbin/magisk.bin`
  differs from it in 62 bytes, which magiskinit randomizes at boot.
- On a new userdata the installer wrote to `/data/magisk`, because
  `/data/adb` did not exist yet. Magisk's daemon moved it to `/data/adb/magisk`
  at the first boot. The script writes there directly.
- The rest of the installer changes nothing on this Dot: there is no
  `/system/addon.d`, no `su` in `/system`, and nothing in `/data` to migrate.
- Its database is seeded so the adb shell gets root without a prompt the Dot
  cannot show: `/data/adb/magisk.db`, mode 600, with
  `policies(uid INT, package_name TEXT, policy INT, until INT, logging INT,
  notification INT)` holding `(2000, 'com.android.shell', 2, 0, 1, 0)`.

## The updater

- An update would replace the boot image and remove root. On the first rooted
  run the script runs `pm hide com.amazon.device.software.ota`, then reads
  `hidden=true` back from `dumpsys package`.

## Back to stock: the stock target

`deploy/dot_firmware.py stock <build>` returns a Dot on amonet v1.1.0 or v2.0.0
to stock Fire OS 6, to test a root from a clean start. `stock` is a target like
the others, so every state has a route to it: a rooted Dot goes to recovery,
v1's fastboot gets v1's TWRP, v1's locked fastboot gets the fastbrick, and in
any TWRP the restore runs.

- `stock` takes a `BUILD`, and no other target does. The run downloads the
  build before it touches the Dot.
- The restore ends at stock-booted: the run waits for stock to start, which
  shows as `adb` answering `unauthorized` about 1.5 minutes after the reboot.
  A Dot already stock-booted, or in stock fastboot, has nothing to restore.
  A v2-unlocked Dot booted into Fire OS 6 without root reads the same. So
  the message says "appears", then names the + gesture, which starts
  v2.0.0's TWRP, for a Dot unlocked with amonet v2.0.0. The condition comes
  first, so the owner of a plain stock Dot can skip it.
- The restore clears boot0 first and writes it back last. It creates
  `boot0-erased` before the clear and deletes it once boot0 reads back, as
  the downgrade does. So a restore stopped in between, then rebooted, is
  resumed through amonet v1.1.0's bootrom step and v1's TWRP, and restored
  again. Before the marker, `stock` could not resume that Dot: it needed a
  TWRP, and the Dot showed up only as its bootrom.
- A resume clears the TWRP states from the run's acted-on set, so a run
  that started in TWRP acts on TWRP again after the bootrom step.
- Measured on a Dot, 2026-10-04: a restore stopped during the system write,
  then `adb reboot` from TWRP, then a rerun about 20 s later. The bootrom's
  one window was open when the rerun's bootrom step started, and amonet then
  skipped ports already present. The window closed, and nothing showed on
  USB for 9.5 minutes, until the cable was unplugged and plugged back in.
  The bootrom step then found it, and the run went on through v1's TWRP and
  the whole restore to stock 8146: `[1/19]` to `[19/19]`, 13 min 50 s with
  the wait. The resume now takes a bootrom port already present, and asks
  for the replug only if nothing happens within a minute.
- A restore stopped without a reboot leaves TWRP running, so a rerun restores
  from there.
- The wait for stock to start has the 10-minute limit of any other state.
- A booted Fire OS 5 Dot without root gets `adb reboot recovery`, as the
  `stock` subcommand did. For the other targets that state is a boot still
  in progress, and the run waits.
- v2.0.0's TWRP mounts by name, under `/dev/block/platform/.../by-name/`, so an
  unmount pattern anchored on `/dev/block/mmcblk0` matched nothing there and
  the writes went to mounted filesystems. What is left mounted is named
  back, because a refused `umount` is otherwise silent and the Dot has no
  screen to look at.
- That TWRP carries two `dd` implementations which do not take the same
  operands: the one on the path answers `conv option disabled`. So the script
  uses `toybox dd` where it exists. TWRP 3.7.0_9-bboe1's `toybox dd`
  truncates at its `seek` offset, and the block device answers `ftruncate:
  Invalid argument`. So the one write with `seek` passes `conv=notrunc` to
  `toybox dd`.
- Measured on a Dot, 2026-10-04, before `stock` was a target: `stock 8146`
  from rooted-v1, through v1.1.0's TWRP 3.2.3, in 6 min 9 s; from
  v2-booted, through v2.0.0's TWRP, in 2 min 59 s; from rooted-bboe in
  1 min 52 s. Each was rooted again after the fastboot gesture: to `v2` in
  7 steps, `v1` in 10, `v1-bboe` in 9, as the table counts.
- As the target: from rooted-bboe in 3 min 11 s, `[1/17]` to `[17/17]`,
  ending when stock started, and 4315 in 4 min 9 s, which rooted back in
  9 min 8 s. Run again on the stock Dot, it reported nothing to restore
  and downloaded nothing.
- `sgdisk`, `mke2fs`, `blockdev` and `md5sum` are looked for before the
  countdown, because `sgdisk` and `mke2fs` are not reached until after 1.6 GB
  has gone in.
- 4315 ships a block OTA in place of a `payload.bin`: `system.new.dat`
  with a transfer list, and boot, LK, TZ and the preloader as files in the
  zip. `system.img` is built from the list, which must be version 3 or 4
  and use only `erase`, `zero` and `new`, since the rest patch blocks this
  does not have. Its ranges must cover the image from 0, and no block may
  be written twice: Fire OS 5's own list erases the whole image and then
  writes over it, where 4315's erases only what it does not write. Only
  the `new` ranges are written, because the file is truncated to size
  first, and `system.new.dat` must end where the last of them does. A block OTA
  carries no hash per image, so they rest on the OTA's pinned SHA-256.
- LK, TZ and boot are padded to a multiple of 4096 bytes. A write is
  checked over whole sectors, `n // 512` of them, and 4315's LK is 241,448
  bytes, so the check left the tail out and failed on a write that had
  gone in. A payload build's images are already whole blocks.
- 4315's package number, 8087722874, is not the one the system it installs
  reports, 8087722884. The table's is the package's, which is what names
  the download.
- FTVDB keys an OTA by its md5, so the download is found by md5 and then
  pinned by the SHA-256 of a copy that matched it.
- Each `payload.bin` operation is read where it lies, so peak memory is one
  image plus one operation: about 1 GB, for the 768 MB system image.
- A passed check prints `ok` rather than the tick where the console cannot
  encode it, such as a legacy Windows code page, which would otherwise raise
  `UnicodeEncodeError`.

### The partition table

- amonet v1.1.0 renames the stock `boot_a` and `boot_b` to `boot_a_x` and
  `boot_b_x`, and adds its own `boot_a` and `boot_b` as partitions 17 and 18,
  cut from the end of userdata. The stock table is rebuilt from the Dot's own
  rather than from constants, so every offset and size is that Dot's.
- The backup table goes in before the primary: a run cut during the primary's
  write leaves it mixed, and the backup is what the next run reads instead.
- A table with no `_x` name is already stock and is kept: that is what makes a
  stopped run safe to repeat, and why v2.0.0's table needs no surgery.
- The kernel must reread the table before cache and userdata are formatted.
  Against the old one, `mke2fs` would size userdata to amonet's cut.
- The first run on a Dot saves that Dot's own table as
  `current-gpt-<serial>.bin`, and later runs keep it rather than saving the
  stock table over it.

### The writes

- boot0's header is cleared first. With no valid preloader the bootrom waits
  for a USB host instead of running anything, so a failure anywhere after it
  leaves a Dot that amonet's bootrom step can reach without opening the case.
  Forced on hardware: the bootrom appeared 4 seconds after the reboot and
  waited about 39 seconds, and amonet v2.0.0's bootrom step rewrote the whole
  chain in 22. The order of the bootchain writes is not what makes this
  survivable -- with the header gone, no slot is reachable anyway -- the
  bootrom is.
- `misc` gets zeros and, at 0x360, the boot control block amonet v1.1.0's
  bootrom step writes: `00 41 42 42 01 8f 00`. Slot a is then `prio 15 tries 0
  success 1`, and slot b is unused. Each slot byte packs `priority:4`,
  `tries:3` and `success:1`, low bits first.
- A zeroed `misc` is not safe. Stock's first boot starts slot a with a few
  tries and `success 0`, and each boot cut short spends one. On a Dot, a
  gesture during the first boot and the power-ons after it left slot a at
  `tries 0 success 0`. The preloader then reset about every 30 s with the ring
  dark, never starting LK, so neither the gesture nor `FACTFACT` reached
  fastboot. It did not fall back to slot b, whose tries stayed at 3. It took
  the eMMC short to get back. A slot marked good spends no tries.
- `tee1` and `tee2` are not slot-tied: every slot-tied partition takes a letter
  from `ro.boot.slot_suffix` and TEE takes digits, and amonet's bootrom step
  writes its LK to both slots unconditionally, but its TEE payload only to
  `tee1`. So they go backup first and
  primary last, rather than by slot.
- boot is padded with zeros to the size of `boot_a`, which both slots share, so
  no byte of amonet's boot image survives and the read-back covers the whole
  partition.
- TWRP answers `ro.product.device` only because amonet's TWRP sets it in its
  `default.prop`; a recovery ramdisk has no reason to carry it otherwise.
- A push and its read-back address different block devices, whose page caches
  are not coherent, so the cache drop between them is what makes the comparison
  mean anything.
- `adb push` to a node that does not exist does not fail: adbd creates a
  regular file in TWRP's RAM-backed `/dev` and reports success. A partition's
  read-back still catches that, because it reads the raw disk at the computed
  offset rather than the node -- but only after 768 MB has gone into a tmpfs on
  a 512 MB device. So the start sector is checked first, which also catches a
  number that means a different partition in the table the kernel still holds.
- boot0 is the one write whose read-back names the node it wrote, so there a
  regular file would match itself and pass. Hence the check that it is a block
  device. For the same reason the cleared header is counted twice, bytes read
  and bytes left non-zero: a read that returned nothing would otherwise look
  like a header that cleared.
- The skip compares a mebibyte of head first, which costs 0.07 s and is enough
  because a different build differs within a kibibyte or two. Only a partition
  that looks right pays for the full comparison, 13 s against the 80 s a write
  would take.
- A restored Dot has no Wi-Fi until it is set up in the Alexa app. To root it
  again, skip that setup: on Wi-Fi it can update to a build
  `dot_firmware.py` has not met, or away from the build under test.
