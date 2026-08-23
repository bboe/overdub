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

Only a `HelloRequest` buys the longer read deadline, so a peer that says nothing
useful holds a slot for ten seconds rather than ninety. Eight peers can still
hold every slot by sending one hello each and renewing it, and nothing evicts
the oldest connection. SECURITY.md says the same of the port: bounded rather
than guarded.

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
