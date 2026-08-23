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
subscribes for the first time. Zero is a plausible value for both of them --
zero dBm is a signal level and zero seconds an uptime -- so a reading that could
not be taken cannot be published as one, which the paragraph on `missing_state`
below picks up.

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

Both readings share one published state, and that is the point rather than a
convenience. It is what every subscriber has been told and the only thing any of
them is ever told: the poll reads the device, and a reading that differs from
what is published replaces it and goes to every subscriber. A reading equal to
the published one sends nothing at all, so a signal that has not moved costs the
read and no traffic. A client that has just arrived is answered from that state
rather than from a reading of its own.

A second reader is what makes that necessary. If the snapshot answering
`SubscribeStatesRequest` took its own reading, it could tell one client a value
the poll never saw, and the poll would then find the device back at the value it
remembered and stay quiet -- leaving that client on a number nothing would ever
correct. It needs no failure of any kind: a reading that moves and moves back
inside one tick, with a client subscribing in between, is enough. A tick that
carried every reading whether or not it had changed would heal that within the
minute, which is what these two had while every tick was a full push. One reader
instead of two is what makes the tick free to carry only what changed.

So the snapshot is sent from `handle`, under the lock that orders it against a
concurrent push, and it reads nothing. That is also why `publish` collects what
it could not send and logs after the lock is dropped rather than under it.

Subscribing wakes the poll instead. The value the snapshot just answered with is
up to a whole tick old, which is a minute here, so a subscriber that took no
reading and got no wake would sit on it for that long. Only the first
`SubscribeStatesRequest` on a connection wakes anything, because a peer holding
the key can send them as fast as it likes and each one after the first would
otherwise be a reading of the device it asked for and got.

That wake is sent with the server lock held, so it is a send that may be
dropped rather than one that may block. A bare send would deadlock the server
outright: `handle` holds the lock for its whole body, and the poll that would
empty the channel takes that same lock to publish.

`MinSensorTick` bounds the ticker rather than the push rate, which the wake
exceeds by a read per connection that subscribes.

The poll is now the only thing that reads these two, which is the cost of there
being one reader rather than two. A poll that wedged would leave every
subscriber on the last published values, including ones that connect
afterwards, where before this each new subscriber at least took a reading of its
own. Both readings come from procfs, which is why that is a trade worth making
here and not one to repeat for a reading that forks a process.

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
flight each, and the product has to fit a 256 MB device.

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
