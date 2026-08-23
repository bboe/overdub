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
prove it has the key holds a slot for ten seconds rather than ninety. Finishing
the handshake is not that proof. Message 1 is sealed under the key, so a peer
cannot compose one without it, but nothing fresh from this side goes into it, so
a captured one replays verbatim and Noise will not refuse it. What a replayer
cannot do is send a frame that decrypts, and eight of them would otherwise hold
every slot for ninety seconds apiece on a captured message and no key. Ten
seconds is one budget rather than one per read, for the same reason: a peer that
stalled the handshake to just inside its deadline would otherwise be given a
second full wait to send that first frame in.

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

The ninety-second idle deadline, the sixty-second sensor push and the client's
own keepalive are one chain rather than three separate numbers. The deadline is
a read deadline and this daemon never pings, so on an otherwise quiet connection
the only thing that satisfies it is Home Assistant's keepalive, and
`aioesphomeapi` cancels that ping on *any* message from the device, our own
state pushes included. Push more often than the client waits and it stops
pinging altogether, and the deadline then expires on a connection that is
perfectly healthy. Measured against the real client: a five-second tick drops
Home Assistant at exactly ninety seconds, over and over, with a reconnect each
time. `MinSensorTick` is where that constraint is written down, and
`PollSensors` raises anything under it to the floor rather than trusting its
caller, because the constant is exported and the cost of getting it wrong lands
on a connection that is working.

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
