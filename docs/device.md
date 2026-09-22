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
  it again: the mode reads Off, a repeated Off is dropped, and `uninstall.sh`
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

- The state is AudioFlinger's `mMicMute`. The mute key and "Alexa, mute" both
  reach it through `AudioManager.setMicrophoneMute`.
- No dumpsys prints it. `dumpsys audio`'s `Mute count` is the per-stream output
  mute, 0 whatever the microphone does. The read is `GET_MIC_MUTE` on
  `IAudioFlinger`, called by number; docs/hardware.md has the transaction.
- The reply is one int32, `0` or `1`. Anything else is no reading, and so is a
  failed call even when its output parses, because a child killed at its
  deadline can return bytes. No reading publishes nothing: a switch has no
  `missing_state`, and "not muted" would claim the Dot is listening.
- Setting it presses the mute key through the clone with `Interceptor.Press`,
  down then up. The switch does what the button does, ring included, and Alexa
  stays the only writer of her own state. A direct write to `mMicMute` would
  leave her believing the microphone is live, and she would overwrite it.
- The down mutes; the up only releases. A lost up leaves Android holding the
  key, which runs a reset gesture unattended (docs/hardware.md). `Emit` writes
  the key and its SYN under one error, so a failure cannot say which half
  landed. Both halves fail as `button.ErrKeyStuck`, and serve.go exits on it so
  the clone goes with the process.
- The key toggles, so the worker reads before it presses and presses only when
  the state differs. An unreadable device is not pressed, and a press that did
  not take is not repeated. The settle is 250 ms, which is slack: the mute shows
  in the first read after the press.
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
  costs 12 ms; the volume's costs 11.7 ms.
- Unmuting needs only the API key, so a misfiring automation can turn the
  microphone on. With `mute_button_mode` at `intercept`, the physical button
  does not mute but this switch still does: the mode governs keys from the real
  node, and this press does not come from it.
- No `entity_category`. The selects are `config` because they change what the
  daemon does with a key; this is the microphone, and a categorised entity is
  filed away from the controls.
