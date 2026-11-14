# Running a command as though it were spoken

`internal/alexa/command.go`, `internal/device/mapdb.go`, and the text entity
and user service in `internal/esphome`. Off only while the Dot is registered
to no Amazon account; then the entity is never advertised.

## Why the cloud, when the Dot is right here

- Audio cannot be injected into Alexa on this device. On Fire OS 5
  `amazon.speech.sim` fired the wake word and opened a real cloud session, but
  only the detector heard the file. The upload still came from the live
  microphone.
- "Alexa" plus 8 seconds of silence, and "Alexa, play dance party music on
  Amazon Music", both ended at about 1.1 seconds (1.16 and 1.11), because the
  command audio never arrived.
- `SpeechRecognizer_ExpectSpeech` did not help: it opened 0.79 seconds into
  the injected speech and ended 199 ms later.
- The `avs-device-sdk` recipe does not transfer: there the injector replaces
  the microphone the whole SDK reads from.
- So a command goes through `/api/behaviors/preview` as `Alexa.TextCommand`.
  That is the Alexa app's own endpoint, and the only layer that takes text.
  The cloud then runs the command inside the Dot's own session, and the Dot
  answers out loud as it does to speech.
- The Dot is found by `ro.serialno` in the account's device list, so the
  command lands on this Echo, not on the first one listed.

## Why not the Dot's own gateway

Measured 2026-11-14 from the Dot, with its own identity.

- The Dot's AVS gateway, `alexa.na.gateway.devices.a2z.com/v20160207/events`,
  takes the Dot's access token: a `System.SynchronizeState` event answered
  `204`. The token comes from `POST api.amazon.com/auth/token` with the
  `refresh_token`, `reg_app_name` and `reg_app_version` the store holds,
  which is the call the Dot's own MAP library makes.
- No text event is accepted there. `Alexa.Input.Text` with `TextMessage`,
  `TextInput`, `Recognize` and `SendText`, under several payload shapes, each
  answered `400 INVALID_REQUEST_EXCEPTION "Bad Request"`. So did an invented
  namespace and a `SpeechRecognizer.Recognize` with no audio: the error does
  not tell an unknown interface from a wrong payload, so there is nothing to
  iterate against, and nothing public documents the event. The name appears
  only in the capability lists app emulators register.
- It would not make the Dot speak anyway. AVS answers an event on the
  connection that sent it, not on the Dot's downchannel, so the reply would
  arrive here as MP3, which this daemon does not decode.

## The credential

- On Fire OS 6 the credential is in the clear. `/data/ace/kvstorage/map.db`
  (`ace_maplite`, `0660`) is an ordinary SQLite file whose one table is
  `deviceData (key TEXT PRIMARY KEY, value BLOB NOT NULL)`. `refresh_token`
  is 353 characters starting `Atnr|`; `access_token`, `adp_token` and
  `device_private_key` sit beside it, also in the clear. Root reads it, and
  the daemon is root. biscuit, radar and donut all hold it there.
- Fire OS 5 kept it encrypted in MAP's Android store, and only MAP's own uid
  could decrypt it. That is what `MapDump.java` was for: a jar run through
  `app_process` as uid 32051. Fire OS 6 has neither `app_process` nor that
  store, so the jar, its build, `/data/local/map` and the `su` lookup are
  gone.
- `internal/device/mapdb.go` reads the file itself: the header, the table's
  root page out of `sqlite_master`, the interior and leaf pages under it, and
  the overflow chain of any value longer than a page holds. The store is
  1 KB pages, its table root is an interior page, and two values overflow,
  so every path runs on the real file. The fixtures under
  `internal/device/testdata/` have the same shape and are built by `mapdb.py`
  there; `mapdb_test.go` also truncates the registered fixture at every
  length and corrupts it at every offset, and the reader must return an
  error and never panic.
- No SQLite library. The device's `libsqlite.so` would need cgo and could be
  tested only on the Dot; the reader is a few hundred lines that fixtures
  check in isolation, which is the rule in constraints.md.
- The file is read twice, 50 ms apart, and used only when both reads agree
  and no hot `map.db-journal` is beside it. Alexa rewrites the store about
  once an hour when she refreshes the access token, and a read that landed
  mid-write would hand half a token to Amazon as the credential. A read that
  does not settle in 5 tries is an error, not a reading.
- A journal is hot when its header carries SQLite's magic. Its size says
  nothing: PERSIST mode leaves the file after every commit with the header
  zeroed, and TRUNCATE mode leaves it empty. The Dots run DELETE mode, with
  no journal file between writes, so the check matters only if that changes.
- The header must say rollback-journal mode (bytes 18 and 19 are 1) and
  UTF-8. A WAL store keeps committed rows in `map.db-wal`, so the main file
  lags; a UTF-16 store would match no key and read as unregistered. Either is
  no reading. The file's size is checked before it is read, 16 MB at most.
- The read costs two file reads and the 50 ms gap. That is on `serveAPI`'s
  cold-boot path, ahead of the firewall rule and the responder; the
  `dumpsys account` read it replaced took 1.5 s and was gated behind the jar
  stat for that reason. 50 ms is not.
- The value must have no whitespace and be 16 to 4,096 bytes before it is
  used, as MapDump's output had to be. Otherwise a wrong key or a damaged row
  is posted to `api.amazon.com` and comes back as an opaque 400.
- The token is held in memory for the life of the process, and a refusal
  drops it and reads the store again, once. A refusal is a 400, 401 or 403, or
  an authenticated call that answers without what it should: a stale session
  can answer 200 with a sign-in page or an empty device list, and both count.
- The token never reaches the log. Error bodies that could carry it are
  withheld, and the cookies it buys are struck out of every error reported.

## What the token buys

- `exchangeCookies` asks for `.amazon.com`, as the Alexa app does. The reply
  is `at-main`, `sess-at-main`, `session-id`, `session-token`, `ubid-main` and
  `x-main`: a signed-in amazon.com retail session, not an Alexa-scoped one.
- Asking for less does not get less. `.alexa.amazon.com` returns the same six
  cookies, keyed `.amazon.com`, and the command still works. The `Domain` this
  code stamps binds nothing at Amazon's end.
- So the feature's scope is the whole account, and there is no opt-in: every
  registered Dot offers the box. Release installs already did, because the
  tarball carried the jar and `install.py` installed it whenever present. The
  only lever is the registration. SECURITY.md and README.md say so.

## The headers

Measured 2026-11-14 on overdub-test, one command per variant.

- Nothing on the Dot sends a `User-Agent`. `libace_connectivity_manager.so`
  embeds the header set of every endpoint the device's native stack calls:
  `api.amazon.com/auth/token` gets `Content-Type` alone, MAP's other calls get
  `accept-language`, `content-type`, `authorization` and the `x-adp-*`
  signature headers, and the AVS gateway gets `authorization: Bearer`. A scan
  of every binary and library found an agent string only in `sntp`. So there
  is no device agent to copy, and none to fetch per model.
- `api.amazon.com` accepts the device's shape. The cookie exchange with no
  `User-Agent`, no `x-amzn-identity-auth-domain` and no `di.*` fields
  returned the six cookies and the command ran. `app_name` is the one field
  Amazon insists on: without it, `400 MissingValue: The request is missing a
  required parameter : app_name`. So the exchange sends `Content-Type`, and
  the form `app_name`, `source_token`, `source_token_type`,
  `requested_token_type` and `domain`.
- `alexa.amazon.com` does not. With no `User-Agent`, `/api/language` answers
  without the `csrf` cookie and nothing after it can run. Nothing on the Dot
  ever talks to that host; its client is the Alexa app, so the daemon sends
  the app's set, taken from
  [alexa-cookie](https://github.com/Apollon77/alexa-cookie), which copies the
  iOS app: `AmazonWebView/Amazon Alexa/2.2.651540.0/iOS/18.3.1/iPhone`,
  `Accept: application/json`, `Accept-Language`, `Accept-Charset: utf-8` and
  `Connection: keep-alive`. The old agent was that shape with the Dot's model
  and an invented version, which no client ever sent.

## What the daemon offers

- A text entity `alexa_command` and a user service `send_command`: one for a
  person on the device page, one for an automation. Both end in
  `commandLocked`, and both are listed only once `UseCommand` has a sender.
- A send can take minutes: each of the 4 HTTP requests has 30 seconds, and an
  auth failure runs the attempt twice. So a worker sends, never the read loop.
- Commands queue 4 deep, and the fifth is refused and logged. Unlike playback,
  the latest does not replace the rest: two commands are two requests, and an
  automation that sets a timer then turns on a lamp must not lose the timer.
  The bound stops a peer with the key from queuing without limit.
- The worker echoes each command into the entity's state, so the box shows the
  last command and a later subscriber sees the same.
- The box starts with an empty state. Home Assistant draws a text entity that
  never had a state as *unavailable*, and an unavailable box cannot be typed
  into, so it would stay grey for ever.
- A command is peer text, so the log cuts it to the peer-string length. The
  failure line is not cut again: the reason is the daemon's own, and `clip`
  already bounds every quoted body at 300 bytes. A 64-byte cut would end
  mid-reason. The retry reports only the second refusal. Both lines go through
  the peer rate limit.
- Empty text is published but not sent. Home Assistant clears the box by
  writing an empty string; keeping the last command would snap the card back
  to text somebody just deleted.
- The clear rides the queue. A clear judged against the published state while
  a command is queued sees the state *before* that command, is dropped as
  redundant, and the command it meant to erase then overwrites it.
- Text longer than `commandMaxLength` is refused, not trimmed. The listing
  binds only Home Assistant's field; a peer with the key can send a whole
  frame, and every byte would reach the recorder and Amazon. The check is at
  both doors and counts runes, as Home Assistant does: 200 CJK characters pass
  its field and are 600 bytes.

## Registration

- The credential *is* the registration. `device.AlexaRegistered` reads the
  same store: a `refresh_token` row is registered; a store without the row,
  without the table or empty is not; and a store that is missing, cannot be
  read or does not parse is no reading. Every Dot seen has the file,
  registered or not: the ACE stack creates its stores at first boot, as the
  0-byte ones beside it show. So a missing file is a Dot in a state nobody
  measured, and the box fails open there as the old reading did when its
  service was not up. `binary_sensor.<name>_alexa_registered` reports it;
  docs/api.md has the tick.
- Fire OS 5 read `dumpsys account`. Fire OS 6 has no account service: the
  call answers `Can't find service: account`, so on it the old reading was
  never taken and the sensor stayed unknown.
- `commandReady` fails open on the reading. "Not registered" hides the box; a
  reading that could not be taken does not. That is the one case where the box
  is offered and still fails, and the failure names the store.
- Without the check, an unregistered Dot offers a box whose every command
  fails with `token: /data/ace/kvstorage/map.db holds no refresh_token`.
- A Dot with no account is asked again every 5 minutes, and stops once it has
  one. It costs two reads of a 10 KB file.
- Offering the box late drops every connection, as `UsePlay` does, because
  Home Assistant reads the entity list once per connection.
- A Dot deregistered while the daemon runs keeps a box that fails.

## Setup mode

- Registration is Alexa's own process: set the button to `pass through`, hold
  it until the ring turns orange, and add the device in the Alexa app.
- Nothing here runs it, but on Fire OS 5 this intent started setup mode
  without the button:

```sh
am startservice -a com.amazon.device.oobe.services.action.SETUP_MODE_ON \
  -n com.amazon.echo.csm.oobe/.services.SonarOOBEMainService --ei reason 2
```

- `reason` must be 2, `OOBE_ENABLED_BUTTON_PRESS`. Without it,
  `logcat -s AmazonOOBE.SonarOOBEMainService` reports
  `Start/Stop Reason = 0` and setup mode does not start.
- Setup mode shows as an address on `p2p0`, the Dot's own network. It takes
  the Dot off the LAN.
