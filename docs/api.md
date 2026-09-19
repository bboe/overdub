# The Home Assistant API

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
scraped out of a message we could not understand. `SelectCommandRequest` is
under the same rule and for a sharper reason -- acting on it means acting on a
key and a mode that may both be halves of something else -- and the two are
the only messages whose payloads are read at all: the rest carry nothing this
daemon needs, so their bodies are never looked at.

Every line a peer can cause goes through the rate limit `internal/untrustedlog`
keeps: the accept loop's, each connection's, the sensor push's, the button
press's, and the three written from workers of their own -- the playback
watcher's, the play worker's and the command worker's. Seven lines are written
outside the count, five here and two in that package: the line the listener
writes once at startup, the two saying a tick was raised to its floor, the two
saying the entity list was dropped so it is sent again, and -- in the package
-- the one saying how many lines were suppressed and the one saying the ceiling
is reached.

Only the first three are beyond a peer's reach. A peer sets how many of the
relist pair are written, one per connection it is holding, up to the eight the
listener allows, and they carry its address and nothing else of its own.
Exceeding the burst is what causes a suppressed-count line, so that one is a
peer's to cause as well. All seven stay bounded anyway: the three are written
once a run and the relist pair at most eight lines each, once; the
suppressed-count line is written at most once a window and stops at the ceiling
with everything else; and the ceiling line is written once.

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
| `dumpsys account` | 10.5ms |

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

`dumpsys account` was measured against `dumpsys audio` in the same loop rather
than on its own -- 4.66 seconds to its 5.23, for three hundred calls each --
because the absolute number moves with whatever else the device is doing, and
what the tick decision needs is the comparison.

So the short tick carries the volume, the temperature, the memory, the jack and
whether the microphone is muted, and the minute tick carries the uptime, the
signal and whether the Dot is registered. The uptime is on the minute
because it changes on every read whatever the cadence, so a short tick would
publish it twenty-four times as often for nothing; the signal is there because
it is sixteen times the cost of reading `/proc/meminfo` and twenty-seven times
`/proc/uptime`, and moves slowly. The registration is there because it is a
fork, and because the thing it reports changes when somebody sets the Dot up or
deregisters it and at no other time.
Five readings on the short tick cost about 24ms of a core every two and a half
seconds, which is one percent, and almost all of it is the two forks: the
volume's 11.7ms and the microphone's 12ms, against a few hundred microseconds
for the three procfs and sysfs reads together.

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
volumes therefore carry `mdi:volume-high` and the rest take the icon their
class implies. The button mode looks like the jack's case and is not. A select
is drawn with a dropdown showing the chosen option, so its icon was never what
shows the state and no pair is given up by sending one. What it
costs to keep is identification: a generic control says nothing about which of
the device's entities it is. So `action_button_mode` carries
`mdi:gesture-tap-button` and the jack still carries none. The icon fields are
not the same number either, 5 on a sensor and a switch, 8 on a binary sensor,
which is the field-numbers-are-per-message rule once more.

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
resuming. And `PollLive` is serial, so on one tick in five the forks
it carries can push the next sample out -- up to 2.3s of budget between the
volume, the microphone and the sound read itself -- which `soundGap` catches at
twice the interval and starts both clocks from. That one applies only where there is a reading to hang
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

Whether the Dot is registered comes out of `dumpsys account`, which lists the
accounts Android's `AccountManager` holds. A registered Dot has one, and the type
is Amazon's:

```
  Accounts: 1
    Account {name=Bryce, type=com.amazon.account}
```

An unregistered one says `Accounts: 0` and nothing under it. Measured on two
Dots, one of each.

The reading is `alexa.Installed()`'s complement rather than a second opinion on
it. `amazon.speech.sim` is in `/system/priv-app`, so it answers `pm path` on a
Dot that has never been set up at all, measured on the unregistered one: the
package says the stack is there and says nothing about whether it has an account
to use.

What the registration does **not** gate is the clip route. A clip asked for over
the API played through to `Playback ended: ... SUCCESS` on a Dot with no account
at all, measured when that route was added, so this reading is not a predictor
of the media player and must not be read as one. What it reports is the
condition nothing else here surfaces: a Dot that was never set up, or that
deregistered itself, keeps answering the button and every entity on this list
while it has stopped being an Echo.

The fork is taken only while somebody is subscribed, which is the rule the live
tick already follows and the minute tick did not have to before: the two
readings it carried were `/proc` reads costing microseconds, and this one is a
fork. So a Dot that is installed and never added to Home Assistant spends
nothing on it at all, rather than 10.5ms a minute for ever.

`PollSensors` itself is not gated, and must not be: it re-asserts the firewall
rule and reads the adb mode, neither of which is for a subscriber's benefit. The
gate is around the one reading.

What the gate costs is the first state. A subscriber is answered from
`published`, which holds only readings that were actually taken, so the very
first one to arrive is sent no registration at all rather than a missing one:
the key is not there to replay. The reading follows a moment later, when the
wake that subscribing sends reaches `PollSensors`. Every later subscriber is
answered from the last reading taken while somebody was listening.

So the entity is unknown for that moment rather than unavailable, and only on
the first connect after a restart. That is the trade against a fork a minute on
a device nobody is asking.

What the parser must not read as an account is the authenticator:

```
    ServiceInfo: AuthenticatorDescription {type=com.amazon.account}, ComponentInfo{...}
```

That line names the same type, and it is there on the unregistered Dot too,
which is the whole trap: a search for `com.amazon.account` alone reports every
Dot registered. So a line counts only if it opens with `Account {` and ends with
the type, and both halves have a fixture.

A dump that carries no `Accounts:` line at all is no reading rather than an
unregistered Dot, for the same reason the signal's zero is no reading: off is a
state somebody would act on, and a `dumpsys` that failed or changed its shape is
not evidence of one.

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

**The reading carries the step as well as the percentage**, and the maximum it
was scaled by. A percentage is what Home Assistant is told. A step is what a
volume *change* has to start from, because a relative step counts from it.
Deriving the step back out of the
percentage would work at this scale and is still the wrong way round: it puts a
rounding between the number that was read and the number a change is counted
against, and the maximum is already on the same read as the level.

The maximum travels with the steps and only with them. A dump that declares a
scale and then names no route this parser can use reports nothing at all, rather
than a scale standing on its own, so nothing downstream can read a maximum as
evidence that there was a level to scale.

**The mute does not reach the step.** A muted stream reports zero percent,
because zero is what can be heard, and reports the step its `Current:` line
names, because that is where a change starts from. Reporting zero for both
would have a muted Dot count its way up from a level it is not holding, and
land as far below the level asked for as the mute was holding back.

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

**Home Assistant can also change the volume**, and it does that through a
`media_player`, because that is the only ESPHome entity with a volume on it.
What the Dot gets is the control rather than a player: `feature_flags` declares
`VOLUME_SET` and `VOLUME_STEP` and nothing else, so the card carries a slider
and no transport buttons. Declaring `PLAY_MEDIA` before there is anything to
play would draw a control that does nothing, and the state is `IDLE` for the
same reason -- nothing here yet starts a sound the media player owns.

The volume that arrives in a command is read only when it is a `fixed32`, and
only when it is finite. A field 5 sent as a varint is not a float that happens to
be zero -- it is a peer saying something the message does not allow, and treating
it as zero would step the Dot to silence on a malformed frame. The clamp bounds
what a nonsense fraction can do, but the clamp is a floor rather than the check.

**A level is set outright, through the same door the mic mute reads.**
`service call audio 4 i32 3 i32 <step> i32 0 s16 overdub` is
`IAudioService.setStreamVolume`, and `/system/bin/service` is a native binary
rather than a VM start. Measured on a Dot: 20 calls in 472ms against a 61ms
baseline, about **20ms each**, read back current with no settle. `flags` is 0
where the key handler passes `FLAG_PLAY_SOUND`, so it is silent, and
`setStreamVolume` resolves the output device itself -- with a cable in, a set
moved `headset` 17 to 20 and left `speaker` at 11.

**It replaced pressing the volume keys**, which sounded Android's tick once per
step -- a nine-step move heard nine times, an automation at four in the morning
heard across the room -- and undershot: a set to 46% landed on step 7 of a
target 14, because the presses outran the 400ms settle they were read back
after. The uinput device, its stuck-key exit and the rule about counting presses
from the live route are all gone with it. Reads still choose a route; writes no
longer have to.

**The transaction number is this build's, not stock Android's**, so it was
measured rather than derived: `setStreamMute` sits at 8 where AOSP 5.1 has
`isStreamMute`, so at least one method is inserted and the ordering cannot be
read off upstream. Confirmed at 4 on all three Dots, all FireOS 5.5.5.4, and
there is no fallback for a build that does not share it -- the same bargain
`micMuteCall` already makes, on a device whose updater is deliberately blocked.

What there is instead is a read back: a set whose level does not arrive is
reported with the level the device kept, rather than assumed. docs/hardware.md
carries why probing for that number is dangerous.

Safe media volume does not bite, which was measured rather than assumed. The
dump says `SAFE_MEDIA_VOLUME_ACTIVE` with `mSafeMediaVolumeIndex=270`, step 27
of 30, and the active route was `headset` -- every precondition AOSP blocks on
-- and a set to 30 landed anyway. What was not checked is whether the gain is
capped downstream of the index.

**The level that is read is the live route's.** A relative step is computed
from wherever the device is, so the level to start from is the socket's when
something is in it and the speaker's otherwise, which is what `JackOccupied`
answers. When the live route has no readable level, **or when which route is
live cannot be read at all**, nothing is set: a level computed from an unknown
one is a guess, and the reading that follows makes it look deliberate. The
switch this reads, `/sys/class/switch/h2w/state`, has never been seen to fail,
and the jack sensor beside it goes missing when it does, so the two agree about
what is unknown.

**A set is a call and a read back**, in the shape the microphone
switch already uses: one worker, one pending request, and `liveWake` at the end
so the poll republishes rather than the worker inventing a state. Two requests
arriving together coalesce, and two *relative* ones add rather than replace, so
a double tap on volume-up moves two steps. A step arriving on a pending *set* is
added to it, and a set arriving on a pending step replaces it. Those are not the
same rule twice, and they are not meant to be: a step says which way to go from
wherever you are, so it belongs on top of whatever is about to happen, while a
set names where to be, so it answers a step rather than landing one above it.
Half and then one more is 51.7%; one more and then half is half, which is what
the second request asked for. There is no settle to wait out: the level reads
back current in the same breath as the call.

**The slider shows the step, and the mute is a flag beside it.** The two volume
sensors report a muted stream as zero percent, because zero is what can be
heard. The media player cannot: a step counts from that number, so a slider
fed the zeroed percentage would sit at 0 while the level it counts from was 12
of 30, and every set would land on the device and snap back in Home Assistant --
then do nothing at all the second time, because the step is already where it was
asked for. So the volume field carries `step / max`, and read and write count
from the same number by construction.

**The mute is a control now, not only a reading.** `VOLUME_MUTE` joins the
feature flags when there is a mute to set; `MUTE` and `UNMUTE` are commands 3
and 4. They share the volume's worker shape -- one pending state, the last one
winning, `liveWake` at the end -- so a mute and an unmute arriving together
leave the Dot unmuted.

**Holding the mute means holding the volume keys.** `setStreamMute` is
transaction 8 and takes, but a press while it is set does not lift it: measured,
muted at step 5, one press left the mute set and the level at **1**, because the
muted index reads 0 and the press moves up from there. So `/dev/input/event2` is
grabbed for as long as the mute is -- it carries those two keycodes and nothing
else -- and the first press lifts the mute and moves nothing. Measured: 29
presses swallowed, level unmoved. A daemon that cannot take the grab mutes
anyway and says so. What it costs is Alexa's advanced factory reset, mute and
volume down together, which cannot complete while muted; docs/hardware.md has
the gesture.

The mute travels beside it as its own flag, read from `Mute count:` rather than
recovered from a percentage that happens to be zero. Those two are not the same
question: a level stepped all the way down is also zero percent, and reporting
*that* as muted puts Home Assistant's mute indicator on a level the mute control
cannot lift, since stepping to zero is not what `Mute count` reports.
`MusicVolume` carries
the flag for that reason. Nothing on this Dot has ever produced the mute it
separates, which is the reason to pin both halves in tests rather than leave
them agreeing by accident.

**The media player carries no missing flag.** `MediaPlayerStateResponse` has a
key, a state, a volume and a muted bool, and nothing that says "unknown", where
every sensor here has one. So a level that cannot be read publishes nothing and
Home Assistant keeps the last value it was told, rather than taking a zero that
reads as silence. The two volume sensors beside it do carry the flag and do go
missing, which is where to look when they and the slider disagree.

**Playing a URL is Alexa's job, and the state comes back from her.** The
`media_url` a peer sends goes to `internal/alexa` untouched: whatever it points
at is her question, not ours, because nothing here decodes an mp3. What the
daemon adds is the two checks the intent forces -- `http://` only, and no comma
or double quote, because the extras are a comma-separated array and the payload
is hand-built JSON -- and a directive id per call, so two clips in flight are
two directives rather than one confused one.

The state follows the speaker rather than the request. A peer asking to play
does not make the entity say playing: `PlaybackWatcher` tails
`logcat -s tts-Server tts-Playback` and reports `Playback started:` and
`Playback ended:` for the `SpeechSynthesizer` namespace, and only those move the
state. Alexa speaks for her own reasons all day, so the watcher is armed by a
request and disarmed by the end it was waiting for; a playback nobody asked for
is hers. The optimistic alternative -- say playing the moment a peer asks -- is
wrong in the direction that lasts: a clip she never plays would leave the entity
playing until something else moved it, where this one reports what was heard and
goes back to idle on its own.

It goes back to idle even when she says nothing at all. The watcher carries a
deadline and gives up at it, because the failure that matters most here is
silence: a 404 and a variable-bitrate mp3 both end `FAILED` with a line worth
keeping, but a stack that is not running answers neither way. The deadline is
extended once `am` has returned, because the intent carries a timeout of its own
and a slow one would otherwise eat the budget meant for her fetch -- extended
rather than re-armed, because a clip that failed inside that second or so has
already been reported, and arming a fresh watch over it invents a timeout for a
playback that is finished and throws away the line that said why.

**The deadline is for starting, not for playing.** Thirty seconds is a generous
wait for her to fetch a clip and begin it, and a ludicrous cap on the clip
itself: a ninety-second recording would otherwise be called a failure while it
was still audible, drop the entity to idle, and then have its real `SUCCESS`
discarded because the watch was already disarmed. So `Playback started:` moves
the deadline out to fifteen minutes -- longer than anything anybody hands a Dot,
and short enough that a playback she starts and never reports the end of does not
own the entity for the rest of the run.

**What the watcher cannot do is tell her clip from ours.** Measured on a Dot,
the lines are `Playback started: uid(32037)_id(0)_namespace(SpeechSynthesizer)`
and the matching `ended`, and the directive id this daemon minted is in neither
of them -- it appears under `SPCH-SIM_SimJobStack` instead, which is a different
tag and a different line. So the watcher matches a namespace, not a directive,
and a reply to a wake word in the seconds after a request is a playback it will
report as ours. Arming on request and disarming on the first end narrows that to
the window a clip was asked for, and nothing here closes it further. A
correlation would need the id out of the other tag, tied to the `id(N)` these
lines carry, which is two more moving parts for a case nobody has hit.

The hook itself is taken under the lock rather than read where it is used, which
matters only because the check that installs it now runs in the background: a
field written by one goroutine and read by another is a race whatever the timing
makes of it, and the timing here is a package manager answering at its own pace
against a peer that can ask to play at any moment.

**One clip plays at a time.** A play goes through a worker with one pending
request, the shape the volume set and the microphone switch already use. Each
one forks `am`, which is a fresh VM on a device with 512 MB, so a peer sending
play commands in a loop would otherwise have as many of those running at once as
it cared to start. It is also the half of the overlap problem that *is* ours to
fix: two clips in flight would report through one watcher whatever the
namespace matching did.

**A url is a peer's string**, so it is cut to the same 64 bytes every other peer
string is cut to before it reaches the log -- in the error `CheckURL` builds as
well as in the line the connection notes, because a rejected url is logged by
the first and never reaches the second. What `am` prints is cut on the same
rule: a java stack trace from a failed `startservice` is kilobytes, and it is a
peer that decides how often one happens. The reason Alexa gives for a failure is
cut with them. Playback is peer-triggered from end to end, so the line
that reports it goes through the rate-limited log rather than `log.Printf` in
`serve.go`: a peer that can ask for a clip a second can otherwise write to
`/data` a second, which is the hazard docs/pitfalls.md opens with.

**Nothing to set with, no control.** The feature flags are computed rather than
fixed, and Sendspin reads the same answer through `CanSetVolume`, so the two
surfaces cannot disagree about whether this Dot's level can be changed. The
computation has one answer today: the setter is wired unconditionally, where
the uinput device it replaced could fail on a missing `/dev/uinput` and leave
the entity listed with no slider. What is lost with it is finding out at wiring
time -- a `service` that cannot run now advertises a slider and reports the
failure per set. `PLAY_MEDIA` and
`MEDIA_ANNOUNCE` are computed the same way and for the same reason, so a daemon
that cannot reach Alexa's synthesizer offers no play button rather than one that
fails quietly.

Whether she is there is pm's answer rather than a file's, and it is asked in the
background rather than once in front of the listener. A debloated Dot keeps the
apk and loses the package, so a stat would say yes where nothing can be reached;
and `pm` early in a boot is a package manager still settling, so a single answer
at startup can be a no that lasts the whole run. It is asked every thirty
seconds for four and a half minutes and latched on the first yes, which is the
same rule the firewall rule is re-asserted under: setup that depends on the
device waits or repeats, never runs once. A daemon that starts before she does
logs the wait and logs the answer when it comes.

Arriving late is not enough on its own, because `ListEntities` is answered once
per connection and Home Assistant holds what it was told. The ordinary cold boot
is exactly that order -- the daemon starts, Home Assistant reconnects and lists
while `pm` is still settling, and the answer lands half a minute later -- so the
play button would be missing for the life of that socket with nothing saying
why. Installing the hook where there was none therefore drops the connections
that have one, and they list again on reconnect. It happens once in a run, and
the log says which peers were dropped and why.

**A watcher that cannot start says so once.** `logcat` is a process, and a
process that will not start will not start again ten seconds later either. The
retry loop reports the first failure and then goes quiet, the way the firewall
re-assert does and for the same reason: a line every ten seconds is six a minute
into a log that is truncated at boot and every twentieth restart, and neither of
those arrives while the daemon is happily running. A tail that ran for a while
before it failed is a new fault rather than the one already reported, so that
one is said again.

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
whole of one read is 1 second plus 0.5, and `VolumeReadBudget` is exported as
the sum: a hundred and twenty-eight times what the call measures at, and inside
the two and a half second tick that made it. `main_test.go` holds those two
together, and it has to hold the sum, because a test against the deadline alone
passes while the real worst case runs over.

One read fitting is not the bound either, and that is what the deadline came
down from 1.5 seconds for. The poll is serial, so a heavy tick spends its forks
one after another: the volume, the speaker's track, and now the microphone. Each
was inside the interval on its own while the sum was not -- two of them already
came to 2.4 seconds against 2.5, and a third would have put the worst case past
the tick that made it. So `main_test.go` holds the sum of the three rather than
each apart, and the volume gave up half a second it had no measurement to
justify: 11.7ms is what the call takes, and the second it now has is generous by
two orders of magnitude.

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

`DeviceInfoResponse` carries a `project_name` of `Amazon.Echo Dot (2nd
Generation)` and a `project_version` of the tag the binary was built from, and
between them they are what Home Assistant shows as the firmware version:
`v1.0.0 (ESPHome 2026.8.0)`. A build with no tag says `unversioned` rather than
nothing, because the field renders whether or not it is empty.

The dot in that name is load-bearing, and not ours. Home Assistant derives the
manufacturer and the model from `project_name.split(".")` when one is sent,
taking `[0]` and `[1]`, so a name without a dot raises an `IndexError` inside
the integration rather than failing here. Composing it from the manufacturer and
model already sent is what keeps the device page identical to what it showed
before the version was added, and a test asserts the split gives back those two
fields rather than asserting the literal. A second test asserts the model
`serve.go` sends carries no dot of its own, because one there would not fail the
split -- it would quietly take `[1]` as the fragment before it and show a
truncated model.

`esphome_version` remains `2026.8.0`. It names a real ESPHome
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

**A switch that turns another surface off.** `switch.<name>_sendspin` is the one
entity that changes something outside the API: it takes the whole Sendspin
surface down, and docs/sendspin.md says what that means. Three things about it
belong here.

It is optional. `UseSendspin` is what creates it, so a Dot that could not read
its Sendspin identity lists no such switch instead of listing one that cannot do
anything. The Alexa command box is gated the same way; the network-adb select is
not, because adb is always available and only its `Secure` option is conditional.

Its setter must not block, and the reason is not politeness. Turning Sendspin off
calls `iptables`, which waits on the xtables lock netd holds constantly, and the
goroutine answering a `SwitchCommandRequest` is the one that reads every other
frame from that connection. So the seam is a `func(bool)` that hands the request
to a worker and returns, and the entity reports what the worker achieved rather
than what it was asked for.

It wakes `liveWake` when the worker is done, for the reason the microphone does.
The reading is in `readLive`, which the poll takes on `HeavyEvery` ticks rather
than every tick -- two and a half seconds, not the half second of `liveTick` --
and the worker outlives that anyway, because `DenyTCP` waits on the xtables lock.
Without the wake the switch would sit in its old state for seconds after the
operator moved it, which reads as the control being broken.

`UseSendspin` **writes** the two func fields without the server lock, the way the
other `Use` methods do: it runs during wiring, before `Poll` and `Listen`, so no
reader exists yet. The reads afterwards are all under the lock -- `listEntities`
and the switch-command path hold it, and `readLive` is the one exception. What
must not happen is taking the lock to read them from inside the command path,
which already holds it: that deadlocks, and the suite hangs rather than failing.

**A number, for the one setting that is a figure.** `number.<name>_sendspin_output_delay`
is the Sendspin output delay, the milliseconds this player is asked to place its
audio earlier than a server stamped it. docs/sendspin.md carries what the figure
means on the wire, that a server can set it too, and which writer wins. What
belongs here is the entity.

`ListEntitiesNumberResponse` is 49, `NumberStateResponse` 50 and
`NumberCommandRequest` 51, and the fields are per message again: on the listing
the icon is 5, the range 6 and 7, the step 8, `disabled_by_default` 9, the
category 10, the unit 11 and the mode 12, where a select carries its options at
6 and its category at 8. The state and the command both put the figure in field
2 as a `float`. API version 1.12 already covers it: numbers are older than that
and `aioesphomeapi` reads whichever entity messages arrive rather than gating
them on the version.

It is optional the way the switch is. `UseSendspinDelay` is what creates it, so a
Dot with no Sendspin identity lists no delay either, and the maximum arrives from
the caller rather than being written down twice: `serve.go` hands over
`sendspin.MaxStaticDelayMS`, which is the bound a server's own figure is held to,
so the control cannot offer one the thing behind it would clamp.

**The mode is the box rather than the slider, which is the whole reason the step
is 1 ms.** A slider sends a value per step dragged, and every value is a property
write -- two forks -- and a `client/state` to whatever server is playing. Music
Assistant's own control is one: 23 values from a single drag, measured on a Dot,
which spent the peer log's whole minute. A box sends one figure when committed,
so a fine step costs nothing and the wire carries whole milliseconds anyway.
Home Assistant would have drawn a box at this step regardless -- its frontend
gives up on sliders past 256 steps -- so declaring the mode only stops the
control depending on that heuristic. There is no `device_class`: `duration` is
the only candidate and Home Assistant's units for it stop at seconds, so the unit
says `ms` and the icon carries the identification.

Its setter must not block either, and for the same class of reason as the
switch's: persisting the figure is a `setprop` and a `getprop` read-back, two
forks, on the goroutine that reads every other frame from that connection. So the
seam is a `func(int)` handed to a worker rather than applied inline.

It is the **switch's** worker. A figure and a switch toggle on two workers can
interleave, and the figure then lands on a switch an `enable` is at that moment
giving a client -- persisted, and never told to the client that is playing. One
worker cannot do that. What it costs is a figure queued behind an `enable`,
waiting out the xtables lock: late is recoverable and silently ignored is not.

With Sendspin off, a set wakes `liveWake` when the figure is taken, so a control
does not sit at its old value while the window holds the write back; the write
wakes it again when it lands. Two wakes for one set costs an extra poll and the
count is not worth defending -- what matters is that the first one does not wait
on flash. A write that fails wakes nothing, and the control is already showing
the figure by then. The reading is in `readLive`, which the poll takes on
`HeavyEvery` ticks. With a client up there is no wake of its own:
the figure goes to the client and the property write is the keeper's, so the
entity follows within `liveTick` times `HeavyEvery`, 2.5 s, from reading the
client rather than from being told.

**A peer's figure is read as a float or not at all.** Field 2 sent as a varint is
not a float that happens to be zero, and neither is a `NaN`: taking either as
zero would move a playing stream by whatever the delay was, on a malformed frame.
The figure is rounded and held to 0 through the maximum before it goes anywhere,
because the range is Sendspin's own and one outside it is refused there.

**A field that is absent is not that case, and reading it as one made 0
unsendable.** proto3 leaves a zero-valued scalar off the wire, so a command for 0
carries its key and nothing else -- the bottom of the range the entity itself
offers was the one figure the control could not send. Found on a Dot rather than
here: the log has the 1 ms and the 2 ms either side of it and no line at all for
the 0, because every test built the frame by writing the field. Absent now reads
as zero; a field present with the wrong wire type is still refused.

The media player's volume is the same rule and had the same defect, with
`has_volume` separating the two readings there: a slider at exactly 0.0 sends the
flag and omits the figure, so mute was dropped the same way.

A command carrying a figure the delay already holds is neither passed on nor
logged, which is the button mode's rule. It is refused as a repeat only when it
matches both what is applied and what was last asked for: the entity reports what
has been **applied**, and a figure queued behind a busy worker is not applied
yet, so comparing against the applied figure alone dropped an operator's second
command and let the first one win.

**An entity is named three times over, and the three are not interchangeable.**
Each listing carries an `object_id`, a `key`, and a name. Home Assistant builds
the entity id from the **name** -- `_attr_has_entity_name` is set on its side --
so `number.<name>_sendspin_output_delay` comes from "Sendspin output delay" and
not from the `object_id` beside it. The `key` is what every state message is
routed by, so it is the one that must never move for an entity that already
exists. The `object_id` is the one with the least reach and the easiest trap: it
is part of the older form of the unique id Home Assistant stores, so changing it
on an entity somebody already has can re-identify that entity even though its id
and its key are untouched. This tree keeps `object_id` as the slug of the name,
which makes all three agree and the question not come up.

`NumberStateResponse` carries a `missing_state` like a sensor's and it is always
false, which is the one place this entity differs from everything else on the
list. Every other reading here is taken off the device and can fail; this one is
a setting the daemon holds, so there is always an answer. What it reports is the
figure actually applied rather than the last one Home Assistant asked for, which
is what makes a server moving the delay show up in the control rather than the
two disagreeing silently.

## Discovery

Home Assistant finds the Dot over mDNS. The responder is `internal/mdns`, which
is not ESPHome's: it answers for every service this device offers, and ESPHome
supplies its own advert through `esphome.Advert`. `docs/mdns.md` carries the
responder, its RFC divergences and the traps.

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
