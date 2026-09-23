# The Home Assistant API

## ESPHome emulation

`internal/esphome`. Home Assistant is the *client* and dials tcp/6053. We answer
the handshake, list entities and push state.

- The advertised API version is 1.12. Later versions gate entity kinds and
  capability requests this daemon does not have. Home Assistant enforces no
  floor.
- Only a frame that decrypts buys the longer read deadline. A peer without the
  key holds a slot for 10 seconds at most.
- Finishing the handshake does not prove the key. Message 1 carries nothing
  fresh from this side, so a captured one replays and Noise accepts it.
- The 10 seconds are one budget for the whole handshake, not one per read.
  Otherwise a peer that stalls each read gets a second full wait.
- A connection counts against the 8 slots from the moment it opens, because an
  uncounted socket is unbounded. The key gates what a peer can reach, never
  whether it holds a slot. 8 silent peers fill the table, and nothing evicts
  the oldest. SECURITY.md says the same: the port is bounded, not guarded.
- A message that does not parse is not acted on. `pbWalk` visits the fields it
  read before it failed, so acting would use fields scraped from a message we
  could not understand. This covers `HelloRequest` and every command.

### Naming an entity

- **An entity is named 3 times, and they are not interchangeable.** Each listing
  carries an `object_id`, a `key` and a name. Home Assistant builds the entity
  id from the **name** (`_attr_has_entity_name`). The `key` routes every state
  message and must never move for an existing entity. The `object_id` is part
  of the older unique id form Home Assistant stores, so changing it can
  re-identify an entity. It stays the slug of the name.

### Logging

- Every line a peer can cause goes through `internal/untrustedlog`.
- 7 lines are written outside the count: the listener's startup line, the 2
  saying a tick was raised to its floor, the 2 saying the entity list was
  dropped so it is sent again, and the package's suppressed-count and ceiling
  lines. Only the first 3 are beyond a peer's reach. All 7 are bounded.
- No log write happens with the server lock held. The lock gates the accept
  path's cap check and every other connection's handler, so a write to `/data`
  under it stalls the server.

### The idle deadline and the ping

- `aioesphomeapi` pings on a 20-second timer that starts when it connects and
  never moves. *Any* message from the device clears the pending ping. So a
  device that talks often is never pinged, and the silence before a ping is 20
  to 40 seconds.
- A deadline that waits for the client's ping therefore expires on a healthy
  connection. With a 5-second push cadence, Home Assistant dropped at exactly
  90 seconds, repeatedly.
- So the deadline is ours. 60 seconds of silence draws a `PingRequest` from this
  side. The answer resets the deadline. 90 more seconds without one drops the
  connection.
- Those are ESPHome's numbers: its firmware pings at `KEEPALIVE_TIMEOUT_MS`,
  60 seconds, and gives up at 2.5 times that.
- TCP retransmits, so the margin buys a slow answer, not a lost one.
  `tcp_retries2` is 15 here, so the kernel spends about 15 minutes on an
  unacknowledged write. The 150-second budget sits inside that, so the read
  deadline ends these connections, never the socket.
- The ping takes over only a read that expired with nothing taken off the
  socket. `io.ReadFull` drops a partial read, so reading again after a
  part-frame would take the rest as a fresh header. `readNoiseFrame` marks
  every error after the header is read, and the loop drops the connection on
  those.
- A connection that has decrypted nothing keeps its 10 seconds and is not
  pinged. ESPHome draws the same line: it updates `last_traffic_` only after
  authentication.
- `MinSensorTick` is the floor on `PollSensors` to bound traffic for slow
  readings. It does not keep the connection alive.

## Which tick a reading rides

Chosen by what the reading costs and whether anybody would look for it sooner.

| reading | source | cost | tick |
|---|---|---|---|
| uptime | `/proc/uptime` | 67 µs | minute |
| signal | `/proc/net/wireless` | 1.8 ms | minute |
| registration | `dumpsys account` | 10.5 ms | minute, subscribed |
| Bluetooth name | `dumpsys bluetooth_manager` | 11.5 ms | minute, subscribed |
| button modes | held in memory | -- | minute |
| adb mode | `/proc/net/tcp`, `iptables -C` | 2 forks if open | minute |
| temperature | a thermal zone's `temp` | 118 µs | heavy |
| memory | `/proc/meminfo` | 111 µs | heavy |
| volumes, route | `dumpsys audio` | 11.7 ms | heavy |
| jack | `/sys/class/switch/h2w/state` | -- | heavy |
| microphone mute | binder `GET_MIC_MUTE` | 12 ms | heavy |
| speaker playing | ALSA, then a `dumpsys` | 2.0 / 18.7 ms | live |

- The minute tick is `sensorTick`, 60 seconds. `PollLive` wakes every
  `liveTick`, 500 ms, and reads the sound every `SoundEvery` tick and the rest
  of `readLive` every `HeavyEvery` tick, 2.5 seconds. Two numbers let a cheap
  reading run oftener without dragging a fork along.
- The uptime changes on every read. The signal costs 16 times `/proc/meminfo`
  and moves slowly. The registration changes only at setup or deregistration.
- The heavy tick costs about 24 ms of a core every 2.5 seconds, 1%. Almost all
  of it is the 2 forks for the volume and the microphone.
- `dumpsys account` and `dumpsys audio` were timed in the same loop, because
  the absolute figure moves with device load: 300 calls took 4.66 and 5.23
  seconds.
- `dumpsys bluetooth_manager` costs what `dumpsys audio` does: 100 calls took
  1.43 to 1.54 seconds against 1.30 to 1.47.
- Zero is a valid reading for the signal, the uptime and the volume. So a
  reading that could not be taken is never published as zero.

## The polls

- `Poll` starts both polls. Inside the package, a test holds every key
  `listEntities` sends to arrive as a state once `Poll` runs. An entity with no
  poll shows in Home Assistant with no value ever.
- Every reading shares one published state. A new client is answered from it,
  never from a reading of its own.
- A second reader breaks that. A snapshot reading the device can tell one client
  a value the poller never saw; the poller then finds the old value and stays
  quiet. A volume changed and changed back inside one tick, with a client
  subscribing between, reproduces it on the Dot.
- `PollLive` raises a tick that is not positive to 1 second: `time.NewTicker`
  panics on it.
- `dumpsys` talks to binder, which can wedge, so the read carries a deadline.
  `exec.CommandContext` kills the child, but `Output` then waits for EOF, which
  a grandchild can hold open. `cmd.WaitDelay` bounds that wait. So a read is
  1 second plus 500 ms, exported as `VolumeReadBudget`.
- The poll is serial, so a heavy tick spends its forks in sequence. 2 came to
  2.4 seconds against 2.5. `main_test.go` holds the sum of the volume, mic and
  speaker budgets, not each apart.
- The budget and the command are variables so a test can shrink the wait and
  substitute a command that never answers.
- `PollLive` reads nothing while nobody is subscribed, because it forks.
- Subscribing wakes the polls, and only on a connection's first request when
  nobody else is subscribed. A peer that keeps becoming the idle case is bounded
  by `wakeGap`, 1 second between wake-caused reads.
- The snapshot is sent from `handle`, under the lock that orders it against a
  push, and reads nothing. `publish` logs its failures after the lock is
  dropped.
- A wake may be dropped but never blocks. A subscribe and a button mode change
  send theirs with the server lock held, and a blocking send there deadlocks:
  the poll that drains the channel takes the same lock to publish.
- Both polls hold a wake to `wakeGap`, because a peer causes most wakes and a
  route can flap.
  `PollLive` drops an early one: its own tick comes in 500 ms. `PollSensors`
  waits out the rest and then serves it, or the value stays wrong until the
  minute tick.
- The polls are the only device readers. A wedged poll leaves every subscriber
  on stale values, so the volume read carries a deadline and procfs reads do
  not.
- A failed reading is sent with `missing_state` set. Leaving it out is right
  only before the first reading: after that Home Assistant keeps drawing the
  last value. Home Assistant's client shows `state=0.0 missing_state=True`,
  drawn as no value.
- `Listen` retries a failed `Accept`. Returning would leave the socket bound
  with nobody accepting, and Home Assistant would hang on a completed
  connection.
- Each connection has its own queue of 64 frames and a writer goroutine, so the
  server lock is never held across a socket write. One stalled subscriber would
  otherwise park everything for the 10-second write deadline.
- A client that overruns its queue is dropped, as ESPHome does. The connection
  cap bounds the memory, on a device with 472 MiB usable.

## The readings

### Wifi signal

- The fourth field of the `wlan0` line in `/proc/net/wireless`.
- The level is an unsigned byte (`struct iw_quality.level` is `__u8`), so
  -48 dBm arrives as 208. Values above 127 are taken off 256.
- A column carries a trailing dot when its `IW_QUAL_*_UPDATED` flag is set.
  Reading the file clears the flags, so a read once a minute almost always
  sees the dot.
- The kernel prints a zero row for a wireless netdev with no statistics, as
  `p2p0` does. So a level at or above 0, or at or below -120 dBm, is no
  reading. That also covers the -256 an `IW_QUAL_DBM` driver writes.
- On a cold boot there is no row until 23 seconds, a zero row from 23 to 25,
  and a real level from 27. The API is up for none of it: `serveAPI` waits for
  the MAC.
- With the radio down (`svc wifi disable`) the row disappears rather than going
  to zeros. `ifconfig wlan0 down` does not test this: the framework restores
  the interface at once.
- The line matches the whole interface name, so another netdev whose name
  contains `wlan0` is not read.

### Temperature and memory

- The CPU temperature is in millidegrees under `/sys/class/thermal`: `41300` is
  41.3 degrees.
- The zone is found once by its `type`, then read by path. The search costs
  4.6 ms against the 118 µs read: it opens the `type` of each of 11 zones in a
  directory that also holds 54 cooling devices. A path that stops working is
  forgotten.
- The zone index is not stable, and `thermal_zone10` sorts before
  `thermal_zone2`. `mtktscpu` is `thermal_zone1` and reads 41.3 degrees.
  `tmp103` is a discrete board sensor that reads 5 degrees cooler. The SoC is
  reported, because throttling is decided on its die.
- A zone with nothing to report answers `-127000`. So only readings between -40
  and 150 degrees are kept. Zero passes.
- The memory is `MemAvailable` from `/proc/meminfo`, in MiB. `MemFree` is the
  wrong figure: 35 MiB free of 472, beside 123 MiB of reclaimable cache, with
  `MemAvailable` at 126.
- A line without `kB` is no reading. The field is absent before Linux 3.14;
  there it is no reading, not a figure rebuilt from `MemFree` and `Cached`.
- The kernel writes `kB` and means KiB, so the reading is divided by 1024.

### The jack

- `/sys/class/switch/h2w/state` detects a plug, not a device. The driver
  (`accdet_amzn`) reads 1 for a 3-pole cable with no microphone pole, the same
  cable with a computer on the far end, and headphones with inline controls.
  2 has never been seen.
- Unplugging the far end of a connected cable produces no transition.
- A state the file does not use is no reading.
- It is a binary sensor, a different ESPHome message from a sensor, with its own
  field numbers.
- It carries a `device_class` and no icon. Home Assistant draws a binary sensor
  from its class as a pair of icons, one per state. A device-sent icon replaces
  both with one that never changes.
- A sensor with neither class nor icon is drawn as `mdi:eye`. No class fits a
  volume percentage: Home Assistant's `volume` measures litres. So the volume
  sensors carry an icon.
- Icon field numbers differ per message: 5 on a sensor and a switch, 8 on a
  binary sensor.
- **The message number an entity is listed under decides its kind, and a wrong
  one draws a different entity rather than failing.** The list and state
  numbers are separate lookups: a text sensor is listed under 18 and its state
  goes out under 27. 15 is `ListEntitiesLightResponse`, and a text sensor
  listed there is drawn as a light with nothing reported.

### Whether the speaker is playing

- 2 signals, both needed. ALSA says whether a PCM substream is open: `state:
  RUNNING` in `/proc/asound/card*/pcm*p/sub*/status`, always `pcm23p` here.
  `dumpsys media.audio_flinger` says whether an output thread has an active
  track, true only while sound comes out.
- ALSA alone reports a Dot quiet for 10 seconds as playing. The fork alone costs
  18.7 ms per sample. So the cheap signal is tested first.

| | jack empty | jack occupied |
|---|---|---|
| PCM device | `pcm23p` | `pcm23p` |
| active track | 0.595 seconds | 0.582 seconds |
| substream closes, after opening | 10.555 seconds | 10.565 seconds |

- The route does not move the substream: the codec routes downstream of it.
- The daemon's own player holds a track for the whole run, so idle reads `1
  Tracks of which 0 are active`. The active count is read whenever the dump
  offers it. The total would report every idle moment as playing.
- **Bluetooth is not covered, and it fails quietly.** A2DP does not use the MTK
  PCM device, so no substream runs and the cheap gate answers "not playing".
  With a speaker paired and playing, no substream was `RUNNING` and `pcm23p`
  held an `XRUN`.
- Nothing unreadable is reported as silence. An empty glob, a failed `dumpsys`,
  track lines that no longer parse, and a status file that will not open are
  all `missing_state` -- unless another substream is already running.
- The paths are globbed once and kept: the glob costs 5.6 ms against 2.0 ms for
  reading all 19 files. An empty result, a vanished path, or `pcmRefresh` (a
  minute) re-globs. The minute is needed because a set taken while ALSA is
  still registering is non-empty and readable, and one missing `pcm23p` would
  report silence for the rest of the boot.
- Sound has to last `SoundOnDelay`, 1 second, before it is reported. This keeps
  the 0.4-second press chime out.
- It has to be gone `SoundOffDelay`, 1 second, before that is withdrawn. Two
  of Alexa's playbacks in one answer arrive 56 ms apart.
- A delay takes effect only at a sample, so it is a count of readings. At
  500 ms the withdrawal takes 2 and the report takes 3. Neither may be 1, so
  `SoundEvery` is 1 against `HeavyEvery`'s 5. `main_test.go` holds both delays
  against the interval.
- A failed read leaves the withdrawal clock alone: it records when sound was
  last *seen*. Zeroing it withdraws on the next sample; moving it forward holds
  the entity on across the failure.
- `PollLive` is serial, so on a heavy tick the forks can push the next sample
  out: up to 2.3 seconds between the volume, the microphone and the sound read.
  `soundGap`, twice the interval, catches that.
- With nothing subscribed the poll does not sample, for an unbounded time. So
  the reading and its clocks are forgotten as soon as nobody is subscribed.
  A carried reading reported the speaker playing after Home Assistant came back.
- It forgets when the last subscriber leaves, not when the next arrives, because
  a new subscriber is answered from the published state before any read.
  Nothing wakes the poll on leave, so a reconnect within 1 tick still gets a
  reading 500 ms old.
- Each sound costs about 21 forks across its 10.5-second tail, about 400 ms of a
  core. A chime too short to report costs the same.
- It carries no `device_class`. That is the only way Home Assistant says "On"
  and "Off": all 28 classes rename the states and none fits. `sound` says
  "Detected", which on a Dot with 3 microphones reads as the mic hearing
  something. It takes a static `mdi:speaker`.
- Sound shorter than about 1.5 seconds can be missed, and about 3 seconds just
  after a stalled tick.

### Whether the Dot is registered

- From `dumpsys account`, which lists the accounts `AccountManager` holds. A
  registered Dot has one of Amazon's type:

```
  Accounts: 1
    Account {name=Bryce, type=com.amazon.account}
```

- An unregistered one says `Accounts: 0`.
- `alexa.Installed()` cannot answer this. `amazon.speech.sim` is in
  `/system/priv-app`, so `pm path` finds it on a Dot never set up.
- It does **not** gate the clip route. A clip played to `Playback ended: ...
  SUCCESS` on a Dot with no account.
- The authenticator line names the same type and is present on an unregistered
  Dot too:

```
    ServiceInfo: AuthenticatorDescription {type=com.amazon.account}, ...
```

  So a line counts only if it opens with `Account {` and ends with the type.
- A dump with no `Accounts:` line is no reading.
- The fork runs only while somebody is subscribed. `PollSensors` itself is not
  gated and must not be: it re-asserts the adb firewall rule and reads the adb
  mode.
- So the first subscriber after a restart gets no registration state at all.
  The entity shows unknown, not unavailable, until the next minute tick.

### Which speaker is connected

- `bluetooth_device` names the speaker A2DP is connected to, or gives its
  address when the Dot has no name for it. Empty means nothing is connected.
  It is read only while somebody is subscribed, beside the registration.
- It is one entity, not a flag and a name. A connected speaker always takes the
  route, so `output_device` already says whether one is connected. A flag on
  the minute tick would disagree with the live route for up to a minute.
- The address is `mCurrentDevice` under `Profile: A2dpService` in `dumpsys
  bluetooth_manager`, `null` when nothing is connected. The profile line must
  match whole: `A2dpSinkService` has the same field for a phone playing into
  the Dot, and `HeadsetService` for a route that is not A2DP.
- The name is the `Name` tag in that address's section of
  `/data/misc/bluedroid/bt_config.xml`. The search stops at the next address,
  so a neighbour's name cannot answer. The Dot's own name, under `Local`, comes
  before every address, so it cannot answer either.
- The lookup reads the file in-process: 0.8 to 1.2 ms against a 59,589-byte
  file. It runs only while a speaker is connected.
- The name is made valid UTF-8. A text sensor state is a proto3 string, and
  Home Assistant's client drops the connection on one it cannot decode. The
  snapshot would then drop every reconnect for as long as the state stood.
- `HidService` and the GATT clients carry no audio and are not read, so the
  Alexa app over BLE is not a speaker. `mPlayingA2dpDevice` is not a playing
  signal: it follows the AVDTP stream and was set with nothing playing.
- **A route change to or from `bluetooth` wakes the minute poll**, so the name
  follows the route within about a second. A change between the speaker and
  the jack cannot move the name and wakes nothing. `wakeGap` bounds a route
  that flaps. The route became `jack` and the name cleared in the same second.
- **A swap from 1 speaker to another waits for the minute tick.** The Dot holds
  1 A2DP sink, and `mBluetoothName` is a literal, not a name, so the route stays
  `bluetooth` and nothing wakes. The link drops between speakers in practice:
  across 6 transitions, the name and the route arrived together, 1 to 3
  seconds behind the device.
- `bt_config.xml`'s mtime does not track connections: a disconnect left it
  unchanged for 2.5 minutes, and it was rewritten with nothing connected.

## The volume

- From `dumpsys audio`: `Max:` under `- STREAM_MUSIC:`, and that stream's
  `Current:` line, where each output device appears as `<hex mask> (<name>):
  <level>`. Only the ratio means anything.
- Android keeps a level per route, so there are 3 readings: speaker, jack and
  Bluetooth. A speaker-only reading froze while headphones were plugged in.
- The jack reads `4 (headset)`, else `8 (headphone)`. Nothing here has produced
  `headphone`. `line` and `aux_line` have never moved.
- Any route can be absent while others are present. The search ends at a
  `Current:` line naming **any** readable route, and the mute published is the
  one that block declared.
- **A paired Bluetooth speaker is read the same way.** The line carries
  `80 (bt_a2dp)`, `100 (bt_a2dp_hp)` and `200 (bt_a2dp_spk)`. The device class
  does not pick the live one: a JBL Go 3 declares `DevClass` 2360340, portable
  audio, and its level landed on the plain `bt_a2dp`. So `bt_a2dp` is read
  first and the other 2 are fallbacks.
- The Bluetooth level is reported with nothing connected, as the jack's is:
  `bt_a2dp` read 25 of 30 with no speaker. A mute zeroes all 3 percentages.
- **This level is not the speaker's own volume.** AVRCP absolute volume is never
  negotiated: `mFeatures: 1` (no `0x02` bit), `mRemoteVolume: -1`, an empty
  `mVolumeMapping`, against 2 speakers. One of them does absolute volume with a
  phone, and `bt_stack.conf` carries only tracing. It is absent from this
  bluedroid build, so the Dot attenuates before encoding and the 2 volumes sit
  in series.
- **The live route costs no fork.** `dumpsys audio` ends with `Audio routes:`,
  and on this build `mBluetoothName` is `Device Connected` or `Device NOT
  Connected`. AOSP puts the device alias there, so anything else is no reading.
  `output_device` is a second scan of the same dump.
- `activeRoute` and `activeVolume` treat an unreadable route differently. The
  sensor publishes nothing. The volume falls back to the jack and the speaker,
  because the mute guard reads it through `SpeakerLevel` before holding the
  keys: one unfamiliar `mBluetoothName` must not release them on a Dot that came
  up muted. `readVolumeStep` is the one place the level is read.
- The route counts only what carries audio out. `HidService`, GATT clients and
  the sink direction are real connections and not routes.
- `settings get system volume_music_speaker` gives the same number but starts a
  VM, and reads max and level in 2 commands nothing makes agree. `dumpsys
  audio` takes about 13 ms against 546 ms for one `settings get`.

### What the reading carries

- The step, the percentage, and the maximum. Home Assistant is told the
  percentage. A volume *change* counts from the step. Deriving the step from
  the percentage adds a rounding.
- The maximum travels only with steps. A dump that declares a scale and names no
  usable route reports nothing.
- **The mute does not reach the step.** A muted stream reports 0% (what can be
  heard) and the step its `Current:` line names (where a change starts).
- The maximum comes from `- STREAM_MUSIC:` only: `STREAM_ALARM` carries the same
  30. A stream's block ends at the left margin or at the next `- STREAM_`.
- No usable maximum means no reading: a guessed denominator is wrong, not
  absent. A level outside the scale is clamped.
- The whole parenthesised name must match. The closing bracket keeps
  `speaker_safe` out; the opening one keeps `usb_headset` out. The level is
  after the last colon, because the first field carries the `Current:` label.
- A muted stream reads 0%. `Mute count:` in the same block counts outstanding
  mute requests (`VolumeStreamState`). Its position in the block does not
  matter. 2 counts resolve first-wins, as 2 maximums do. A count that will not
  parse is not a mute.
- Only `setStreamMute` makes the count non-zero, and the daemon's own mute is
  that call. "Alexa, mute" sets the level to 0 and leaves the count at 0.
  `input keyevent 164` does nothing. Stepping below 0 clamps.
- Volume is read every 2.5 seconds rather than every 60 because somebody changes
  it and then looks: about 0.5% of a core, 24 times sooner. Only a changed value
  goes out, so `PollLive` needs no `MinSensorTick`.

## Setting the volume

- Home Assistant sets it through a `media_player`, the only ESPHome entity with
  a volume. `feature_flags` has no transport buttons.
- The volume in a command is read only as a finite `fixed32`. Field 5 sent as a
  varint is refused; reading it as 0 would silence the Dot on a malformed frame.
- A field sent twice is the **last** occurrence, as proto3 reads it. `fixed32`
  then varint is a varint and is refused.
- `has_volume` separates "0" from "no volume": proto3 leaves a zero scalar off
  the wire.
- A level is set outright: `service call audio 4 i32 3 i32 <step> i32 0 s16
  overdub` is `IAudioService.setStreamVolume`. `/system/bin/service` is native,
  not a VM start: about **20 ms** per call, read back current with no settle.
- `flags` 0 makes it silent (the key handler passes `FLAG_PLAY_SOUND`).
  `setStreamVolume` resolves the output device itself.
- Pressing the volume keys instead sounds Android's tick per step, and presses
  outrun the 400 ms settle: a set to 46% landed on step 7 of a target 14.
- **The transaction numbers are this build's, not AOSP's.** `setStreamMute` is
  8, where AOSP 5.1 has `isStreamMute`. 4 is confirmed on all 3 Dots at FireOS
  5.5.5.4. There is no fallback; there is a read back.
- The numbers come from the device: `android.media.IAudioService$Stub`'s
  `TRANSACTION_*` fields through `app_process`, offset from the first. 4 is
  `setStreamVolume`, 8 `setStreamMute`, 2 `adjustStreamVolume`, and **3
  `adjustMasterVolume`** -- calling 3 for a stream is the master mute
  docs/pitfalls.md records. docs/hardware.md says why probing by calling is
  dangerous.
- Safe media volume does not apply: with `SAFE_MEDIA_VOLUME_ACTIVE`,
  `mSafeMediaVolumeIndex=270` (step 27 of 30) and `headset` active, a set to
  30 landed. Whether gain is capped downstream is not known.

### Which route a set counts from

- A relative step counts from the live route: a paired speaker when Bluetooth is
  routed, the jack when occupied, else the speaker (`parseBluetoothRoute` and
  `JackOccupied`). If the live route has no readable level, **or the route
  cannot be read**, nothing is set.
- `activeRoute` prefers Bluetooth to the jack, as `AudioPolicyManager` does.
  With `jack=1`, `mMainType=0x1` and `Device Connected` at once, a
  `setStreamVolume` of 18 landed on `bt_a2dp` while `headset` and `speaker`
  held.
- Both at once means the speaker connected second: **plugging a cable
  disconnects the speaker**, 3 out of 3. `A2dpStateMachine` went Connected to
  Disconnected within 500 ms of the jack switch, and removing the cable does
  not bring it back.
- **A lost link looks different from a dropped one.** A commanded disconnect
  goes `Connected`, `Pending` (`what=2`), `Disconnected`. A lost one goes
  straight to `Disconnected` on a bare `what=101`. A speaker carried out of
  range paged back 33 seconds later. An `output_device` returning to `speaker`
  unprompted is usually this.

### The worker

- A set is a call and a read back, shaped like the microphone switch: one
  worker, one pending request, and `liveWake` at the end so the poll
  republishes.
- 2 relative requests add, so a double tap on volume-up moves 2 steps. A step
  on a pending set adds to it; a set on a pending step replaces it. Half then
  one more is 51.7%; one more then half is half.
- **The slider shows the step, and the mute is a flag beside it.** The volume
  sensors report a muted stream as 0%. The media player cannot: a slider at 0
  while the level is 12 of 30 makes every set snap back. So its volume is
  `step / max`.
- `VOLUME_MUTE` is in the feature flags when a mute can be set. `MUTE` and
  `UNMUTE` are commands 3 and 4, on the volume's worker, so a mute and an
  unmute arriving together leave the Dot unmuted.
- **Holding the mute means holding the volume keys.** A key press does not lift
  `setStreamMute`: muted at step 5, one press left the mute set and the level at
  **1**, because the muted index reads 0. So `/dev/input/event2` is grabbed
  while muted. The first press lifts the mute and moves nothing; 29 presses
  were swallowed with the level unmoved.
- Letting presses through does not lift the mute: 45 samples of
  `getStreamVolume(3)` read 0 while the route's index walked from 13 to **1**.
- **Only the keys lift a mute.** A level set from Home Assistant or a server
  lands and leaves the mute on, because the dump keeps the held step.
- A daemon that cannot take the grab mutes anyway and logs it. The cost: Alexa's
  advanced factory reset cannot complete while muted.
- **A node lost while muted is re-taken `volumeGuardTries` (3) times**, then
  logged. The count resets on the next clean unmute. Unbounded, a node failing
  every read spins open, ioctl and close: 51,748 in 300 ms. The keys stay held
  between an unmute and the read back that confirms it.
- The mute is its own flag, from `Mute count:`. A level stepped down to 0% is
  not muted, and calling it muted shows a mute the control cannot lift.
- **The media player carries no missing flag.** `MediaPlayerStateResponse` has
  no "unknown", so an unreadable level publishes nothing and Home Assistant
  keeps the last value. The volume sensors do go missing; look there when they
  and the slider disagree.

## Playing a clip

- The `media_url` goes to `internal/alexa` untouched: nothing here decodes an
  mp3. `CheckURL` requires `http://` and refuses a comma, double quote or
  backslash, because the extras are a comma-separated array in hand-built JSON.
  Each call gets its own directive id.
- The state follows the speaker, not the request. `PlaybackWatcher` tails
  `logcat -s tts-Server tts-Playback` for `Playback started:` and `Playback
  ended:` in the `SpeechSynthesizer` namespace. A request arms it; the matching
  end disarms it. Alexa speaks for her own reasons, so it is not always armed.
- Reporting "playing" on request would leave a clip she never plays showing as
  playing indefinitely.
- The watcher gives up at a deadline, `playbackWait` (30 seconds), because a
  stack that is not running says nothing. A 404 and a variable-bitrate mp3 both
  end `FAILED`. The deadline is extended once `am` returns, not re-armed, so a
  failure inside that time is already reported.
- `Playback started:` moves the deadline out to 15 minutes. The 30 seconds are
  for starting, not playing.
- **The watcher cannot tell her playback from ours.** The lines are `Playback
  started: uid(32037)_id(0)_namespace(SpeechSynthesizer)` and the matching
  `ended`. Our directive id appears only under `SPCH-SIM_SimJobStack`. So a
  wake-word reply just after a request is reported as ours.
- The hook is taken under the lock, because the check that installs it runs in
  another goroutine.
- **1 clip plays at a time**, through a worker with one pending request. Each
  forks `am`, a fresh VM, on a 512 MB device.
- A url is cut to 64 bytes before it reaches the log, in `CheckURL`'s error as
  well. `am` output is cut too: a java stack trace is kilobytes. The line goes
  through the rate-limited log.
- **Nothing to set with, no control.** The feature flags are computed.
  Sendspin reads the same answer through `CanSetVolume`. `PLAY_MEDIA` and
  `MEDIA_ANNOUNCE` appear only when Alexa's synthesizer is reachable.
- Whether she is there is `pm`'s answer, asked in the background. A debloated
  Dot keeps the apk and loses the package, so a file check says yes wrongly.
  `pm` early in boot can answer a no that would last the run. It is asked every
  30 seconds for 4.5 minutes and latched on the first yes.
- `ListEntities` is answered once per connection, and the cold boot order lists
  entities before `pm` says yes. So installing the hook drops the connected
  clients, and they list again on reconnect.
- **A watcher that cannot start says so once.** A tail that ran for a while and
  then failed is a new fault and is logged again.

## Device info

- `project_name` is `Amazon.Echo Dot (2nd Generation)`. `project_version` is the
  build's tag, shown as `v1.0.0 (ESPHome 2026.8.0)`. An untagged build sends
  `unversioned`, because an empty field still renders.
- The dot in `project_name` is required: Home Assistant takes manufacturer and
  model from `project_name.split(".")[0]` and `[1]`. No dot raises
  `IndexError`. A dot in the model would take `[1]` as a fragment.
- `esphome_version` is `2026.8.0`, a real ESPHome release. It must parse as a
  version: the bluetooth-proxy firmware check compares it through
  `AwesomeVersion` against 2026.5.1. That check runs only for a device with
  proxy flags, which this one does not have.

## The Sendspin switch and delay

- `switch.<name>_sendspin` changes something outside the API: it takes the whole
  Sendspin surface down.
- `UseSendspin` creates it, so a Dot that could not read its Sendspin identity
  lists no switch. The Alexa command box is gated the same way. The adb select
  is not: only its `Secure` option is conditional.
- Its setter must not block. Turning Sendspin off calls `iptables`, which waits
  on netd's xtables lock, and the goroutine answering a `SwitchCommandRequest`
  reads every frame on that connection. So the setter hands a `func(bool)` to a
  worker, and the entity reports what the worker achieved.
- The worker wakes `liveWake` when done. The reading is in `readLive`.
- `UseSendspin` **writes** its func fields without the server lock, during
  wiring before `Poll` and `Listen`. Do not take the lock to read them from the
  command path, which already holds it: that deadlocks, and the suite hangs.
- `number.<name>_sendspin_output_delay` is the output delay. `serve.go` passes
  `sendspin.MaxStaticDelayMS` as the maximum, so the control cannot offer a
  figure the client would clamp.
- **The mode is a box, not a slider, which is why the step is 1 ms.** A slider
  sends a value per step dragged, each a property write (2 forks) and a
  `client/state`. Music Assistant's control sent 23 values in one drag, which
  spent the peer log's whole minute. A box sends one figure. Home Assistant
  draws a box past 256 steps anyway.
- No `device_class`: `duration` is the only candidate, and its units stop at
  seconds.
- Its setter must not block either: persisting is a `setprop` and a `getprop`
  read back, 2 forks.
- **A figure is read as a float or not at all.** Field 2 as a varint, or a
  `NaN`, is refused. The figure is rounded and held to 0 through the maximum.
  Field 2 sent twice is the last.
- **An absent field 2 is 0, not a refusal.** proto3 leaves a zero scalar off the
  wire, so a command for 0 carries only its key.
- A command for the figure already held is neither passed on nor logged. It is a
  repeat only when it matches both what is applied and what was last asked for.
  Matching the applied figure alone drops an operator's second command.
- `NumberStateResponse`'s `missing_state` is always false: this is a setting the
  daemon holds, not a device reading.
- docs/sendspin.md, "Set from Home Assistant", has the rest: the two
  writers, the flash window and the worker the number shares with the switch.

## Discovery

Home Assistant finds the Dot over mDNS. The responder is `internal/mdns`, and
ESPHome supplies its advert through `esphome.Advert`. docs/mdns.md carries the
responder.

## Encryption

`internal/esphome/noise.go`.

- `serve` refuses to start without the key file rather than fall back. It is the
  one API failure that stops the daemon before the button is grabbed: an
  address arrives on its own later, and a key never does. A later bind failure
  is also fatal; exiting hands the button back.
- A plaintext client gets an empty encrypted frame, not a closed socket. Home
  Assistant reads that as `RequiresEncryptionAPIError` and offers to take a key.
  A closed socket is `SocketClosedAPIError`, which prompts nothing.
- A handshake the cipher refuses (a wrong key) is answered with the exact text
  `Handshake MAC failure`. Home Assistant string-matches it as
  `InvalidEncryptionKeyAPIError`.
- Other handshake refusals write nothing and close: a non-empty client hello, an
  empty or oversize frame, a non-zero preamble.
- 2 framings share a connection: `0x01` and a 2-byte big-endian length outside
  the encryption, `[type:2][len:2][payload]` inside it. Plaintext framing is not
  implemented.
- Every frame is bounded before allocation by ESPHome's numbers: 128 bytes
  during the handshake, 32,768 after. Both reads happen before a peer proves
  anything.
- `deploy/install.sh` generates the key only when the device has none, so a
  reinstall does not lock Home Assistant out. Nothing generates a key on the
  device, so no unencrypted first connection exists.
