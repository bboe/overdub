# Alexa's session

Alexa's session runs through this daemon on its way to Amazon.

Every Dot that installs the daemon works this way, whatever the voice mode,
which is what makes the install and the uninstall the part worth testing.

## What was measured

Measured on biscuit, Fire OS 6 8349.

- PuffinApp, Alexa's client on Fire OS 6, is Amazon's AVS Device SDK. It talks
  to Amazon through libcurl over HTTP/2, at the endpoint in
  `/system/etc/AlexaClientSDKConfig.json` (`acl.endpoint`,
  `https://avs-alexa-14-na.amazon.com`).
- The LIPC property `com.amazon.puffin AVSEndpoint` reads and sets that
  endpoint. A write takes effect at once: PuffinApp logs
  `MessageRouter:setAVSGateway`, drops its session with
  `reason=SERVER_ENDPOINT_CHANGED`, and dials the new endpoint. No app restart
  is needed.
- PuffinApp keeps the value in `/data/avs/resources/miscDatabase.db`, so it
  outlives a reboot.
- libcurl's CA path is `/system/etc/security/cacerts`, where each file is named
  by `openssl x509 -subject_hash_old`. Fire OS 6 also has
  `/system/etc/security/cacerts-sha1`, the same certificates named by
  `-subject_hash`. The daemon's authority goes into both. With it there,
  PuffinApp completed TLS and HTTP/2 with the relay and sent its requests: it
  does not pin Amazon's key.
- The relay's certificate names `127.0.0.1` and `localhost`, and the endpoint is
  `https://127.0.0.1:8443`.
- There is no `/etc/resolv.conf`, so the relay dials Amazon through the daemon's
  own resolver. With Go's default one, every request failed with
  `lookup avs-alexa-14-na.amazon.com on [::1]:53`.
- `OUTPUT` drops what no rule accepts, and loopback is not always among them.
  docs/pitfalls.md has what that looked like on donut.
- A reply that is merely written is a reply the device never sees. The
  long-lived request is answered with a status and nothing else until there is
  something to say, so the headers are flushed as soon as they are written.
- A request that ends with its headers goes on the same way. The HTTP/2 server
  hands the relay a body for every request, even one that ended with its
  headers, and the client sends any body with a length of 0 as a stream left
  open for one. So the downchannel `GET` went to Amazon as headers, then an
  empty data frame.
- A failure is answered with a failure. Amazon unreachable breaks the stream,
  which is what the device would see if the network ate the connection, and its
  own retry covers it. A status invented here is a status she never sent.

## Where the session goes

- The relay passes the session on to wherever `AVSEndpoint` pointed when the
  daemon started. If it already pointed at the relay, a daemon before this one
  left it there, and the relay uses `acl.endpoint` from the SDK's config
  instead.
- The write is skipped when the endpoint already points at the relay, because
  each write drops Alexa's session.
- At boot the daemon starts before PuffinApp answers LIPC. On donut PuffinApp
  started 5 seconds before the daemon, and `lipc-get-prop` still exited 3. The
  relay did not start, and Alexa stayed `PENDING` on an endpoint with nothing
  behind it. So the first read is tried every second for up to 2 minutes.
- A daemon that cannot start the relay points the endpoint back at
  `acl.endpoint` if it still reads the relay. A daemon before it, stopped with
  SIGKILL, left it there, and nothing listens there now.
- The relay starts beside the ESPHome API, not before it. PuffinApp's LIPC
  calls can take 10 seconds each while its session changes, and the API would
  wait for all of them.
- The relay serves before the write, because PuffinApp dials the new endpoint
  at once, and the read-back can take 15 seconds.
- The daemon counts the relay as running from just before the write. A SIGTERM
  that arrives during the write's read-back still points Alexa back.

## What it changes

- `com.amazon.puffin AVSEndpoint`, a property outside this daemon's namespace.
- One file under `/data/local/bin`, which the daemon creates on first run, and
  two under `/system`: `cacerts/4c55d173.0` and `cacerts-sha1/cbac1ab7.0`. Each
  always lands under the same name, so nothing has to be kept to remember it
  by; `internal/avs` pins both names with a test, and `uninstall.py` names them
  directly.

`uninstall.py` undoes all three and reads each one back. docs/deployment.md has
that order and why it is that order.

## What depends on it staying up

**If the daemon is not running, Alexa cannot answer.** The endpoint still points
at it. A clean shutdown puts it back, but init stops the daemon with SIGKILL, so
only `uninstall.py` reliably does. The boot script restarts a daemon that died 5
seconds later.

## The voice mode

`voice_mode` is a select, kept in `persist.overdub.voice_mode`, and it starts
in `pass through`. The property is written after the mode changes, not while the
API holds its lock: `setprop` and its read-back can take seconds. The button modes are not kept; this one is, because a
reboot that changed how the house hears you would be a silent change.

| mode | what the relay does |
|---|---|
| `monitor` | reads Alexa's answers and publishes them |
| `pass through` | passes the session on and looks at nothing |

- `monitor` reads Amazon's replies, the answers to a wake word and the
  downchannel both, and changes nothing in them. Each `SpeechSynthesizer.Speak`
  gives its words to `alexa_reply`.
- The words are the cues of the Speak's WEBVTT `caption`. A line right before
  a timing line is a cue's identifier and is dropped; any other line is words,
  a bare number included. A Speak with no caption gives its SSML with the tags
  removed.
- One question can draw more than one Speak. Asked for the weather on an Echo
  Dot (3rd Generation), Amazon sent 2, 0.3 seconds apart, and another time 3,
  1.0 and 1.6 seconds apart. So each Speak is published as it arrives, as a
  state of its own, and the history reads as the whole answer.
- Joined, the answer outgrew the sensor: Home Assistant refuses a state over
  255 characters, and the weather ran past it. A Speak over 255 bytes is
  split at the last space that fits, or on a character when a word is longer.
- A joined answer also had to wait for its last Speak. Waiting 1 second for
  quiet published the 3-Speak weather 3 times, and no wait is long enough for
  every answer.
- Speaks are published in order by one goroutine, so a slow Home Assistant
  does not hold Alexa's reply on its way through. Its queue holds 16; a Speak
  past that is dropped and logged. One that reaches the head of the queue
  after `monitor` ends is dropped, and so is one that lands after the mode
  has changed.
- Leaving `monitor` empties `alexa_reply` and publishes it empty.
- The reply is published on its own, not by waking the sensor poll, which
  would run the firewall check and every other read to send one string.
- The poll does not carry the reply. It reads every sensor before it
  publishes, so a poll that read the old answer just before a new one was
  published would put the old one back. Each publish of the reply reads it
  under a lock of its own, so the last one sends the newest words.
- Amazon never sends what it heard. A reply carries the caption and SSML of
  the answer and no transcript, so the question cannot be published.
- The log gets the length of each Speak, not its words.
- A delimiter counts only at the start of a line, as the multipart format
  has it. Audio bytes that spell the boundary are audio.
- The relay looks only at JSON parts. An audio part is skipped as it passes,
  keeping less than one boundary of it. A JSON part is held to 64 KB, and a
  longer one is skipped.
- `monitor` asks Home Assistant nothing. Sending the audio to Home Assistant
  as well would run its whole pipeline, which acts on a command Alexa also
  acts on, and answers too.
- The relay is the same in both modes, so a mode change takes effect at once,
  without dropping Alexa's session. A part already passing when `monitor` is
  chosen is skipped.

## What this does not do

- It does not change anything in the session.
- It asks Home Assistant nothing, and holds no Home Assistant credential.

## Open

- Amazon can move a device to another endpoint with a directive. PuffinApp would
  then write `AVSEndpoint` itself, and the session would bypass the relay until
  the daemon restarts. Not seen yet.
