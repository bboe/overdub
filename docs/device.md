# Network adb, and the microphone

What Home Assistant can switch on the Dot itself. What it reads -- the signal,
the temperature, the memory, the jack, the speaker, the volume -- is in
docs/api.md, with the polls that drive it.

## Network adb, and its keys

`internal/device/adb.go`: adb over tcp/5555, so a Dot on a shelf can be worked
on without a cable.

- firebreak's Fire OS 6 unlock leaves adb always on, and from v1.2.0 always
  authenticated.
  `/init.mt8163.usb.rc` sets `service.adb.tcp.port 5555` at every boot,
  `/system/bin/firewall.sh` opens tcp/5555 while `persist.sys.usb.config`
  holds `adb`, and the system partition carries `ro.adb.secure=1` and, in
  `/adb_keys`, the public key of the computer that ran firebreak.
- So there is no switch for it. Fire OS 5's select had Off, Insecure and
  Secure, through Magisk's `resetprop` on `ro.adb.secure`. Fire OS 6 has no
  `resetprop`, the boot image fixes the property, and Off would take away the
  one way into a Dot with no cable.
- netd rebuilds the INPUT chain and can discard the rule. The sensor poll
  re-asserts it through `HoldADBOpen` while adbd listens, so a Dot whose adb
  is down runs no iptables. The stock rule has no interface; the daemon's is
  `-i wlan0`, so the two do not collide.

### Keys from Home Assistant

- An action, `set_adb_keys`, with one string, `keys`: one or more adb public
  keys, one per line, as `adbkey.pub` holds them. Home Assistant offers it as
  `esphome.<name>_set_adb_keys`; its Developer Tools action form gives a text
  box, so no YAML is needed.
- It replaces `/data/misc/adb/adb_keys` whole. A key left out stops working.
  `none` removes them all. An empty `keys` is refused, not taken as none: a
  form run before the paste, or a template that renders empty, would
  otherwise revoke every machine added. adbd reads the file at each connection, so
  no restart is needed. `/adb_keys` keeps working whatever is sent, so no call
  locks out the computer that rooted the Dot, and none can revoke it.
- Not a text entity: Home Assistant caps a state at 255 characters, and one
  key is about 700.
- Each line must be the 524-byte RSA key adb writes, base64, then an optional
  name after any whitespace. One line that is not, or more than 16 keys,
  refuses the whole call and touches nothing. The refusal names the line as
  pasted, blank lines counted.
- An action returns nothing to Home Assistant, so a diagnostic text sensor,
  `adb_keys`, shows who can connect: the names in `/adb_keys`, then the ones
  set from Home Assistant, separated by commas, as
  `you@mac 🔒, a@b, c@d`. 🔒 marks a key from `/adb_keys`, the rooting
  computer's: no call removes it. With no key anywhere it reads
  `none`. A refused call puts `refused: <why>; ` in front until a call
  succeeds. It is cut to 255 characters, Home
  Assistant's limit for a state.
- The file is `0640` `system:shell` in a `02750` `system:shell` directory, as
  init makes it, set explicitly because the daemon's umask is 077: a bare
  write landed `0600`. It is written to `adb_keys.new`, synced, and renamed
  over, then the directory is synced, so neither a reader nor a power cut
  sees half of it. A failed write removes the `.new`.
- The write runs off the server lock, under a lock of its own, and wakes the
  sensor poll when it is done. Each call is numbered, and a call that a later
  one has overtaken does nothing. Two calls close together could otherwise
  write in either order, and a "none" sent to revoke keys could lose to the
  keys it revoked.
- Key names are read as UTF-8, a bad byte becomes `?`, and the cut backs off
  to a whole character. Home Assistant refuses a state that is not UTF-8.
- The API key is enough to install a key, and an adb key is a root shell. The
  API key can already install a firmware update, so this asks no more trust of
  it than that does.
- `install.py` pushes no key. It removes `/data/local/bin/adb_keys`, where
  an earlier install put one, and the copy in `/data/misc/adb/adb_keys` when
  the md5s match: that copy still opened adb. It warns when `ro.adb.secure`
  is not 1. `uninstall.py` removes the keys set from Home Assistant;
  `/adb_keys` still opens adb.

### A Dot rooted before the lock

- firebreak locks adb from v1.2.0. A Dot rooted earlier has
  `ro.adb.secure=0` in `/default.prop` and no `/adb_keys`, so any host on its
  network gets a root shell, and keys set from Home Assistant are not
  consulted. A reboot does not change it: the file is on the system
  partition. Seen on main-bedroom, a radar on 8149.
- `install.py` asks whether `getprop ro.adb.secure` is 1 with a word of its
  own, so a failed `adb shell` or a stray stderr line does not raise the
  warning.
- The fix is firebreak's lock, done on the running Dot to both slots:
  `ro.adb.secure=1` in `default.prop`, and the rooting computer's
  `adbkey.pub` as `/adb_keys`, `0644`, labelled `adb_keys_file`. It takes
  effect at the next boot. docs/usage.md has the commands.
- firebreak writes these from TWRP, where SELinux is permissive. On the
  running Dot it enforces, and `/` is labelled `rootfs`, so every file
  created there is refused: `avc: denied { associate } ... scontext=
  u:object_r:rootfs:s0 tcontext=u:object_r:labeledfs:s0`. `sed -i` fails
  too, because it writes a temporary file. Setting `fscreate` from the shell
  did not take.
- So `default.prop` is rewritten in place, `cat new > /default.prop`, which
  creates nothing. The key is created in `/system`, on the same partition,
  labelled with `chcon`, and moved to `/adb_keys`: a rename makes no new
  inode. The other slot, mounted read-write under `/data/local/tmp`, carries
  the same labels and takes the same steps.
- With `ro.adb.secure=1` and no key adbd can read, adb is shut after the
  reboot, and the way back is firebreak's gesture into TWRP. So both slots
  are read back first: the property, the key byte for byte, the mode and the
  label.

## A setting that survives a reboot

- `Flag`/`SetFlag` and `Number`/`SetNumber` keep a value in
  `persist.overdub.<name>`. The property service writes `persist.*` to
  `/data/property` itself, so a plain `setprop` survives a reboot without
  Magisk.
- The file is `/data/property/persist.overdub.<name>`, `-rw-------` root:root.
  A flag written `0` survives a cold reboot.
- A value never written reads as unknown, not off, so the caller picks the
  default. Both setters read the value back and report a write that did not
  take.
- A property write costs about 40 ms: 50 `setprop`/`getprop` pairs take 2
  seconds, and 50 reads alone take well under 1. Do not write from a goroutine
  with other work, or once per slider step. docs/sendspin.md has the window the
  output delay writes in.
- The device refuses a property name longer than 31 characters: `setprop`
  prints `could not set property` and writes nothing. The prefix takes 16, so a
  name has 15. `key` checks the length before the name reaches the device.
- `setprop` then `getprop` in one shell command reads back empty, even for an
  accepted name. Put a `sleep` between them when measuring, or every length
  looks refused.
- The setters' read-back is a separate `execve` and reads correctly on the
  device. If it ever loses that race, the cost is a log line for a write that
  landed.

## Whether the microphone is muted

`internal/device/mic.go`: a switch, because Home Assistant both reads the
microphone and mutes it.

- The state is the privacy driver's own: its `state` file, 1 with the
  microphones off and 0 with them on. A sysfs read, no fork.
- `acebuttond` publishes the same as LIPC `com.doppler.buttond muteState`, 8
  on and 9 off (`8-enable mic, 9-disable mic`), a 70 ms fork. It lags the
  driver after a write, and can miss one entirely, so it is read only to
  learn that `acebuttond` has caught up, never as the state.
- `acebuttond` takes it from the privacy driver, not the key:
  `/sys/devices/soc/10010000.keypad/amz_privacy` on biscuit and radar, the
  `gpio-privacy` device on donut. An injected 113 left it unchanged on both
  layouts. At its start it restores the state it saved, `write gpio privacy to
  1` on main-bedroom, whose microphone was off.
- Fire OS 5 read AudioFlinger's `mMicMute` through `GET_MIC_MUTE`, by number.
  Fire OS 6 has no AudioFlinger, so every read failed and Home Assistant drew
  the switch as unknown.
- `state` is 0 or 1. Anything else, or no driver, is no reading. No reading publishes nothing: a switch has no
  `missing_state`, and "not muted" would claim the Dot is listening.
- **Muting is one way.** Writing `1` to the driver's `enable` latches the
  microphones off and lights the button's red light; nothing in userspace
  leaves privacy. On overdub-test, `0` to `enable` and `1` to `privacy_trigger`
  each left it latched, and a press cleared it every time. LibreEcho's port of
  the driver says why: privacy_trigger "deliberately refuses to let userspace
  leave privacy, so that no program can silently un-mute a microphone".
- So an unmute from Home Assistant is refused and logged, and the switch
  springs back. Only the button turns the microphones on.
- The driver is `/sys/devices/soc/10010000.keypad/amz_privacy/` on biscuit and
  radar and `/sys/devices/platform/gpio-privacy/` on donut, each with
  `enable` and `state`. The first that exists is used.
- `acebuttond` reads the driver only at its start and on a press, so after the
  write the ring stays dark and `muteState` still reads 8. A press is enough:
  on a 113 it reads the driver, not the key, then plays the mic-off tone,
  draws the ring and sets 9, as for the button. A key that fails falls
  through to the restart below: the driver is latched either way.
- So the daemon presses 113 through the clone where the clone carries it,
  biscuit and radar. On donut the driver sends its own 113 on the
  `gpio-privacy` node, which `acebuttond` reads. The mute is done when
  `muteState` reads 9, within 2 seconds.
- Failing that, `acebuttond` is restarted, and its start finds
  `currMuteState(disabled)`; then 15 seconds. 5 were too few: init kills it
  with SIGKILL, and a new one that starts before D-Bus has let go of the old
  one's name exits with `initFailed: reason=lipcOpenFailed`, and init starts
  it again 5 seconds later. On donut that put 9 at 5.5 seconds.
- The restart is the fallback, not the way, because it loses the tone. Killed
  as Alexa starts the mic-off tone, `acebuttond` takes its power registration
  with it; the tone's player logged `createRawPowerResourceFailed`, and the
  tone went into the mixer and out of no speaker. With the key, `acebuttond`
  kept its pid and the tone was heard.
- donut's driver sends that 113 on the watched mute node; biscuit's sends
  none. For 1.5 seconds after the daemon's write, a 113 on the watched node
  is that echo and is not reported: an automation on the mute button would
  otherwise fire on a mute from Home Assistant. It came 0.15 to 0.66 seconds
  after the write. The window is the write's, not the whole mute's, so a real
  press during a restart's 15 seconds is still reported. The echo is decided
  at the key-down and held to its key-up.
- The daemon's own press through the clone never reaches the read loop, so
  it is not reported, and biscuit's real presses are never filtered.
- Fire OS 5 pressed the mute key through the clone, and Android's mute
  followed it both ways. On Fire OS 6 an injected 113 reaches `acebuttond`
  but never the driver, which takes only the button's interrupt.
- The worker reads before it sets and sets only when the state differs. An
  unreadable device is not set, and a set that did not take is not repeated.
- A command is dropped only when the last apply reached the same state. The
  published state trails an apply by the settle and a poll turn, so a different
  command in that window is a change of mind. A poll reading that disagrees
  clears the guard.
- The worker wakes the live poll. `PollLive` ignores a wake within 1 second of
  its last read, and the sound sample reads every 500 ms, so the state usually
  follows on the next heavy tick. Command to state measures 1.65-2.13 seconds,
  which Home Assistant draws as a toggle that springs back and flips again.
- It reads on the heavy tick beside the volume, under the volume's rule in
  docs/api.md: somebody mutes the Dot, then looks to see whether it took. A read
  is one sysfs file.
- Muting needs only the API key; unmuting needs a hand on the Dot.
- No `entity_category`. The selects are `config` because they change what the
  daemon does with a key; this is the microphone, and a categorised entity is
  filed away from the controls.
