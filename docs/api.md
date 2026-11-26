# The Home Assistant API

## ESPHome emulation

`internal/esphome`. Home Assistant is the *client* and dials tcp/6053. We answer
the handshake, list entities and push state.

- The advertised API version is 1.15. From 1.15 Home Assistant sends a
  `DeviceCapabilitiesRequest` on every connect and waits for the answer, so
  the request must be answered even when there is nothing to report. Below
  1.15 it builds the capabilities from `DeviceInfoResponse`, which has no
  Sendspin field. Versions 1.13 and 1.14 gate climate flags and an optional
  `object_id`, and nothing this daemon lists.
- The capabilities carry the Sendspin `client_id`, with the pairing-token
  flag, and the API answers `SendspinPairingTokenRequest` with the token.
  That is how Music Assistant pairs the Dot through Home Assistant.
  docs/sendspin.md has the token. A Dot with no Sendspin keys reports no
  Sendspin capability, and answers a token request with `FAILED`, which
  aioesphomeapi documents as "will not start until the device reboots".
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
| registration | `map.db`, read twice | 2 reads, 50 ms apart | minute, subscribed |
| Bluetooth name | `btclient`, then `bt_config.conf`, only over Bluetooth | 340 to 350 ms | minute, subscribed |
| button modes | held in memory | -- | minute |
| adb rule | `/proc/net/tcp`, then `iptables -C` | 1 fork if open | minute |
| adb keys | `/adb_keys` and `/data/misc/adb/adb_keys` | 2 reads | minute, subscribed |
| temperature | a thermal zone's `temp` | 118 µs | heavy |
| memory | `/proc/meminfo` | 111 µs | heavy |
| volume, mute | held from `lipc-wait-event` | -- | heavy |
| route | held from `lipc-wait-event` | -- | heavy |
| jack | `/sys/class/switch/h2w/state` | -- | heavy |
| microphone mute | the privacy driver's `state` | -- | heavy |
| playing | held from the focus watcher, clips and Sendspin | -- | live |

- The minute tick is `sensorTick`, 60 seconds. `PollLive` wakes every
  `liveTick`, 500 ms, and reads what plays every `SoundEvery` tick and the rest
  of `readLive` every `HeavyEvery` tick, 2.5 seconds. Two numbers let a cheap
  reading run oftener without dragging a fork along.
- The uptime changes on every read. The signal costs 16 times `/proc/meminfo`
  and moves slowly. The registration changes only at setup or deregistration.
- The heavy tick cost about 24 ms of a core every 2.5 seconds, 1%, on Fire OS
  5, almost all of it the forks for the route and the microphone. On Fire OS 6
  it forks nothing: the microphone is a sysfs read, and the route is held from
  audiod's events.
- `dumpsys account` and `dumpsys audio` were timed in the same loop, because
  the absolute figure moves with device load: 300 calls took 4.66 and 5.23
  seconds.
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
- Every fork the polls make carries a deadline, because the service behind it
  can wedge. `exec.CommandContext` kills the child, but `Output` then waits for
  EOF, which a grandchild can hold open. `cmd.WaitDelay` bounds that wait.
- `btclient` gets 1 second plus 500 ms, exported as
  `BluetoothReadBudget`. Its output is scanned before `Wait`, where
  `WaitDelay` does not reach, so the pipe is also closed at the deadline. A
  client whose grandchild held the pipe took 30 seconds without that.
- The poll is serial, so a tick spends its forks in sequence. On Fire OS 5 the
  heavy tick's 2 came to 2.4 seconds against 2.5. On Fire OS 6 neither live
  tick forks. `main_test.go` holds the registration's and Bluetooth's sum
  against the minute.
- The budget and the command are variables so a test can shrink the wait and
  substitute a command that never answers.
- `PollLive` reads nothing while nobody is subscribed: nobody would see it.
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
  `PollLive` keeps an early one and serves it at the first pass after the
  gap. It used to drop it, reasoning that its own tick comes in 500 ms, but a
  tick reads the media player, the volume and the mute only every 5th, so a
  clip's start reached Home Assistant 2.1 to 2.6 seconds late. `PollSensors`
  waits out the rest and then serves it, or the value stays wrong until the
  minute tick.
- The polls are the only device readers. A wedged poll leaves every subscriber
  on stale values, so every fork carries a deadline and procfs reads do
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

### What is playing

- `playing` is a text sensor naming the sound in the foreground: `alert`,
  `communications`, `dialog`, `clip`, `content` or `sendspin`, in that order,
  or empty. Alexa's channels keep her own names, lowercased.
- Alexa's come from the focus watcher, docs/audio.md "Ducking while Alexa
  holds focus". `dialog` counts only while `SpeechSynthesizer` holds it: the
  recognizer takes the same channel to listen, and listening makes no sound.
  Her reply takes it over, so the recognizer's late release does not end it.
- `clip` is the daemon's own clip from Home Assistant, announcements included,
  from its start to its end. `sendspin` is a Sendspin stream the chime holds
  and has not finished playing. The chime finishes and closes it itself, not
  through the client, so it is asked, not counted: a count kept in the client
  stayed at 1 after the music ended.
- Earcons are not counted. The ones seen took PuffinApp's `Earcon` channel,
  which the watcher does not hold.
- Alexa's channels, a clip and a stream's open wake the live poll, so the
  state follows them at once. A stream's end, and a stream the client resumes
  rather than opens, wake nothing, so the poll also reads it every
  `SoundEvery` tick, and they show within 500 ms.
- An open stream counts from its open, before its first chunk and through any
  silence in it.
- With no subscriber the reading is forgotten, so a returning Home Assistant
  is not answered with a sound that has ended.
- It is missing when nothing is wired to say what plays: a Dot whose mixer
  did not open has no focus watcher, clips or Sendspin.
- Sound from anything else is not seen: a phone playing to the Dot over
  Bluetooth, multi-room music, an announcement from Alexa. Which channel
  each takes is not measured; the Alexa SDK puts a phone's Bluetooth and
  Spotify Connect on `Content`.
- A lost focus line holds a channel until its 15-minute expiry, as the duck
  does. docs/audio.md has the hold.
- Fire OS 5 read the PCM's state, and AudioFlinger's active tracks to cut the
  substream's 10.5-second tail. Fire OS 6 has no AudioFlinger. The PCM alone
  could not tell an earcon from speech: the mixer writes about 2 seconds past
  every sound, so 2 earcons from a mute toggle ran past a 2-second delay, and
  a delay that long reported real sound late. The PCM cannot see Bluetooth.

### Whether the Dot is registered

- From `/data/ace/kvstorage/map.db`, Alexa's own credential store: a
  `refresh_token` row is a registered Dot. docs/command.md has the store and
  the reader, because the same row is the command's credential.
- A store without the row, without the table or empty is "not registered".
  A store that is missing, cannot be read or parsed, or did not settle in 5
  reads, is no reading.
- Fire OS 5 read `dumpsys account`. Fire OS 6 has no account service, and
  the registered Dot's `pm path amazon.speech.sim` was never an answer either:
  the package is in `/system/priv-app` on a Dot never set up.
- It is read only while somebody is subscribed, as the fork it replaced was.
  `PollSensors` itself is not gated and must not be: it re-asserts the adb
  firewall rule.
- So the first subscriber after a restart gets no registration state at all.
  The entity shows unknown, not unavailable, until the next minute tick.

### The route

- Whether `output_device` is `bluetooth` is audiod's `OutputDevice`:
  `SPEAKER##`, `AUX##` for the jack, or `BT##` for a Bluetooth speaker.
  Anything else is no reading. The jack against the speaker is still the
  jack's switch, "The jack" above.
- It is held from `OutputDeviceChanged`, on the volume's child process, so a
  reading costs no fork. "The volume" below has the child, the seed read, and
  what happens when it ends.
- The event carries the speaker's name, `"BT##JBL Go 3"`. The property never
  does: it read `BT##` for as long as the speaker was connected. So the name
  is not taken from either; a name read only from an event is unknown after a
  restart.
- Only the part before `##` is kept, cut from the event's opening quote. The
  name is never parsed, so a quote or a backslash in it, which
  `lipc-wait-event` may not escape, cannot lose the event.
- On donut the jack's switch and `OutputDevice` changed in the same second,
  both ways.
- Plugging the jack in with a speaker connected disconnects the speaker. The
  route went `bluetooth`, `jack`, `speaker`, and the speaker did not reconnect.
- audiod keeps a level per output: 20 on the speaker, 63 on the JBL Go 3, 57
  on the jack.
- Fire OS 5 read `mBluetoothName` under `Audio routes:` in `dumpsys audio`.
  Fire OS 6 has no audio service.

### Which speaker is connected

- `bluetooth_device` names the speaker A2DP is connected to, or gives its
  address when the Dot has no name for it. Empty means nothing is connected.
  It is read only while somebody is subscribed, beside the registration.
- It is one entity, not a flag and a name. A connected speaker always takes the
  route, so `output_device` already says whether one is connected. A flag on
  the minute tick would disagree with the live route for up to a minute.
- It is read only while the route is `bluetooth`. Any other route is empty
  with no fork, and an unknown route is no reading.
- The addresses come from `btclient`, Amazon's console for `btmanagerd`. It
  reads commands from stdin: `connectedlist`, then `quit`. It answers `Got N
  devices for getConnectedDevices` and then N addresses, each line behind
  `INFO[<pid>]: `.
- It runs with `Pdeathsig`, as the logcat followers do, so a daemon killed
  mid-read leaves no `btclient` behind.
- It buffers stdout into a pipe, so nothing arrives until it exits. A read
  that waited for the answer before sending `quit` waited out its deadline, and
  the sensor read unknown on donut with a speaker connected.
- `quit` sent too soon hangs it: at 20 and 50 ms after the command, 0 of 10
  answered, and each ran until killed. At 100 and 200 ms, 10 of 10 did. So
  `quit` goes 300 ms after the command, and the output is read after that.
- Output that ends before N addresses is no reading, and so is a list that is
  empty while the route says `bluetooth`. An empty reading is how the sensor
  says nothing is connected, and it must not say so for a connected speaker.
- The list is every connected device, not only A2DP. So the address whose
  section in `/data/misc/bluedroid/bt_config.conf` lists the A2DP sink
  service, `0000110b-...`, in either case, is chosen. A phone playing into the
  Dot lists the source service, `0000110a-...`.
- With no sink among them, 1 device is taken as the speaker, and more than 1
  is no reading: the first could be a phone.
- The name is `Name =` in that address's `[aa:bb:...]` section. The file is
  INI. Fire OS 5's was `bt_config.xml`. Only sections named by an address are
  read, so the Dot's own `[Adapter]` name cannot answer.
- The file also holds every link key and the Dot's own LE keys. Read it on the
  Dot; never copy it off whole.
- The name is made valid UTF-8. A text sensor state is a proto3 string, and
  Home Assistant's client drops the connection on one it cannot decode. The
  snapshot would then drop every reconnect for as long as the state stood.
- **A route change to or from `bluetooth` wakes the minute poll**, so the name
  follows the route within about a second. A change between the speaker and
  the jack cannot move the name and wakes nothing. `wakeGap` bounds a route
  that flaps.
- **A swap from 1 speaker to another waits for the minute tick**, if the route
  never leaves `bluetooth`. On Fire OS 5 the link dropped between speakers in
  practice: across 6 transitions, the name and the route arrived together, 1
  to 3 seconds behind the device.

## The volume

- audiod keeps a level per output, 0 to 100, and `MainVolume` is the live
  output's. Unplugging the jack sent `AuxPluggedState 0`, then
  `OutputDeviceChanged "SPEAKER##"`, then `MainVolume` went from the jack's 77
  to the speaker's 40 with no key pressed. Alexa's own level is
  `com.amazon.puffin AvsVolume`, and `MainVolume` follows it.
- Only the live output's level can be read, so there is one volume sensor. A
  sensor for the jack or Bluetooth could only repeat a remembered value.
- `internal/device` follows `lipc-wait-event -m com.doppler.audiod
  MainVolumeChanged,MuteChanged,OutputDeviceChanged` in a child process, as
  `internal/alexa` follows logcat. `MainVolumeChanged <level> 3` carries the
  level; `MuteChanged <0|1> <level>` carries the mute and the level;
  `OutputDeviceChanged "<route>"` carries the route.
- The daemon keeps the last level and mute, so a reading costs no fork. One
  `lipc-get-prop` takes about 86 ms, 54 ms of it CPU: 2 every 2.5 seconds would
  cost about 4% of a core.
- When the child starts, `lipc-get-prop` reads `MainVolume`, `Mute` and
  `OutputDevice`, and again every minute. The read does not wait for the
  subscription, so a change between the two is caught by the next minute's
  read. Each field of a read is
  dropped if an event wrote that field meanwhile: the event is newer. A key
  press during the first read changes only the level, and the mute still comes
  from the read.
- The read is joined before the reading is forgotten, so a child that ended
  leaves no read behind to bring the reading back.
- The failure logged for a child that ended carries its exit status: 3 is no
  such publisher, audiod not up yet.
- A child that ends forgets the reading, so the volume goes missing rather than
  stale. It is restarted every 10 seconds, and the failure is logged once.
- A line that does not parse changes nothing. A level outside 0 to 100 is
  clamped.

### What the reading carries

- The step, the percentage, and the maximum. Home Assistant is told the
  percentage. A volume *change* counts from the step.
- The keys move the level 10/3 at a time: 40, 43, 47, 50, 53. So the scale is
  30 steps, and a level is step `round(level * 30 / 100)`. Every step survives
  the trip to a level and back.
- A level between steps, from the Alexa app, is reported as it is. A change
  counts from the nearest step.
- **The mute does not reach the step.** A muted Dot reports 0% (what can be
  heard) and the step it holds (where a change starts).
- There is no reading until both the level and the mute are known. A mute
  nobody has read is not "off".

## Setting the volume

- Home Assistant sets it through a `media_player`, the only ESPHome entity with
  a volume. `feature_flags` has no transport buttons.
- The volume in a command is read only as a finite `fixed32`. Field 5 sent as a
  varint is refused; reading it as 0 would silence the Dot on a malformed frame.
- A field sent twice is the **last** occurrence, as proto3 reads it. `fixed32`
  then varint is a varint and is refused.
- `has_volume` separates "0" from "no volume": proto3 leaves a zero scalar off
  the wire.
- A level is set on Alexa: `lipc-set-prop -i -q com.amazon.puffin AvsVolume
  <level>`. audiod follows, and Alexa tells the cloud. The set returns when the
  follower has seen the level, at most 1 second later. From Home Assistant's
  command to the read back took about 150 ms.
- **Each set plays Alexa's volume earcon and lights the ring**, as a key press
  does, over music too. The alternatives, measured:
  - `MainVolume` alone is silent and leaves Alexa's level stale. The keys then
    step from audiod's level, but "Alexa, volume up" steps from the cloud's:
    54 went to 40.
  - `MainVolume` and then `AvsVolume` at the same value still plays it.
  - `DuckEarconStreamVol 0` does not silence it.
  - Muting audiod around the set is silent with a 0.4 second wait, and cuts
    whatever is playing for that long.
- `lipc-set-prop` exits with the LIPC error code: 8 for no such property, 3 for
  no such publisher. A level above 100 lands as 100. `Mute` reads any non-zero
  value as 1.

### The worker

- A set is a call and a read back, shaped like the microphone switch: one
  worker, one pending request, and `liveWake` at the end so the poll
  republishes.
- 2 relative requests add, so a double tap on volume-up moves 2 steps. A step
  on a pending set adds to it; a set on a pending step replaces it. Half then
  one more is 51.7%; one more then half is half.
- **The slider shows the step, and the mute is a flag beside it.** The volume
  sensor reports a muted Dot as 0%. The media player cannot: a slider at 0
  while the level is 12 of 30 makes every set snap back. So its volume is
  `step / max`.
- `VOLUME_MUTE` is in the feature flags when a mute can be set. `MUTE` and
  `UNMUTE` are commands 3 and 4, on the volume's worker, so a mute and an
  unmute arriving together leave the Dot unmuted.
- The mute is audiod's `Mute`, set with `lipc-set-prop -i -q com.doppler.audiod
  Mute 1`. It silences Music and Earcon streams alike.
- **Holding the mute means holding the volume keys.** A key press does not lift
  audiod's mute. Muted at 33, presses moved the level to 53 and back to 40, the
  ring showed each step, no tick played, and `Mute` stayed 1. So the volume
  keys are held while muted, by a grab or inside the action node's: docs/
  hardware.md has which. The first press lifts the mute and moves nothing.
- **Only the keys lift a mute.** A level set from Home Assistant or a server
  lands and leaves the mute on.
- A daemon that cannot take the grab mutes anyway and logs it. The cost: Alexa's
  advanced factory reset cannot complete while muted.
- At start the daemon waits up to 30 seconds for the first reading, then holds
  the keys if the Dot is muted.
- **A node lost while muted is re-taken `volumeGuardTries` (3) times**, then
  logged. The count resets on the next clean unmute. Unbounded, a node failing
  every read spins open, ioctl and close: 51,748 in 300 ms. The keys stay held
  between an unmute and the read back that confirms it.
- The mute is its own flag. A level stepped down to 0% is not muted, and calling
  it muted shows a mute the control cannot lift.
- **The media player carries no missing flag.** `MediaPlayerStateResponse` has
  no "unknown", so an unreadable level publishes nothing and Home Assistant
  keeps the last value. The volume sensor does go missing; look there when it
  and the slider disagree.

## Playing a clip

- The daemon plays a clip itself, through `internal/clip`. The media player
  lists 2 `supported_formats`, FLAC at 48 kHz, stereo, 2-byte samples, one
  for ordinary playback and one for announcements. Home Assistant then hands
  every URL to its ffmpeg proxy and sends the proxy's URL instead, for each
  ESPHome player that lists formats. It picks an announcement format only from
  those marked for announcements, so both are listed.
- The proxy URL is on Home Assistant itself, so **the Dot must reach Home
  Assistant's port**. Measured 2026-11-14: caroline, on Fire OS 5, connected
  to 192.168.53.5:8123; overdub-test, on Fire OS 6, was refused on every port,
  by the network, not the Dot. Its own firewall passes all of `wlan0`.
- The clip is read as it arrives, not saved, by `internal/flacread`, which
  Sendspin uses too; docs/sendspin.md has its traps. STREAMINFO must be the
  first block, as the format requires, and say 48 kHz, stereo and 16-bit. The
  blocks after it are skipped, never allocated, and may claim 1 MB together.
  A clip frame may hold 16,384 samples, the cap the clip passes in.
- `testdata/clip.flac` is what the proxy streams: ffmpeg writing FLAC to a
  pipe, so STREAMINFO holds no totals and a comment block follows it.
  `clip.py` there makes it, and prints the hash of ffmpeg's own decode, which
  the test holds the decoder to.
- **1 clip plays at a time.** A new one stops the old, and the old one's end
  is reported at once. Otherwise its duck or Sendspin's withdrawal holds
  through the new clip's fetch, up to 15 seconds. The player reads idle in
  that gap, which is true: nothing plays. After that only the current clip
  reports, so a replaced clip's late end does not mark the player idle under
  the new one. `stop` (feature `STOP`, command 2) goes
  through the same worker as play, so the newest command wins and a stop
  cannot overtake a later play.
- The state is `playing` from the first decoded frame to the end of the queue.
  It goes idle when the last block is handed to the mixer, up to 800 ms before
  the speaker finishes: Fire OS 6's mixer holds 16 blocks of 50 ms.
- On the Dot, served from the Dot itself, a 3-second clip held the mixer's
  stream open 3.10 seconds, and a stop 2.2 seconds in closed it at 2.33.
- A clip and a Sendspin stream meet as Alexa and a stream do. An announcement
  ducks the stream to 20%, the same as her speech, and the deeper of the two
  wins. Anything else makes the Dot unavailable to Sendspin, as her music
  does, so the server ends its stream. Her music starting stops a media clip
  and leaves an announcement playing. docs/audio.md has the duck.
- A clip ducks under her speech and alarms to the same levels as the stream.
  An announcement does not duck itself: its 20% applies to the stream only.
- Her music stops a media clip in one step under the player's lock. A check
  for media, then a separate stop, could stop an announcement that replaced
  the media clip between the two.
- A failure is logged once and reported as the player going idle: a refused
  connection, an HTTP status, or a reply that is not FLAC in the listed format.
  A failed fetch logs its cause, not the URL: the line is cut at 64 bytes, and
  the proxy's URL alone is longer.
- A clip ends with a reason when nothing moves for 15 seconds, whether the
  fetch stalls or the mixer stops taking audio. The watch runs until the
  mixer reports the clip done, not until the fetch ends: up to 2 seconds are
  still queued then, and a mixer that stopped reading would otherwise leave
  the clip playing. Otherwise a stream that stops
  sending without closing reads `playing`, and keeps Sendspin off, until
  somebody presses stop. Home Assistant generates the speech when the proxy
  is first read, and that fits inside the 15 seconds.
- Each fetch uses a new connection. A kept-alive one would sit open to Home
  Assistant for the rest of the boot, and go stale across its restarts.
- A stream cut inside a frame header is a failure, not an end: FLAC ends on a
  frame boundary.
- A clip's start and end are reported under one lock, and each checks that
  the clip is still current under it. Otherwise a stop landing as the first
  frame decodes reports the end, then a late start, and leaves the player
  `playing` with the duck or Sendspin's withdrawal held.
- A stopped or replaced clip is cut off at once, with no ramp.
- A peer holding the API key can make the Dot fetch any http or https URL it
  can route to. On Fire OS 5 Alexa fetched it instead, from the same network.
  Only a reply in the listed FLAC format plays.
- Fire OS 5 handed the URL to Alexa's `SpeechSynthesizer` through `am
  startservice` to `amazon.speech.sim`. Fire OS 6 has neither: no `am`, no
  `app_process` behind the `pm` script, and Alexa is PuffinApp, a native Alexa
  Client SDK build.
- Searched 2026-11-14, nothing on the Dot takes a URL. PuffinApp plays one
  only when a directive says so: `AudioPlayer.Play` carries an http(s) URL its
  own player fetches, and `SpeechSynthesizer.Speak` carries a `cid:` reference
  to a part of the same response. Directives reach it from 2 places only:
  - the cloud downchannel, at the `AVSEndpoint` property of
    `com.amazon.puffin`, which LIPC can write
  - the local hybrid engine, `AlexaHybridExecutionControllerLitespeed`, whose
    socket PuffinApp connects to as a client. Its own speech arrives as `cid:`
    attachments too.
- The rest is narrow. PuffinApp's LIPC properties are volume, locale, state
  and settings. Its `LipcIPCTransport` property feeds the SDK's IPC router,
  which only device-local pairing and multi-room music use. Its AIPC calls are
  audio focus, wake-word blocking and UX restore. The hybrid engine's skills
  are alerts, time, stop and volume, and its smart-home socket is off.
- So the daemon plays a clip itself, as above.
- Standing in for the cloud would reach her own player, at the cost of a relay
  every session passes through and playback events for a stream Amazon never
  issued. That waits for the voice relay.

## Device info

- `project_name` is `Amazon.` and the board's model: `Echo Dot (2nd
  Generation)` on biscuit, `Echo Dot (3rd Generation)` on donut, `Echo (2nd
  Generation)` on radar. `project_version` is the
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
  lists no switch. The Alexa command box is gated the same way. The
  `set_adb_keys` action is not: every Dot has adb.
- Its setter must not block. Turning Sendspin off calls `iptables`, which waits
  on netd's xtables lock, and the goroutine answering a `SwitchCommandRequest`
  reads every frame on that connection. So the setter hands a `func(bool)` to a
  worker, and the entity reports what the worker achieved.
- The worker wakes `liveWake` when done. The reading is in `readLive`.
- `UseSendspin` **writes** its func fields without the server lock, during
  wiring before `Poll` and `Listen`. Do not take the lock to read them from the
  command path, which already holds it: that deadlocks, and the suite hangs.
- `number.<name>_sendspin_output_delay` is the output delay. `serve.go` passes
  `sendspin.MaxOutputDelayMS` as the maximum, so the control cannot offer a
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

## The restart button

- `ListEntitiesButtonResponse` (61) and `ButtonCommandRequest` (62). A button
  has no state, so nothing else is sent.
- Its name, `device_class` and category match ESPHome's own restart button:
  `restart`, `restart` and config. A press restarts the whole Dot;
  docs/device.md has what it does.
- The press runs on a goroutine of its own, so the handler returns at once and
  holds no lock while the daemon shuts down.

## The update entity

- `ListEntitiesUpdateResponse` (116), `UpdateStateResponse` (117) and
  `UpdateCommandRequest` (118). Home Assistant's ESPHome integration shows
  the entity as an update with install, progress and release notes. It reads
  the notes from `release_summary` only when somebody opens them, so the full
  release body goes there, cut at 8 KiB.
- The device fetches the release itself, as ESPHome's `http_request` update
  platform does. Home Assistant only sends `UPDATE` or `CHECK`. ESPHome's other
  update path, where Home Assistant compiles YAML through the ESPHome dashboard
  and pushes the firmware, has nothing to compile here.
- Home Assistant compares the versions itself, with AwesomeVersion. So
  `latest_version` equals `current_version` when nothing newer is offered.
- No state is sent until a check succeeds. Home Assistant's update entity
  ignores `missing_state`, so a state carrying only the running version would
  read as up to date, and an empty `latest_version` as an update.
- The one exception is a binary on its own trial. It was the latest release a
  moment ago, so it sends `latest_version` equal to `current_version` at start.
  Home Assistant keeps the last state it received across a reconnect, and
  that state is the old binary's "installing, 100%"; without the exception it
  stays until the first check, or for good if GitHub is unreachable.
- A binary the boot loop went back to has the same problem and no marker. It
  sends the failed release as `latest_version` at start, with the failure in
  the notes: that release was the latest when it was installed. Until the
  first check knows the release's assets, **Install** does nothing.
- A release the boot loop went back from is named at the top of its release
  notes, from the record the loop writes. The binary that failed held its
  failure only in memory, so without the record the restored binary would
  offer the same release as a clean update.
- The updater is serial: one goroutine runs checks and installs, and a
  command only drops a token in a 1-slot channel. A command arrives with the
  server lock held, so it must never wait.
- A `CHECK` within a minute of the last check is dropped. GitHub allows 60
  unauthenticated requests an hour from one address, and every Dot on a
  network shares that address.
- An install that fails puts the reason at the top of the release notes and
  clears the progress. aioesphomeapi has no other field for it, and the
  install call in Home Assistant returns before the download starts.
- The reason is cut at 300 bytes and made valid UTF-8. It can carry a status
  line or a URL from an unsigned answer, and Home Assistant's protobuf rejects
  a string that is not UTF-8.
- Only a release whose tag is `vX.Y.Z` is offered, and only one newer than
  the running version. A draft, a prerelease, a release without
  `SHA256SUMS.sig` and an older release are not.

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
- `deploy/install.py` generates the key only when the device has none, so a
  reinstall does not lock Home Assistant out. Nothing generates a key on the
  device, so no unencrypted first connection exists.
