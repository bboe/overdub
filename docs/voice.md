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

## The satellite

`internal/esphome` offers the Dot to Home Assistant as an ESPHome voice
satellite, so a pipeline can run over the API session Home Assistant already
holds. Nothing starts a turn yet.

- `DeviceInfoResponse` sets 2 of `voice_assistant_feature_flags`: the voice
  assistant and API audio. Without API audio, Home Assistant takes no audio
  over the API, and drops what is sent.
- A Dot with no clip player is not a satellite. Its media player lists no
  announcement format, and Home Assistant then makes the answer in the TTS
  engine's own format, often MP3.
- The speaker flag is not set. With it, Home Assistant streams the answer as
  16 kHz WAV over the API. Without it, Home Assistant makes the answer in the
  media player's announcement format, FLAC, which the clip player already
  plays. `TTS_END` carries its URL.
- The announce flag is not set. With it, Home Assistant waits up to 5 seconds
  for a wake word configuration on every connect, and nothing here has one.
- A turn starts with `VoiceAssistantRequest` and no flags, so Home Assistant
  starts at speech to text. The audio goes as `VoiceAssistantAudio`, 16-bit
  little-endian PCM. `end` stops the run when the audio is done.
  `VoiceAssistantRequest` with `start` false aborts it, and a cancelled turn
  sends that.
- The speech ends at `STT_VAD_END` or `STT_END`. The run ends at `RUN_END` or
  an `ERROR`.
- Events carry no run id, and Home Assistant sends `RUN_END` after an
  `ERROR`. So a turn ignores every event before its `RUN_START`, or a late
  `ERROR` or `RUN_END` from the last run would end the next turn. Home
  Assistant sends `RUN_START` before it validates the pipeline, so a run that
  fails validation still sends one.
- A turn has no deadline of its own. Home Assistant sends no event at all for
  a pipeline it cannot find, so the context a turn starts with must carry one.
- Closing a turn sends `VoiceAssistantAnnounceFinished`. Without it, Home
  Assistant shows the satellite as responding until the next turn starts.
- The last connection to subscribe is the satellite. A new one ends the old
  one's turn, and so does a disconnect. Events from any other connection are
  ignored.
- Every send checks, under the server lock, that the turn is still the
  current one. A disconnect closes the connection's send queue, and a send on
  a closed queue panics, which restarts the daemon.

## What this does not do

- It does not change anything in the session.
- It asks Home Assistant nothing, and holds no Home Assistant credential.

## The audio Alexa uploads

Measured on biscuit, Fire OS 6 8349, from turns that Home Assistant's text to
speech spoke to the Echo through another Echo's speaker.

- A turn is a `SpeechRecognizer.Recognize` event: a `metadata` JSON part, a
  `wakewordEngineMetadata` part on a wake-word or follow-up turn, then an
  `audio` part, streamed as the user speaks.
- The audio is Opus: fixed 80-byte packets, each one SILK wideband, 16 kHz
  mono, 20 ms (TOC configuration 9), padded to a constant 32 kbit/s. A packet
  decodes on its own, with no container around it.
- `internal/opus` splits the stream into packets, carrying a part packet into
  the next chunk. The 80 bytes never change, so no length prefix is read.
- The device decodes them. `/system/lib/libopus.so` is libopus 1.5.2 and
  exports `opus_decoder_create`, `opus_decode` and `opus_decoder_destroy`. It is
  the same file on biscuit, radar and donut, with md5
  `2f7b3859e3d6834cd02f646f756f862b`.
- A wake-word turn starts 0.5 seconds before the wake word.
  `initiator.payload.wakeWordIndices` gives the word's start and end in samples:
  8000 and about 17,100 to 18,100. Samples before `endIndexInSamples` are the
  pre-roll and the word. A tap (`TAP`) and a follow-up
  (`NATURAL_ATTENTION_SPAN`) carry no indices, and start where the user does.
- `wakewordEngineMetadata` is the wake-word engine's telemetry and an acoustic
  fingerprint (`KWFP`), not audio.
- Each wake also sends `Alexa.AudioFrontEndController.OneMicAudio`, about 1.4
  seconds of single-microphone Opus around the wake word, as an event of its
  own.

The decoder runs only on the device, so its test does too. CI runs the rest of
the package. From a tree that `build.sh` has built:

```sh
CC=$(ls "${ANDROID_NDK_HOME:-/opt/homebrew/share/android-ndk}"/toolchains/llvm/prebuilt/*/bin/armv7a-linux-androideabi22-clang) \
  CGO_ENABLED=1 CGO_LDFLAGS="-L$PWD/build/stublibs" GOOS=android GOARCH=arm GOARM=7 \
  go test -c -o build/opus.test ./internal/opus/
adb shell mkdir -p /data/local/tmp/opus-test/testdata
adb push build/opus.test /data/local/tmp/opus-test/
adb push internal/opus/testdata/turn.opus /data/local/tmp/opus-test/testdata/
adb shell 'cd /data/local/tmp/opus-test && ./opus.test -test.v'
adb shell rm -r /data/local/tmp/opus-test
```

`adb shell` exits 0 even when a test fails. Read the result in the `-test.v`
output: `PASS` on the last line.

`testdata/turn.opus` is one captured turn, "Alexa, what time is it?" in Home
Assistant's voice. The test checks its loudness every 100 ms against what
macOS's libopus decodes from the same packets.

## Open

- Amazon can move a device to another endpoint with a directive. PuffinApp would
  then write `AVSEndpoint` itself, and the session would bypass the relay until
  the daemon restarts. Not seen yet.
