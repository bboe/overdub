# Network adb, and the microphone

What Home Assistant can switch on the Dot itself. What it reads -- the signal,
the temperature, the memory, the jack, the speaker, the volume -- is in
docs/api.md, with the polls that drive it.

## Network ADB

`internal/device/adb.go`: a select for adbd on tcp/5555, so a Dot on a shelf
can be worked on without a cable.

### The positions

| position | tcp/5555 | `ro.adb.secure` | who can connect | offered |
|---|---|---|---|---|
| Off | closed, no rule | deleted | nobody | always |
| Insecure | open | deleted | anyone who routes to the Dot | always |
| Secure | open | 1 | a client holding the key | with a key |

- The rule changes only after the properties take. A step that fails earlier
  leaves the chain alone, so the device stays where it was and the reading
  reports that truthfully.
- init freezes `ro.adb.secure`: `setprop` cannot move it and Magisk's
  `resetprop` can. That is the one step here that needs Magisk rather than root.
- The key is `/data/local/bin/adb_keys`. `setADBLocked` refuses Secure without
  it, in case a peer sends it anyway. Without a key,
  Secure gives a listening adbd that authenticates nobody.
- The icon is `mdi:console-network`; `mdi:android-debug-bridge` does not exist.
  An unknown icon name fails nowhere: the frontend draws no icon. No test here
  can check an icon name.

### Reading it back

- adbd restarts through a property, so nothing returns a result. The worker
  waits `adbSettleFor`, then reads the mode.
- A read that fails is not an answer. Taken as "not secure", it would publish
  Insecure for a Dot in Secure and close adb on a Dot already in place.
- Secure fails closed: a Dot that reads Insecure after a Secure request is set
  Off. The property did not take while adbd came up anyway, so the Dot is open
  to the subnet with no authentication.
- A mode that cannot be read is not published. The rule check waits on the
  iptables lock that netd holds constantly, so it fails on a working device. A
  select has no `missing_state`.
- `CurrentADBMode` runs on the 60-second sensor tick. A closed port costs one
  read of `/proc/net/tcp`. An open one costs 2 forks, one of them an
  `iptables -C` waiting on netd's lock.

### The rule, and who re-asserts it

- netd rebuilds the INPUT chain and discards the rule. The sensor poll
  re-asserts it through `HoldADBOpen`, only while adbd listens, so a Dot with
  adb off runs no iptables.
- The gate is `ADBListening`, not `CurrentADBMode`. `CurrentADBMode` includes
  the rule, so after netd wipes it the Dot reads Off while adbd still listens.
- Nothing remembers the position across a daemon restart. The supervisor
  respawns on any fatal exit while adbd may still listen, so the device is
  asked instead.
- The poll does not re-assert while an apply is in progress. `ctl.restart` is
  asynchronous, so the old adbd is still up when `SetADBMode` returns. A
  re-assert then would restore the rule the close removed, and nothing removes
  it again: the mode reads Off, a repeated Off is dropped, and `uninstall.py`
  leaves this port alone.
- So a close that reads Off after the settle deletes the rule a second time,
  through `DenyADB`. `AllowTCP` can wait up to 10 seconds on netd's lock, longer
  than the settle, so a re-assert released at the end of an apply can still
  land after the close.
- `HoldADBOpen` and `SetADBMode` share `adbMu`, so the listening check and the
  `AllowTCP` are one step. It is not the server lock, because this waits on
  iptables, which waits on netd.

### Commands

- Home Assistant can move a dropdown faster than adbd restarts. The command
  hands the mode to a worker and returns, because `handle` holds the server lock
  and `SetADBMode` takes seconds.
- The worker keeps one pending mode, not a queue: three positions in quick
  succession restart adbd once, on the last.
- A command for the position the last apply reached is dropped. Each restart
  drops every live adb session, and repeats are not bounded by the 8 slots.
- The guard compares against the last apply, not the device: reading the device
  forks, and this runs under the server lock. The published state trails an
  apply by the settle, an iptables pair and the tick's reads.
- The apply must have succeeded. The properties can land while the rule does
  not, and the device then reads Off; without the condition every later Off
  would be dropped.
- A poll reading that differs from the guard clears it, because adbd can stop
  for reasons nothing here asked for.
- The worker wakes the poll instead of publishing. Two readers can read either
  side of a change and publish out of order.
- Uninstall does not reset the position. It lives in the property store and the
  INPUT chain, and deleting the rule could cut the connection the uninstall runs
  over.

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
