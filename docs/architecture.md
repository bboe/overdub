# Architecture

## Button interception

`internal/button` over `internal/evdev`. `event1` carries the action button
*and* mute, so an exclusive `EVIOCGRAB` takes both. The fix is a `uinput` clone
advertising exactly the real key bitmap, named `mtk-kpd` so Android applies the
same keylayout; 138 is consumed and the rest re-emitted. `EventHub` picks the
clone up by inotify. Read the bitmap with `EVIOCGBIT`, never from sysfs: that
file's word size differs from `/proc/bus/input/devices` here, and guessing wrong
silently breaks mute.

The clone copies the original's `struct input_id` too, read with `EVIOCGID` for
the reason the bitmap is read with `EVIOCGBIT`: it is what the device says
rather than what this end believes about one model of Dot. All four fields
matter, and the name is only the last resort among them. Android resolves a
keylayout by `Vendor_XXXX_Product_XXXX_Version_XXXX.kl`, then
`Vendor_XXXX_Product_XXXX.kl`, then the device name, then `Generic.kl`. Measured
on biscuit, neither `Vendor_2454_Product_6500.kl` nor the clone's old
`Vendor_0001_Product_0001.kl` exists, so both fell through to `mtk-kpd.kl` and
the name alone was carrying it. Copying the ids means the clone would keep
matching on a Dot that did ship a vendor keylayout, where the name would not.

The bus is the field that was measurably wrong. `EventHub::isExternalDeviceLocked`
looks first for `device.internal` in the device's `.idc` and answers from that
when it is there; with no such property, which is this Dot's case for both
nodes, it calls a device external when its bus is `BUS_USB` or `BUS_BLUETOOTH`. The
clone hardcoded `BUS_USB` while biscuit's keypad is `BUS_HOST`. Measured before
the change, `dumpsys input` agreed on everything else -- both devices
`Sources: 0x00000501`, `KeyboardType: 1`, identical mapper parameters -- and
disagreed on `IsExternal`, false for the real node and true for the clone. That
is the one thing Android could have treated differently about a key arriving
from a passed-through press, so it is the one thing worth removing.

Copying the ids makes the clone's `InputDeviceIdentifier` byte-identical to the
keypad's, and Android disambiguates them anyway. Measured after the change, the
two report the same `bus=0x0019, vendor=0x2454, product=0x6500, version=0x0010`
and different descriptors, `f0d2e427...:24546500` against `d3f110579...:24546500`,
so the per-device settings that key on a descriptor do not collide. That is the
answer to the obvious objection rather than an incidental reading.

What no test holds is the wiring: the id reaching `uinput` is the one read from
the node. `userDev` is tested against an id a test hands it and `idFromBytes`
against bytes a test hands it, and the two are held together by a round trip,
but putting the old invented id back in `NewUinput`'s caller leaves the whole
suite green. Closing that needs `/dev/uinput` and root, which CI has neither of,
so it is verified on the device instead -- the daemon logs the id it cloned, and
`dumpsys input` reports what Android made of it. That is the rule CLAUDE.md
already states, said out loud here because the failure would reach the Dot
before anything else noticed.

The wait for `wlan0` does not give up. Nothing restarts a daemon that has not
exited, so a bounded wait that expired would cost the API for the rest of the
boot on a Dot whose access point came back a minute late. It polls quietly after
the first minute, because a line a minute for the rest of the boot would bury
everything else.

The button is taken before the network is waited for, and the API waits in its
own goroutine. Waiting for the MAC first left a Dot with no `wlan0` exiting
after a minute and being restarted five seconds later, with no button for the
whole boot. Waiting in the read loop would be no better, because mute passes
through that loop.

`main.go` is left holding the constants, the decision about what a press means,
how long to wait for the node, and the signal handler. That is the seam every
later feature arrives through: the package opens the node and reads it, and the
decisions stay with the caller.

A failed grab is fatal. Without the grab the real node still delivers to
`EventHub`, so a clone that echoed anyway would land every key twice and mute
would toggle on and straight back off. Exiting hands the button back to Alexa
and takes the clone with it; a live clone beside a live original does not.

A failed re-emission is fatal for the same reason. Writes fail for the clone
rather than for one key, so carrying on holds the grab with mute going nowhere,
and nothing restarts a daemon that has not exited. Exiting releases the grab and
the supervisor builds a new clone five seconds later.

The clone is destroyed before the grab is released, and not the other way round.
The read loop can still be running when the close comes, so the reverse order
opens the same window: `event1` ungrabbed and the clone still live, and a key
pressed inside it lands twice. Losing that key is the cheaper failure.

## ESPHome emulation

`internal/esphome`. Home Assistant is the *client* and dials tcp/6053. We answer
the handshake, list entities and push state. The advertised API version is 1.12,
and nothing here needs anything above it: the versions above gate entity kinds
this daemon does not have, and capability requests it would then have to answer.
Home Assistant enforces no floor, so the number states what is implemented
rather than how recent the device is.

Only a frame that decrypts buys the longer read deadline, so a peer that cannot
prove it has the key holds a slot for ten seconds rather than the full idle
budget. Finishing the handshake is not that proof. Message 1 is sealed under the
key, so a peer cannot compose one without it, but nothing fresh from this side
goes into it, so a captured one replays verbatim and Noise will not refuse it.
What a replayer cannot do is send a frame that decrypts, and eight of them would
otherwise hold every slot for the whole idle budget apiece on a captured message
and no key. Ten seconds is one budget rather than one per read, for the same
reason: a peer that stalled the handshake to just inside its deadline would
otherwise be given a second full wait to send that first frame in.

A connection is counted against the eight from the moment it opens rather than
from the moment its handshake succeeds, because a socket that is not counted is
not bounded at all. That is the trade rather than a defence, and it is worth
saying plainly which way it runs: the key gates what a peer can *reach*, never
whether it can *hold a slot*. Eight peers that send nothing at all still fill
the table for a handshake wait each, and can reconnect for as long as they like;
nothing evicts the oldest connection. So the API is available to whoever can
route to the Dot on `wlan0`, and denying that availability costs an attacker
eight sockets. SECURITY.md says the same of the port: bounded rather than
guarded.

A `HelloRequest` that does not parse is not answered. `pbWalk` visits the fields
it read before it failed, so replying would mean replying to whatever was
scraped out of a message we could not understand. `SwitchCommandRequest` is
under the same rule and for a sharper reason -- acting on it means acting on a
key and a state that may both be halves of something else -- and the two are
the only messages whose payloads are read at all: the rest carry nothing this
daemon needs, so their bodies are never looked at.

Every line a peer can cause goes through the rate limit: the accept loop's,
each connection's, and the sensor push's. Three lines in the package are
written outside the count: the line the listener writes once at startup, the
one saying how many were suppressed, and the one saying the ceiling is reached.
Only the first is beyond a peer's reach, since exceeding the burst is what
causes a suppressed-count line. All three stay bounded anyway: the
suppressed-count line is written at most once a window and stops at the ceiling
with everything else, and the ceiling line is written once.

No log write happens with the server lock held, which is why the hello line is
carried out of `handle` by the read loop, a message that will not parse is
reported through its error rather than logged where it is found, and a failed
sensor push is logged after the lock is dropped. The lock gates the accept
path's cap check and every other connection's handler, so a write to `/data`
underneath it stalls the server.

The idle deadline is a read deadline, and what fills it is a ping.
`aioesphomeapi` runs a twenty-second timer that repeats from the moment it
connects, and *any* message from the device, our own state pushes included,
clears the ping that timer has pending. The timer itself never moves, so what a
message buys is one tick rather than a fresh twenty seconds, and the silence
before a ping actually goes out is between twenty and forty. Either way a device
that talks often enough is never pinged, and a deadline waiting for that ping
expires on a connection that is perfectly healthy. Measured against the real
client: a five-second push cadence dropped Home Assistant at exactly ninety
seconds, over and over, with a reconnect each time.

The deadline is ours rather than the client's because this daemon asks. Sixty
seconds of silence draws a `PingRequest` from this side; the answer is a read,
and the deadline resets on it. Ninety more without one drops the connection.
What has gone is the requirement that the client speak first, and with it the
rule that this daemon stay quieter than the client's own timer.

The ping takes over a read that has just expired, and it may only do that when
the read took nothing off the socket. `io.ReadFull` copies what it did get into
a buffer the caller drops along with the error, so a frame that stopped part-way
is already missing those bytes: reading again would take the rest of that frame
as a fresh header, and everything behind it as rubbish. So `readNoiseFrame`
marks every error it returns once the header is read, and the loop drops the
connection on those rather than spending its ping, which is what every read
error did before there was a ping to spend. The mark is on all of them and not
only on the expiry that can reach the ping today, because what it records is
that the stream is no longer at a frame boundary; a later reader who widens what
may be resumed should find that already true rather than have to notice it. Home
Assistant does not produce the case at all -- that needs a segment boundary
inside a frame and then a whole deadline of silence -- but the alternative is a
frame that arrives complete and inside the budget and is refused anyway, under a
log line blaming the peer for bytes this end lost.

Those are ESPHome's numbers and ESPHome's shape rather than ones chosen here.
Its own firmware pings at `KEEPALIVE_TIMEOUT_MS`, sixty seconds, and gives up at
`KEEPALIVE_DISCONNECT_TIMEOUT`, two and a half times it. The ratio is the part
worth copying, and not because a ping can go missing. TCP retransmits, so the
ping is not lost the way a UDP datagram would be. What TCP will not do is say
whether it arrived, and it will not report a peer that has vanished either: on
this device `tcp_retries2` is 15, so the kernel spends about fifteen minutes on
an unacknowledged write before it gives up. The whole hundred and fifty second
budget sits inside that window, which is why the read deadline is what ends
these connections and never the socket. So what the margin buys is a slow answer
rather than a missing one. A Home Assistant part-way through a recorder write, a
garbage collection or an integration reload will service its socket late, and
the longer of the two waits is the one spent on a client that has been asked
something rather than one that has merely gone quiet. Splitting a single budget
in half, which is where this started, gives it the shorter wait instead.

An unreachable peer now costs a hundred and fifty seconds rather than ninety.
The budget that bounds slot exhaustion is the other one and it has not moved:
the ping is only for a peer that has proved it holds the key, and finishing the
handshake is not that proof, since message 1 replays verbatim. A connection that
has decrypted nothing keeps its ten seconds and is not pinged -- pinging it
would hand it a second deadline and twice the slot for free. ESPHome draws the
same line, updating its `last_traffic_` only after authentication.

`MinSensorTick` remains the floor on `PollSensors` because a tick under it is
still a lot of traffic for readings that change slowly, but it is no longer what
keeps the connection alive.

The sensors are read once per tick, and once more whenever a connection
subscribes for the first time. Zero is a plausible value for all three -- zero
dBm is a signal level, zero seconds an uptime, zero percent a volume -- so a
reading that could not be taken cannot be published as one, which the paragraph
on `missing_state` below picks up.

The signal is the fourth field of the `wlan0` line in `/proc/net/wireless`, and
that file is the reason the reading needs two guards rather than none. Measured
on the Dot:

```
 wlan0: 0000    0   208     0        0      0      0      0      0        0
  p2p0: 0000    0     0     0        0      0      0      0      0        0
```

The level is an unsigned byte -- `struct iw_quality.level` is a `__u8`, so no
driver can hand the kernel a negative one -- and -48 dBm arrives as 208, with
256 coming back off anything above 127. A negative number does reach this file,
but only because the kernel subtracts 0x100 itself for a driver that sets
`IW_QUAL_DBM`, and a row written that way never trips the test.

Each column is followed by a dot when its `IW_QUAL_*_UPDATED` flag is set, and
reading the file clears those flags. So the dots are there for the first read
after the driver refreshes and gone for every read until it refreshes again:
measured on the Dot, one dotted row followed by nine plain ones in the same
burst. This daemon reads once a minute, which means it gets the dotted form
essentially every time, and stripping the suffix is the ordinary path rather
than a concession to some other driver.

A zero there is not a reading. The kernel prints a row for every wireless netdev
and fills it with zeros when it has no statistics, which is what `p2p0` is doing
above. Taken at face value that would publish 0 dBm, the strongest signal Home
Assistant can draw, at a moment there is no link at all. So the level is kept
only when it is a signal somebody could receive: at or above zero it is not one,
and at or below -120 dBm it is under the noise floor of any radio. That also
covers the -256 an `IW_QUAL_DBM` driver writes for the same absence, which a
test for zero alone would let through.

Which of these a running Dot actually produces was worth measuring rather than
reasoning about, because the two obvious guesses are both wrong. Across a cold
boot there is no row at all until 23 seconds, the zero row from 23 to 25, and a
real level from 27 -- and the API is up for none of it, because `serveAPI` waits
for the MAC before it binds and on that boot it bound at 27 seconds. Take the
radio down on a running device and the row does not go to zeros either: it
disappears, and the parser leaves by its "not found" path instead. Measured with
`svc wifi disable`, which is what actually disassociates -- `ifconfig wlan0
down` proves nothing, because the framework restores the interface at once and
the driver keeps its last statistics, so the reading never changes.

So the zero row is real and the guard is not dead code, but it is not what
catches a radio that drops. Both paths arrive at the same place, which is the
point: no reading.

Which tick a sensor rides is decided by what its reading costs and by whether
anybody would look for it sooner. Measured on the Dot, per reading:

| reading | cost |
|---|---|
| `/proc/uptime` | 67us |
| `/proc/meminfo` | 111us |
| a thermal zone's `temp` | 118us |
| `/proc/net/wireless` | 1.8ms |
| `dumpsys audio` | 11.7ms |

The poll wakes on one interval and reads on another. `liveTick` is half a
second and `HeavyEvery` is five, so the expensive readings are taken every fifth
tick -- the same two and a half seconds this poll has always read on -- and the
ticks between are free. They are two numbers rather than one because a reading
cheap enough to be worth taking oftener should be able to say so without
dragging a fork along with it: with one number the only way to sample anything
faster was to pay for `dumpsys audio` that often too. A later reading can take a
divisor of its own against the same tick.

Nothing rides the cheap ticks yet, so today the split costs the wakeups it adds
and nothing else: a select and a look at whether anybody is subscribed, twice a
second, and only while somebody is.

So the short tick carries the volume, the temperature and the memory, and the
minute tick carries the uptime and the signal. The uptime is on the minute
because it changes on every read whatever the cadence, so a short tick would
publish it twenty-four times as often for nothing; the signal is there because
it is sixteen times the cost of reading `/proc/meminfo` and twenty-seven times
`/proc/uptime`, and moves slowly.
Three readings on the short tick cost about 12ms of a core every two and a half
seconds, which is half a percent, and almost all of it is the volume's fork.

The CPU temperature comes from `/sys/class/thermal`, in millidegrees: `41300`
is 41.3 degrees.

The zone is looked for once and then read by its path. The search costs 4.6ms
against the 118us of the read it ends in, because it opens the `type` file of
every zone in a directory holding eleven of them and fifty-four cooling
devices. Zones do not appear or move while the kernel is up, so paying that per
reading buys nothing -- and at a two and a half second tick it would have been
forty times the cost of the reading itself. A path that stops working is
forgotten rather than kept, so a reading that fails once does not fail for the
rest of the boot.

The zone is found by its `type` rather than by its index, and that is the
`- STREAM_MUSIC:` rule again rather than caution. This Dot has eleven zones and
fifty-four cooling devices in the same directory, the names are not zero-padded
so `thermal_zone10` sorts before `thermal_zone2`, and nothing fixes which number
the CPU lands on. Measured here, `mtktscpu` is `thermal_zone1` and reads 41.3
degrees; `tmp103` is a discrete sensor elsewhere on the board and reads five
degrees cooler. The SoC is the one worth reporting, because throttling is
decided on its die and not on the board.

The memory is `MemAvailable` from `/proc/meminfo`, in MiB. `MemFree` is the
number that looks alarming and is the wrong one. Measured on this Dot: 35 MiB
free of 472, beside 123 MiB of cache the kernel would hand back on demand, and
`MemAvailable` of 126. So `MemFree` reads as a device about to fall over while
`MemAvailable` reads as what an allocation could actually get. The two do not
add up to each other, which is the point: the kernel's estimate discounts the
cache it cannot reclaim. The unit is taken from the file rather than
assumed, and a line without `kB` is not a reading, because a value scaled by a
unit we guessed at would be wrong rather than absent. The field is absent
before Linux 3.14, and there it is no reading at all: reconstructing it from
`MemFree` and `Cached` is the guessed denominator again, since the kernel's own
estimate accounts for what it cannot reclaim.

A zone with nothing to report answers `-127000`, so the reading is kept only
between -40 and 150 degrees. Neither bound is a temperature a powered SoC
indoors can reach, which is what makes them safe to reject on. Zero is inside
them and passed through: it is not a plausible reading either, but it is not the
kernel's marker for anything, and a guess about which zero is which would be
the `p2p0` row all over again with no second signal to lean on.

The jack is `/sys/class/switch/h2w/state`, a plug and not a device. This Dot's
driver is `accdet_amzn` and every transition it logs is `Headset_plug_in`,
whatever is in the socket: measured here, a three-pole cable with no microphone
pole at all, the same cable with a computer on the far end, and real headphones
with inline controls all read 1, and 2 -- which would mean a plug it thinks has
no microphone -- has never been seen. So the reading is whether the socket is
occupied, and the entity says that and nothing more. A state the file does not
use is no reading rather than a guess.

Unplugging the far end of a connected cable produces no transition at all,
which is the same fact from the other side: the detection is the two contacts
in the socket.

It is a binary sensor, which is a different ESPHome message from a sensor
rather than a sensor carrying 1 -- `ListEntitiesBinarySensorResponse` and
`BinarySensorStateResponse`, with their own field numbers, and Home Assistant
reads fields by number. It goes through the same published state as everything
else, so a plug or an unplug is one state change and a poll that finds no
change sends nothing.

It carries no icon, and that is the decision rather than an omission. Home
Assistant draws a binary sensor from its `device_class` as a *pair*, one icon
per state -- `mdi:power-plug-off` unplugged and `mdi:power-plug` plugged in,
labelled "Unplugged" and "Plugged in" -- and an icon sent by the device
replaces both with one that never changes. So an icon here would cost the state
it is there to show. The sensors are the other way round: a sensor with neither
a device_class nor an icon is drawn as `mdi:eye`, and there is no class for a
volume percentage, since Home Assistant's `volume` measures litres. Both
volumes therefore carry `mdi:volume-high` and the other four take the icon their
class implies. The button switch looks like the jack's case and is not. Home
Assistant does draw a switch from a pair, `mdi:toggle-switch-variant` and
`-off`, but it also renders a toggle control beside it, so the icon is not what
shows the state there and the pair an icon replaces was redundant. What it
costs to keep is identification: a generic toggle says nothing about which of
nine entities it is. So `button_capture` carries `mdi:gesture-tap-button` and
the jack still carries none. The icon fields are not the same number either, 5
on a sensor and a switch, 8 on a binary sensor, which is the
field-numbers-are-per-message rule once more.

Whether the speaker is playing is two signals, and needs both. ALSA says whether
a PCM substream is open -- `state: RUNNING` in
`/proc/asound/card*/pcm*p/sub*/status`, always `pcm23p` here, the same device
`mediaserver` holds. `dumpsys media.audio_flinger` says whether an output thread
has an active track, which is true only while sound is coming out. ALSA alone
would report a Dot that has been quiet for ten seconds as playing; the fork
alone would cost 18.7ms on every sample. So the cheap signal is tested first and
the fork is paid for only when something might be playing.

Measured on the Dot, with the same chime played twice:

| | jack empty | jack occupied |
|---|---|---|
| PCM device | `pcm23p` | `pcm23p` |
| active track | 0.595s | 0.582s |
| substream closes, after opening | 10.555s | 10.565s |

Neither route moves the substream, because the codec routes downstream of it.
The ten and a half seconds is why the track is consulted at all.

The daemon's own player is a track too, and it is held for the whole run rather
than built per chime, so this thread now always has one. Measured idle with the
player up: `1 Tracks of which 0 are active`, where before the change it read
`0 Tracks`. The count that is read is the active one whenever the dump offers
it, which is why the reading is still silence -- and the long form is what this
Dot prints at zero active, measured rather than assumed, because taking the
total instead would report every idle moment as playing.

Bluetooth is the route this cannot answer for, and it fails quietly rather than
loudly. A2DP does not go through the MTK PCM device at all, so no substream
runs, and the cheap gate below answers "not playing" without ever reading the
track that would have said otherwise. It is the same hole the volume has for a
paired speaker, and it is not measured here: nothing was paired to this Dot.

Nothing that could not be read is reported as silence. An empty glob, a
`dumpsys` that failed, and an output thread whose track lines no longer parse
are all `missing_state`, and so is a status file that will not open -- unless
another substream was already running, which is an answer rather than a gap. Silence is a plausible
value at the moment we know least and Home Assistant draws it as a measurement,
which is the `p2p0` argument again. The paths are globbed and kept, since the
glob costs 5.6ms against 2.0ms for reading all nineteen files it finds and
substreams do not move while the kernel is up. Three things reconsider the set:
an empty result, a path that has gone, and a minute passing. The last is there
because the first two cannot see the case that matters -- a set resolved while
ALSA is still registering is readable and non-empty, so it would be kept for
good, and one that happened to miss `pcm23p` would report confident silence for
the rest of the boot. That is the same mistake as reporting silence from a file
we could not read, in its permanent form, and a glob a minute costs 0.01% of a
core to rule out.

The reading is not the sample. Sound has to last `SoundOnDelay`, a second,
before it is reported, and be gone `SoundOffDelay`, another, before that is
withdrawn. The first is the point of the entity: a press plays a chime of four
tenths of a second, and reporting that would make this flicker on every press
rather than say whether the Dot is speaking. The second keeps the gaps inside a
sentence from reading as the end of it -- measured, two of Alexa's own playbacks
in one answer arrive 56ms apart.

A delay only takes effect at a sample, so what it names is a number of readings.
At half-second sampling the withdrawal takes two and the report takes three,
because the clocks are set at different ends: the report's at the first sample
that saw sound, the withdrawal's at the last. What matters is that neither is
one, since then a single bad read moves the entity and the on delay wants its
own sequence to bring it back. `SoundEvery` is one against `HeavyEvery`'s five
for this reason, and `main_test.go` holds both delays against the interval.

Two things take a sample out of its place. A read that failed leaves the
withdrawal's clock alone, because that clock records the last time sound was
*seen* and a failure is not a sighting of silence: zeroing it made the
withdrawal measure against 1970 and fire on the next sample, and moving it
forward -- which is what the gap guard below does -- held the entity on across
the failure and then reported it playing again, which reads as the speaker
resuming. And `PollLive` is serial, so on one tick in five the volume's
two-second budget can push the next sample out, measured at 2.0016s with that
read stubbed to spend it, which `soundGap` catches at twice the interval and
starts both clocks from. That one applies only where there is a reading to hang
it on, which is the first rule again: a clock is only moved by a sample that saw
something. Neither changes what is reported, only when it can next change.

A third case does change it. With nothing subscribed the poll does not sample at
all, and nothing bounds how long that lasts -- Home Assistant restarting is one
stretch, and it can be an hour. Carrying a reading across that reported the
speaker as playing for a delay after Home Assistant came back, and fired
anything triggered on it turning on. So the poll forgets the reading outright,
where the sampling gap keeps it: the difference is that one is bounded by a
fork's budget and the other is not bounded at all.

It forgets once nobody is subscribed rather than when the next subscriber
arrives, and the reason is that the hysteresis is only half of what carries
across. A subscriber is answered from the published state before the poll has
read anything, so forgetting on arrival is too late: the stale reading has
already gone out, and an entity that reports the speaker playing and then
corrects itself fires the same automation the forgetting was there to prevent.
The published reading therefore goes with the clocks.

What that buys is bounded rather than immediate, and the bound is a tick.
Nothing wakes the poll when a subscriber leaves -- `liveWake` is only sent on
subscribe -- so the poll learns that nobody is listening at its next tick, and a
Home Assistant that reconnects inside that tick is still answered from the
published reading. That is the case this does not cover and does not need to:
the value it is handed is half a second old, which is a reading rather than a
memory. What the forgetting is for is the hour, and an hour is many ticks. For
the same reason the drop is not simultaneous with the last subscriber going, so
`published` is momentarily a reading nobody holds; the next tick publishes over
it, because a key that is absent always counts as changed.

The fork is conditional, and the condition is the ten and a half seconds. Every
sound therefore costs about twenty-one forks in its wake, roughly 400ms of a
core spread over that tail, and a chime the entity is designed never to report
costs the same. That, rather than the 18.7ms, is what the reading actually
costs, and it is why the substream is tested first: the alternative is paying it
twice a second for ever.

It carries no `device_class`, which is the only way Home Assistant says "On" and
"Off": all twenty-eight of its binary sensor classes rename the two states and
none of them names this one -- `running` says "Running", `sound` says
"Detected", which on a device with three microphones reads as the mic having
heard something. The class would have supplied a pair of icons, one per state,
so without one it takes a static `mdi:speaker`. The jack keeps none for the same
rule read the other way: there the pair being given up is a plugged and an
unplugged icon rather than two generic circles.

What can be missed is anything shorter than the delay plus the interval, so
about a second and a half -- and about three seconds for sound that begins just
after a stalled tick, since the guard starts the delay again there. Riding the
fork's two and a half seconds instead of sampling every half would make the
ordinary case three and a half and take most of Alexa's replies with it.

The volume comes out of `dumpsys audio`, which carries both the numbers it
takes: `Max:` under `- STREAM_MUSIC:`, and that stream's `Current:` line, where
each output device appears as `<hex mask> (<name>): <level>`. Only the ratio
means anything. Measured here, 12 of 30.

The volume is two readings rather than one, because Android keeps a level per
route and moves between them: with something in the socket the level that
matters is `4 (headset)`, and the speaker's own sits unchanged at whatever it
was. Reporting only the speaker made the sensor freeze the moment anything was
plugged in -- measured, three volume changes on a pair of headphones while the
sensor held 40% throughout, which reads as a broken integration rather than as
a routing question. `8 (headphone)` is read when `4` is absent, though nothing
on this device has ever produced it, and `line` and `aux_line` have never moved
at all. A route paired over bluetooth is a third level again and is not read at
all, so a Dot playing to one is reporting neither of these.

Either reading can be absent while the other is not, which is why a `Current:`
line that names no speaker no longer ends the search: before there were two
routes it could only mean a line we could not use, and now it is the ordinary
shape of a dump whose jack is the half we can answer for.

`settings get system volume_music_speaker` gives the same number and was the
first attempt. It is a shell script that starts a VM, and it puts the two
numbers in different commands where nothing makes them agree. One call answers
both, and costs far less: measured on the Dot, three hundred `dumpsys audio`
calls took four seconds, about thirteen milliseconds each, against 546ms for one
`settings get`.

The maximum is read out of `- STREAM_MUSIC:` specifically, because every stream
in that dump carries one and this Dot's `STREAM_ALARM` carries the same 30 -- a
reader that wandered into the next stream would look right here and be wrong
where they differ. A stream's block ends at the left margin as well as at the
next `- STREAM_`, so that nothing printed after the last stream can answer for
it. What follows the streams was not captured -- the fixture stops at the third
-- so the guard is cheap insurance rather than a response to a measured line. When
there is no usable maximum there is no reading: the maximum is the denominator,
and a guessed one reports a percentage that is wrong rather than absent, which
is the worse of the two. A level outside the scale is treated the other way and
clamped, because there the answer is bounded either way -- a level above the
maximum is full volume and a negative one is silence -- while a missing
denominator leaves the whole ratio unknown. Each device is found by name rather than by position,
and the whole parenthesised name has to match, brackets included. Both brackets
earn their place, and against different names: `speaker_safe` is a device of its
own with a level of its own and is kept out by the closing bracket, while
`usb_headset` -- a real device on an Android newer than this one -- would
otherwise answer for `headset` and is kept out by the opening one. The last
colon in the field is the level's, because the first field on that line still
carries the `Current:` label and so has two.

A muted stream reads as zero rather than as the level it is holding. `Mute
count:` sits in the same block as `Max:` and `Current:`, and Android's
`VolumeStreamState` counts outstanding mute requests there, so anything above
zero is muted while the `Current:` line goes on naming the level the stream will
return to. Reporting that level would be reporting a volume nobody can hear.

Where the count sits in the block does not matter: the reading is settled when
the block ends rather than at the `Current:` line, so a count printed after the
level still applies to it. Two counts in one block resolve first-wins, the way
two maximums do. A count that will not parse is not treated as a mute, because
the alternative is reporting silence on a line we did not understand.

Nothing on this Dot produces a non-zero count, and that was measured rather than
assumed. "Alexa, mute" sets the speaker's level to 0 and leaves every stream's
count at 0; `input keyevent 164` does nothing; stepping below zero clamps. So a
muted Echo is reported by the ordinary level path, reading 0%, and this branch
is not what covers the case it was written for.

It is kept because the dump's `mute affected streams = 0x2e` includes
`STREAM_MUSIC`, so the state exists and an app calling `setStreamMute` would
produce it, and because what it costs is four lines that can only turn a wrong
number into a correct zero. What a non-zero count means is still Android's
semantics rather than this device's, and docs/hardware.md carries the commands
behind the rest.

Volume is the one reading somebody changes and then goes looking for, so it is
read every two and a half seconds rather than every sixty. A two and a half
second tick spends about half a percent of one core's wall time on that read,
and reaches a change twenty-four times sooner than the minute tick would. What
is read is not what is sent: only a value that differs from the published one
goes out, so a volume nobody touches costs the read and no traffic at all.
That is also why `PollLive` has no `MinSensorTick` of its own. The floor there
bounds traffic, and a tick that publishes nothing produces none.

Both polls are started by one call, `Poll`, rather than by a `go` statement each
in `serveAPI`. What goes wrong there is a sensor that is listed with nothing to
read it, which Home Assistant shows as an entity that never has a value, and
inside the package that is a test: every key `listEntities` sends has to arrive
as a state once `Poll` is running. Two `go` statements in `main` are reachable
by no test at all.

Every reading shares one published state, and that is the point rather than a
convenience. It is what every subscriber has been told and the only thing any
of them is ever told: the pollers read the device, and a reading that differs
from what is published replaces it and goes to every subscriber. A client that
has just arrived is answered from it rather than from a reading of its own.

A second reader is what makes that necessary. If the snapshot answering
`SubscribeStatesRequest` took its own reading, it could tell one client a value
the poller never saw, and the poller would then find the device back at the
value it remembered and stay quiet -- leaving that client on a number nothing
would ever correct. It needs no failure of any kind: a volume changed and
changed back inside one tick, with a client subscribing in between, is enough,
and it was reproduced on the Dot before this was written. A tick that carried
every reading whether or not it had changed would heal it within the minute,
which is what the uptime and the signal had while they were the only two
sensors. One reader instead of two is what makes the tick free to carry only
what changed.

`PollLive` raises a tick that is not positive to a second, the same number the
wake gap uses. `time.NewTicker` panics on one rather than returning an error,
and this poll has no `MinSensorTick` for a caller to have been stopped by, so
the panic would reach the supervisor and be repeated five seconds later.

`dumpsys` talks to binder and binder can wedge, so the read carries a deadline
of its own. The poll is serial, so a read that outlasted its tick would delay
every reading behind it, and a wedged binder would cost every reading rather
than one.

The bound is two numbers rather than one, and the deadline alone is not it.
`exec.CommandContext` kills the child when the deadline passes, but `Output`
then waits for the pipe to reach EOF, which a killed child does not close if it
left one of its own behind. `cmd.WaitDelay` bounds that second wait. So the
whole of one read is 1.5 seconds plus 0.5, and `VolumeReadBudget` is exported as
the sum: a hundred and thirteen times what the call measures at, and inside the
two and a half second tick that made it. `main_test.go` holds those two
together, and it has to hold the sum, because a test against the deadline alone
passes while the real worst case runs over.

The budget and the command beside it are variables rather than constants so a
test can shrink the wait and put a command there that never answers.

The volume poll sits still while nothing is subscribed. The read forks a
process, which is a poor thing to do every two and a half seconds on a Dot that
Home Assistant may never have connected to, so the cost above is what it spends
while somebody is listening and nothing at all otherwise.

Coming back from that is the only thing a subscriber can ask a reading for.
Subscribing wakes the polls rather than taking a reading of its own -- the same
rule as the snapshot -- and only on the first request of a connection, since a
peer holding the key can send them as fast as it likes. And only when nobody
else is subscribed: with another
connection already there the polls have been running, and the published state
the new one was just answered from is a tick old at worst. So the ordinary case,
a second client arriving while Home Assistant is connected, reads nothing at
all. What is left is a peer that keeps making itself the idle case by
connecting, subscribing and leaving, which is bounded by a second between reads
a *wake* can cause. The tick is not bounded by it, because that is a cadence
somebody chose rather than a read a peer asked for, so the ceiling is a wake per
second alongside the tick's own read rather than one read per second overall.

So the snapshot is sent from `handle`, under the lock that orders it against a
concurrent push, and it reads nothing. That is also why `publish` collects what
it could not send and logs after the lock is dropped rather than under it.

A wake is sent with the server lock held, so it is a send that may be dropped
rather than one that may block. A bare send would deadlock the server outright:
`handle` holds the lock for its whole body, and the poll that would empty the
channel takes that same lock to publish.

`MinSensorTick` bounds `PollSensors`' ticker rather than the push rate, which a
wake exceeds. Both polls therefore hold a wake to `wakeGap`, a second, because
everything that wakes either of them is something a peer asked for. They differ
in what they do with one that arrives too soon: `PollLive` drops it, since its
own tick comes round in half a second, and `PollSensors` waits the remainder out
and then serves it, since dropping one there would leave the value it was going
to correct wrong until the minute tick.

The pollers are the only things that read the device, which is the cost of there
being one reader rather than two: a poll that wedged would leave every
subscriber on the last published values, including ones that connect afterwards,
where before this each new subscriber at least took a reading of its own. That
is why the volume read carries a deadline and the two procfs reads do not.


A reading that fails is still sent, with `missing_state` set. Leaving it out was
the first attempt and it is only right before the first one: afterwards Home
Assistant keeps drawing the last value it was given, so a radio that has dropped
shows the signal it had when it was working, beside an uptime still ticking.
Measured through Home Assistant's own client with the radio disabled: `state=0.0
missing_state=True`, which it renders as no value rather than as a measurement.

The line is matched on the whole interface name. `lo` is not in this file, so it
is not what the match is for: `p2p0` is, and so is any driver that adds a second
wireless netdev whose name contains ours.

`DeviceInfoResponse` carries an `esphome_version` of `2026.8.0`, which Home
Assistant shows as the device's firmware version. It names a real ESPHome
release: ESPHome versions by calendar the way Home Assistant does, which is why
the two look alike. Nothing on the Dot corresponds to it. It has to parse as a
version, because the bluetooth-proxy firmware check runs it through
`AwesomeVersion` against a floor of 2026.5.1, but that check is reached only
for a device advertising proxy flags, which this one does not.

`Listen` retries a failed `Accept` rather than returning from it. Returning would
leave the socket bound with nobody accepting, so Home Assistant would hang on a
connection the kernel had already completed, with the iptables counter showing
the packets arriving, which README.md gives as the sign that the firewall is not
what dropped them.

Every connection has its own queue and its own writer goroutine, so the server
lock is never held across a socket write. Held there, one subscriber that has
stopped reading parks every state and every other client behind it for the ten-
second write deadline, and a second stalled client costs another ten. A client
that cannot keep up overruns its queue and is dropped instead, which is what
ESPHome itself does. Connections are capped for the same reason: one frame in
flight each, and the product has to fit a device with 512 MiB of RAM, of which
`/proc/meminfo` reports 472 MiB usable. Every memory figure here is binary, and
so is the file they come from: the kernel writes `kB` and means KiB, which is
why a reading of it is divided by 1024 rather than by 1000.

## The button switch

`button_capture` is the one entity Home Assistant writes to, and everything
else here is read-only. Off, the action button is Alexa's again; on, it is this
daemon's and a press does what it did before. It is a switch rather than a
diagnostic reading of whether the grab took, because what somebody looking at
that reading almost always wants is to change it.

**The grab is untouched either way.** Releasing it is the obvious reading of
"pass the button through" and it is the wrong one: the real node still delivers
to `EventHub`, so a released grab beside a live clone lands every key twice, and
mute would toggle on and straight back off. What "not captured" means instead is
that keycode 138 is re-emitted through the clone like every other key. The clone
is named `mtk-kpd` so Android applies the same keylayout, and that is the route
mute has always taken here, which is the reason the clone carries the whole key
bitmap in the first place.

How far that is measured is worth being exact about. Android's input layer
treats the two devices identically: the same keylayout, the same
`Sources: 0x00000501`, the same `KeyboardType`, and since the ids are copied the
same `IsExternal`. `mtk-kpd.kl` maps `key 138 BUTTON_MODE` and both devices
resolve to it.

The app layer was the open question and it is now answered. Measured on the Dot
with capture off and 138 injected into `event1`, which reaches the daemon and
not `EventHub` because `EVIOCGRAB` gates reading rather than writing:

```
HeadlessKeyPolicyManager: KEYCODE_BUTTON_MODE, scanCode=138, deviceId=24, source=0x501
KeyEventObserver:         Received uber, keyCode=110, state=0
KeyListener:              STATE_DOWN -> STATE_UP -> STATE_SHORT
SPCH-SIM_StartSpeechCommand: mInitiator=SHORT_BUTTON_PRESS
SPCH-SIM_SimStateMachine:    ReadyState -> ListenState
```

`deviceId=24` is the clone, so Alexa's handlers do not care which device the
`KeyEvent` came from; the daemon logged no interception for that press and did
for the same injection with capture on, which is the control. "uber" is the
Dot's own name for the action button, and is what to grep for.

What that measures exactly is press-to-talk. Stopping a timer and entering setup
mode were not separately exercised, and they are the same `uber` key reaching
the same `KeyListener`: `STATE_SHORT` is what press-to-talk and a timer stop
both hang off, and setup mode hangs off the long press instead. So they follow
from the same evidence rather than resting on it, which is a weaker claim than
the one above and worth keeping apart from it.

Releasing the grab instead, so the real node delivers to `EventHub` directly, is
the obvious alternative and is rejected. It would give exact fidelity and cost a
worse property: two paths to Android rather than one. Toggling between them
inside a press desynchronises Android's key state, and one direction is bad
rather than untidy. Taking the button back while the key is held means Android
already has the key-down natively and never receives the up: `EVIOCGRAB` makes
the kernel synthesize no release, and a synthetic one through the clone cannot
clear it, because `dumpsys input` tracks `KeyDowns` per device. A `BUTTON_MODE`
stuck past six hundred milliseconds is Alexa's long press, which is setup mode.
With the clone as the only path there is nothing to desynchronise: a press is
either delivered whole or not at all, which is what latching at the key-down
buys.

**A press is latched at its key-down and stays latched until the key-up.** A
toggle can arrive between the two, and the two halves of one press must not take
different routes. The failures are not symmetric. Consuming the down and passing
the up gives Android a release for a key it was never told was pressed, which it
shrugs at. Passing the down and consuming the up leaves it holding `BUTTON_MODE`
for ever. So the flag is read once, at the down, and the release follows it.

**The button owns the flag and the server reads it**, rather than the server
owning it and the button being told. The read loop consults it on every event
and cannot take the server lock to do it, so it needs its own copy whichever way
round this goes; one authority means there is only ever the one. The zero value
is a captured button, so a construction path that never mentions capture keeps
the key rather than quietly handing it to Alexa.

Nothing is published from `handle`, and that is a hard rule rather than a
preference: `publish` takes the server lock, `handle` holds it for its whole
body, and a `sync.Mutex` is not reentrant, so publishing there deadlocks that
goroutine with the lock held and wedges the accept path and every other
connection behind it. So a toggle wakes `PollSensors` instead and the state goes
out on that poll's next turn -- which is also why the reading rides `readTicked` rather than
having a path of its own. That poll publishes once before its first wait, so the
switch has a state before any connection exists.

That wake is the one thing here a peer can ask for repeatedly on a connection it
already holds, and it is why `PollSensors` has a `wakeGap` at all. Subscribing
wakes the polls only on a connection's first request, so a peer spamming that
needs a new connection each time and the eight slots bound it; a switch command
needs neither. The no-op guard turns away a command asking for the state the
button is already in, and that is all it does: a peer alternating on and off
changes the state every time and so passes it every time. What bounds that is
the gap on the poll, which is where the bound belongs, since the guard can only
ever recognise the case a peer would not bother sending.

The log line saying which way the button went is carried out of `handle` on the
connection, the way the hello line is, because the log is a file on `/data` and
the lock gates the accept path and every other connection. It is the only record
that separates a button nobody is answering from one Home Assistant let go of,
and it is not a guarantee: it goes through `peerLogf` like every other line a
peer causes, so a peer that has already spent the run's ceiling on connection
churn moves the button unrecorded.

Exempting it from that budget is the obvious fix and is wrong. `conn.noted` is
set once per state *change*, which is once per message a peer sends, not once
per wake -- the gap bounds the poll, not the toggles. So an exempt line is an
unbounded write to `/data` by an unauthenticated-until-the-key peer, which is
the hazard docs/pitfalls.md exists for. A budget of its own would work; sharing
the general one and saying so is what is done.

`SwitchStateResponse` has no `missing_state` field, unlike the other two state
messages: a switch is what this end last set it to, and there is no read of the
device to have failed. Its listing numbers are its own as usual -- 17, 26 and 33
against the sensor's 16 and 25 -- and `entity_category` is field 8 there,
carrying `config` rather than the `diagnostic` every other entity here carries.

## Discovery

`internal/esphome/mdns.go`. Without it the Dot is added to Home Assistant by
address, and a DHCP reservation is the only thing keeping that address true.
With it the Dot answers for `<name>._esphomelib._tcp.local.` and Home
Assistant's own integration offers the device unprompted. The messages are built
and read with `golang.org/x/net/dns/dnsmessage`; docs/constraints.md says why
that import is here.

It is separate from the API server because the two are different sockets with
different rules: a TCP listener with a key and a connection cap, and a stateless
responder that answers anyone. They share the name, the MAC, the port, and the
version string, which is one constant so the TXT record and `DeviceInfoResponse`
cannot drift.

Nothing on the reply path is logged, because a peer causes every reply. A failed
send is kept for the next address poll instead, at most once per rebuild, and
counts against the socket only when it was aimed at the group: a querier can name
somewhere unroutable, port zero does it, and counting that as ours hands anyone
on the subnet a rebuild every poll. The responder rebuilds rather than adapts,
because the socket is bound to an interface address and the records name it, so
both go wrong together.

Two of RFC 6762's rules are load-bearing. A record is multicast at most once a
second, which is the only cap on the one unbounded thing a peer can ask for:
forty bytes of query draw 328 to 700 back, at every host on the segment. And a
query already carrying our PTR at half its TTL is not answered at all, because
python-zeroconf repeats the known answer on every refresh and each one would
otherwise draw a full reply. That answer is read from the answer section, which
begins after *all* the questions — Home Assistant sends about ninety in one
packet. It has to fit in that packet too: known answers that overflow one
message are split across two, and the second carries no question, so a query
split that way is answered. That costs a reply the rule would have saved and
nothing else, because the once-a-second limit still holds. Unicast replies are held to twenty a second rather than one, because
they reach only the host that asked, and a spoofed source otherwise makes this
a reflector.

Six divergences from the RFC are deliberate, and none of them stops
python-zeroconf resolving the device: the answering record sits in the
additional section, which it merges anyway; no NSEC is sent for the AAAA this
device does not have; the source port is never looked at, so a legacy querier
is answered as though it were an ordinary one, which means by multicast unless
it asked for unicast and so by nothing it can see if it did not;
shared records skip the twenty to a hundred and twenty millisecond delay meant
to spread collisions among responders this network has one of; the
`_services._dns-sd._udp.local.` meta-query goes unanswered, so a general browser
never learns the Dot exists; and a truncated query is answered rather than held
for the known answers that follow it, which section 7.2 asks for and
python-zeroconf's own responder does.

**Nothing probes for the name, and nothing notices a conflict.** The Dot claims
its host and instance records as unique without asking whether another device
holds them, and the read loop discards responses, so a conflicting claim is
never seen. Two Dots given the same `-name` both assert the same records and
Home Assistant sees one device flapping between two addresses; README says the
name has to be unique, and that is the whole of the enforcement. The same
deafness means a spoofed goodbye retires the Dot from every cache on the
segment, unnoticed, until something asks again.

## The chime

`internal/audio`. A press is acknowledged by the daemon's own sound, played
through OpenSL ES, which is the layer AudioFlinger mixes. That is what lets it
coexist with Alexa: her speech is a track like ours rather than an owner of the
device. Writing to ALSA directly is not an option and fails worse than silently:
card 0 device 23 is the speaker, declares one substream, and the audio HAL holds
it open for the life of the boot, so a second opener gets `EBUSY`. The DSP
front-ends around it do accept a write and do reach the speaker, and doing so
corrupts the codec stream and leaves the driver logging
`mtk_pcm_I2S0dl1_pointer underflow` fast enough to empty the kernel ring buffer,
until a reboot. Every sound on the device pops in that state.

Handing a URL to Alexa's `SpeechSynthesizer` via `am startservice` was the route
before this, and it worked. What it cost was 691ms from press to sound against
33ms now, measured five trials each at the moment the codec substream goes
RUNNING. Nearly all of the difference was hers: binder to her service, an http
fetch, an mp3 decode. It also required a loopback listener alive for the life of
the daemon, four separate quirks of `SpeechInteractionManager`, one exact mp3
encoding, and a stack that a debloated Dot may not have running at all. None of
that is needed to make a sound.

**The player is built once and held.** Building it per press was measured at
333ms, and almost all of that is `exec` plus linking `libOpenSLES`: the helper's
own work, from its `main` to the buffer queue, was 4ms. A resident process is
therefore the whole trick, and once one is resident it may as well be this one --
a separate helper measured the same 33ms while costing a second binary to push
and verify, a pipe, a child to supervise, and 10MB of its own.

**It is cgo, so the target is `GOOS=android` rather than `GOOS=linux`.** That is
not a preference. Go's linux runtime hangs before `main` against Bionic, so a
linux build does not fail, it stops -- with no output, which is what makes it
worth stating. Bionic also folds pthread into libc and ships no `libpthread`,
while cgo appends `-lpthread` regardless, so `build.sh` makes the empty stub
archives older NDKs used to carry.

The daemon pays about 9MB of RSS for holding the media stack in its own address
space, roughly doubling it, and CI keeps building for `GOOS=linux` because the
audio package is behind a build tag. What that buys is the tests and `go vet`
running exactly as they did, on the real 32-bit target, with the audio path
verified on the device like everything else that touches hardware.

**The sound is generated rather than stored**, which is why no asset is in the
tree and nothing needs ffmpeg to regenerate one. Two sustained sine tones a
fifth apart, A5 then E6, ramped at both ends and fading over the last tenth of
a second. Those
are the recording's own numbers rather than a choice: a DFT over each half of the
clip this replaces reads 880.0 Hz and 1320.0 Hz, within two cents of A5 and E6,
with no partial above the fundamental carrying enough energy to matter, and an
envelope that holds its level for three quarters of its length rather than decaying
like a bell.

Generating it once costs 12.7ms on the Dot and 39.4KB held for the run.
Generating it per press would spend that 12.7ms inside the 33ms budget, which is
most of what the change bought, and the buffer queue holds a *pointer* into the
PCM rather than a copy -- so a clip regenerated under a second press is a
use-after-free rather than a slow chime. The 39.4KB is 0.008% of what this device
has.

## Encryption

`internal/esphome/noise.go`. `NewServer` takes the key rather than reading it,
and `serve` refuses to start if the file is missing rather than falling back to
something weaker. That is the one API failure that stops the daemon
before the button is taken: an address arrives on its own a few seconds later,
and a missing key never does. Reading it first is what lets a daemon that cannot
serve the API exit without having taken the button away from Alexa. A bind that
fails later is fatal too, but by then the button is already grabbed, and exiting
is what hands it back.

A client that opens in plaintext is answered with an empty encrypted frame
rather than a closed socket. Home Assistant reads that as
`RequiresEncryptionAPIError` and offers to take a key; a closed socket reaches
it as `SocketClosedAPIError`, which prompts nothing and leaves the device
unavailable with no way forward. A handshake refused by the cipher, which is
what a wrong key looks like, is answered with the exact text `Handshake MAC
failure`, which Home Assistant string-matches to report a wrong key rather than
a generic failure. The handshake's other refusals write nothing and close: a
client hello that is not empty, an empty or oversize frame, a non-zero
preamble. None of those is a peer that holds the key and got it wrong. Both
were driven with `aioesphomeapi`, the library Home Assistant itself uses, and
report `RequiresEncryptionAPIError` and `InvalidEncryptionKeyAPIError`
respectively.

Two framings live in one connection, which is ESPHome's doing: `0x01` and a
two-byte big-endian length outside the encryption, `[type:2][len:2][payload]`
inside it. The third, plaintext framing is not implemented at all: it would only
serve a client this daemon refuses.

The 16-bit length is what the field can say rather than what is accepted. Every
frame is bounded before it is allocated for, and by ESPHome's own two numbers:
128 bytes while the handshake is running, 32768 once it is done. Both matter
because both reads happen before a peer has proved anything, and taking the
field at its word would let one that has proved nothing reserve 64 KiB. The
largest message this daemon will send comes out of the second number, less the
four-byte inner header and Poly1305's sixteen-byte tag.

The key is generated by `deploy/install.sh`, and only when the device has none,
so reinstalling does not lock Home Assistant out of a Dot it was talking to.
Nothing generates a key on the device itself, so there is no unencrypted first
connection to be caught during.
