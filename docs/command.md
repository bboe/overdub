# Running a command as though it were spoken

`internal/alexa/command.go`, `deploy/mapdump/`, and the text entity and user
service in `internal/esphome`. Off unless `mapdump.jar` is installed *and* the
Dot is registered to an Amazon account; without either, the entity is never
advertised.

## Why the cloud, when the Dot is right here

- Audio cannot be injected into Alexa on this device. `amazon.speech.sim` fires
  the wake word and opens a real cloud session, but only the detector hears the
  file. The upload still comes from the live microphone.
- "Alexa" plus 8 seconds of silence, and "Alexa, play dance party music on
  Amazon Music", both end at about 1.1 seconds (1.16 and 1.11), because the
  command audio never arrives.
- `SpeechRecognizer_ExpectSpeech` does not help: it opened 0.79 seconds into
  the injected speech and ended 199 ms later.
- The `avs-device-sdk` recipe does not transfer: there the injector replaces
  the microphone the whole SDK reads from.
- So a command goes through `/api/behaviors/preview` as `Alexa.TextCommand`.
  That is the Alexa app's own endpoint, and the only layer that takes text.
- The Dot is found by `ro.serialno` in the account's device list, so the
  command lands on this Echo, not on the first one listed.

## The credential

- MAP's store is encrypted at rest, so scanning `map_data_storage_v2.db`
  yields nothing. Its gate is *identity*, not a signature: `MapDump.java` runs
  as MAP's uid and asks MAP's own storage class to decrypt. No crypto is
  reimplemented.
- The wake word's permissions are `signature|system` and Android enforces
  them, so a `/system/priv-app` install gets them. MAP's gate is not Android's,
  so a privileged install buys nothing there.
- Root reaches the store too: `su -c` and `su 32051 -c` both report
  `accounts: 1` and return the same 353 characters. The daemon uses 32051
  because it is the least privilege that works, and a root shell has no reason
  to hold an account credential.
- The older store reports 0 accounts rather than an error. The live store is
  `map_data_storage_v2`, reached through `BackwardsCompatiableDataStorage`
  (Amazon's spelling), which takes MAP's own context wrapper.
- MAP's helpers start from `getApplicationContext()`, which is null here:
  nothing in this process created an `Application` for MAP's package. The
  `ContextWrapper` answers itself for that one method and delegates the rest,
  so the class loader and data directory stay MAP's.
- Accounts are sorted before the first is read. Otherwise a Dot on more than
  one account returns whichever token the `Set` iterated first.
- `getAccounts` returning null prints `accounts: 0` rather than throwing,
  because 0 accounts is the real case and a `NullPointerException` misleads.
- Extraction takes 0.55 seconds, so the token is held in memory for the life of
  the process.
- A refusal drops the held token and retries once, because Amazon rotates it.
  A refusal is a 400, 401 or 403, or an authenticated call that answers
  without what it should.
- A stale session can answer 200 with a sign-in page or an empty device list.
  Both count as refusals.
- MapDump's output must have no whitespace and be 16 to 4,096 bytes before it
  is used. Otherwise anything dalvik prints beside the value is posted to
  `api.amazon.com` as the credential, and comes back as an opaque 400.
- The token never reaches the disk. MapDump writes the value to stdout and
  every step, count and failure to stderr. Stderr may be logged; stdout never
  is.

## What the token buys

- `exchangeCookies` asks for `.amazon.com`, as the Alexa app does. The reply
  is `at-main`, `sess-at-main`, `session-id`, `session-token`, `ubid-main` and
  `x-main`: a signed-in amazon.com retail session, not an Alexa-scoped one.
- Asking for less does not get less. `.alexa.amazon.com` returns the same six
  cookies, keyed `.amazon.com`, and the command still works. The `Domain` this
  code stamps binds nothing at Amazon's end.
- So the feature's scope is the whole account. The only lever is whether the
  jar is installed. SECURITY.md and README.md say so.

## The two user agents

- The `api.amazon.com` one is MAP's own, read off the device. Its middle field
  is the MAP client library version, which no system property gives:

```
AmazonWebView/MAPClientLib/130050002/Android/5.1.1/AEOBC
```

- The `alexa.amazon.com` one has no on-device source, because the Dot never
  contacts that host. It is the Alexa app's shape, with this device's model
  and release and an invented app version.

## What the daemon offers

- A text entity `alexa_command` and a user service `send_command`: one for a
  person on the device page, one for an automation. Both end in
  `commandLocked`, and both are listed only once `UseCommand` has a sender.
- A send can take minutes: `extractToken` and each of the 4 HTTP requests
  have 30 seconds, and an auth failure runs the attempt twice. So a worker
  sends, never the read loop.
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

- The credential *is* the registration, so `serve.go` checks both the jar and
  the registration before it offers the box.
  `binary_sensor.<name>_alexa_registered` reports the registration;
  docs/api.md says how it is read.
- `commandReady` fails open on the reading. "Not registered" hides the box; a
  reading that could not be taken does not. The jar is a deliberate act and the
  reading an observation, so a failed observation does not revoke the act. That
  is the one case where the box is offered and still fails.
- Without the check, an unregistered Dot offers the box and a command fails
  with only `extract token: exit status 1: step: system context = ...`. Run from
  the daemon, MapDump's stderr after its first line does not reach the pipe,
  and logcat has no exception. Run by hand as 32051, the jar prints all four
  steps and `accounts: 0`.
- A Dot missing the jar or the account is asked again every 5 minutes, and
  stops once both hold. It costs a stat, plus a fork only when the jar is there
  and the account is not: 0.003% of a core.
- The registration is read only after the jar check, because the read can take
  1.5 seconds and `serveAPI` is on the cold boot's critical path, ahead of the
  firewall rule and the responder.
- Offering the box late drops every connection, as `UsePlay` does, because
  Home Assistant reads the entity list once per connection.
- A Dot deregistered while the daemon runs keeps a box that fails.

## Setup mode

- Registration is Alexa's own process: set the button to `pass through`, hold
  it until the ring turns orange, and add the device in the Alexa app.
- Nothing here runs it, but this intent starts setup mode without the button:

```sh
am startservice -a com.amazon.device.oobe.services.action.SETUP_MODE_ON \
  -n com.amazon.echo.csm.oobe/.services.SonarOOBEMainService --ei reason 2
```

- `reason` must be 2, `OOBE_ENABLED_BUTTON_PRESS`. Without it,
  `logcat -s AmazonOOBE.SonarOOBEMainService` reports
  `Start/Stop Reason = 0` and setup mode does not start.
- Setup mode shows as an address on `p2p0`, the Dot's own network. It takes
  the Dot off the LAN.
