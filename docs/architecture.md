# Architecture

## Button interception

`internal/button` over `internal/evdev`. `event1` carries the action button
*and* mute, so an exclusive `EVIOCGRAB` takes both. The fix is a `uinput` clone
advertising exactly the real key bitmap, named `mtk-kpd` so Android applies the
same keylayout; 138 is consumed and the rest re-emitted. `EventHub` picks the
clone up by inotify. Read the bitmap with `EVIOCGBIT`, never from sysfs: that
file's word size differs from `/proc/bus/input/devices` here, and guessing wrong
silently breaks mute.

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
scraped out of a message we could not understand. It is the only message whose
payload is read at all: the rest carry nothing this daemon needs, so their
bodies are never looked at.

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
class implies. The icon fields are not the same number either, 5 on a sensor and
8 on a binary sensor, which is the field-numbers-are-per-message rule once more.

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
wake exceeds by a read per connection that subscribes.

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

## Speech

`internal/alexa/say.go` hands a URL to Alexa's `SpeechSynthesizer` via `am
startservice`, so she mixes and ducks it like her own speech. Writing PCM is not
an option: `mediaserver` holds card 0 device 23 permanently, so it contends and
stutters.

`/system/priv-app/SpeechInteractionManager` enforces four things:

1. **Two parsers read the same intent** and want different shapes. One throws
   `IllegalArgumentException: Namespace null` without singular
   `namespace`/`name`/`payload`; the other needs the string arrays
   `namespaces`/`names`/`payloads`/`payloadVersions`. Send both.
2. **This build's `am` does not unescape `\,` in `--esa`.** A JSON payload with
   commas arrives as several broken array elements and fails with `Invalid
   JSON`, so the array payload carries only `{"url":"..."}`.
3. **`sequenceId` is required** alongside `directiveId`, both plain string
   extras. Omit either and the play throws a bare `IllegalArgumentException`,
   logged as `Unable to play` and naming no field, while `am` still exits 0.
   The values are not keys: the same pair sent twice plays twice, because
   `SpeechInteractionManager` mints its own queue id. They are still worth
   choosing. `directiveId` carries the pid and `sequenceId` counts presses
   within it, and both appear verbatim in the `SPCH-SIM_*` and
   `SpeechSynthesizerAgent-TtsEventListener` lines, which is the only way to
   follow one press through to its `onSpeechEnded`.
4. **The URL must be `http://`.** `file://` fails with `ClassCastException:
   FileURLConnection cannot be cast to HttpURLConnection`, because the code
   casts unconditionally.

The clip is served from a loopback listener that lives as long as the daemon.
If it ever stops the daemon exits, because nothing else would notice: the URL
would still be handed over, `am` would still exit 0, and every press for the
rest of the boot would be logged and silent. Exiting hands the supervisor a
restart, and the kernel drops the grab and the clone with the descriptors.

`am` exits 0 whether or not it found the service, printing `Error: Not found; no
service started.` and returning zero, so its output is read as well as its
status. That is exactly the state a suppressed Alexa stack leaves behind, and
without the check the play reads as accepted, and what surfaces is the device's
own two-minute timeout rather than a reason.

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
