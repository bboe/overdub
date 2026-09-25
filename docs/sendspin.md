# Sendspin

Sendspin is the multi-room audio protocol Music Assistant uses as its native
playback provider. `internal/sendspin` implements the **client** side, so the
Dot joins a synchronised group.

- Spec: github.com/Sendspin/spec, pinned at commit `8fc2f8f` (2026-09-12). Read
  the spec for what the protocol says. This page carries what was measured and
  what was decided against.
- The repository has no tags and no releases, and changes continuously.
  `version: 1` on the wire is the core version and does not move with the
  document, so an unpinned citation rots silently.
- The wire authority is `aiosendspin` 9.1.1, the version Music Assistant pins.
  "The reference server", at the end, lists where it differs from the spec.

## Connecting

### The shape

- Plain `ws://`. Confidentiality is inside the payloads: Noise `KKpsk2`, with
  the **server as initiator** whichever side dialled. `client_id` and
  `server_id` are base64url Curve25519 public keys.
- **Server-initiated.** The Dot advertises `_sendspin._tcp.local.` on 8928 and
  Music Assistant dials it. This reuses the mDNS responder and the firewall
  helpers, at the cost of the multi-server admission rules.
- Role `player@v1`, formats **`flac`, then `pcm`**, at 48000 Hz, then the same
  two at 44100 Hz; stereo, 16-bit. A server takes the first it can encode,
  until the Dot asks for another ("Following the output" below).
  - Stereo on every output. The speaker path sums the channels: through a
    2-channel AudioTrack, a sweep in either channel alone played, and one with
    the right channel inverted was inaudible. A single-driver Bluetooth speaker
    (JBL Go 3) did the same. So the Dot never mixes down.
  - As PCM the stream is about 1.54 Mbit/s. FLAC is lossless, so it carries the
    same samples in fewer bytes: 58% and 67% of PCM on 2 test signals, about 2.1
    times mono FLAC. The signals put independent noise in each channel; music
    shares more between them.
  - PCM stays second at each rate: every server must support it.
  - 48 kHz is first because the Dot's speaker runs at 48 kHz.
  - The chime and the stream share one player, which opens at the stream's
    rate (docs/audio.md).

### The WebSocket is hand-rolled

Binary size, measured on a linux/arm build of the daemon:

| variant | delta |
|---|---|
| a `net/http` server, no library | +15 KB |
| `gorilla/websocket` | +200 KB |
| `coder/websocket` | +205 KB (2.13%) |

- About 56 KB of that is library code. The rest is mostly `compress/flate` for
  permessage-deflate, which is useless here: Noise ciphertext does not
  compress. `CompressionDisabled` saves 0 bytes, because the linker cannot
  prove the path dead.
- Size is not the reason. docs/constraints.md imports `dnsmessage` because a
  peer-facing parser is worth not owning. That does not apply here: every
  application byte is authenticated, so a framing error fails Poly1305 and the
  connection aborts. The only cleartext is the init exchange and the two
  `noise/handshake` frames.
- The remaining risk is an off-by-one in masking or reassembly.
  `FuzzFrameReader` ran 9.1 million executions and found nothing. The target is
  one peer, not full RFC 6455 conformance.
- Each refusal in the frame reader has a test. Lengths are checked against the
  read limit **before** the payload is allocated.
- A chain of empty continuation frames allocates nothing and passes both length
  checks. `maxWSFrames` and `maxMessageFrames` (both 4,096) end such chains at
  the WebSocket and the Sendspin layer.
- `maxRequestBytes` caps each upgrade line, not the request. With
  `maxRequestLines` the request is bounded at about 512 KB.
- `Conn.mu` is not for framing. Go's per-connection write lock already keeps
  frames whole. The mutex makes "arm the deadline, then write" one step, so one
  caller cannot spend a deadline another caller armed.

### Identity

- `client_id` is the routing identifier and the static key the handshake
  authenticates. A pairing PSK sits beside it: per device, from a CSPRNG,
  long-lived, not consumed by pairing.
- **The daemon generates both on first run. They never cross adb.** Shell cannot
  derive an X25519 public key, and a secret that is never pushed needs none of
  the staging docs/pitfalls.md describes.
- The key file (`sendspinKeyPath`) is 64 raw bytes: private key, then pairing
  PSK. The public key is derived, so it cannot go stale.
  `TestStoredPrivateKeyAgreesWithTheNoiseLibrary` exists because a disagreement
  would not fail: it would give a `client_id` no server can match.
- A load **refuses** a file that group or other can read. The pairing token is
  `client_key || pairing_psk`, so a readable key file is a readable token.
- Either half all zeros is refused. X25519 clamps a zero private key into a
  working one, and a zero PSK is a guessable token.
- The file is written to `<key>.new-<random>` at `0600`, synced, then put in
  place with `link`, which fails if the key exists. Two daemons racing on first
  boot (the supervisor respawns every 5 seconds) either win the link or read a
  complete file. A kill between write and link leaves the temporary file, so
  `uninstall.sh` sweeps `<key>.new-*`.

### The pairing token

- Clients must implement the Pairing PSK method only: no PAKE, no pairing code.
  The operator carries a token from the Dot to the server. The token is `SP:`,
  a version character, and a base32 body.
- After RFC 4648 base32 with `=` stripped, **every `2` becomes a `9`**. This is
  not for the QR alphanumeric set, and the spec gives no reason. Follow the
  published vector.
- Two constants are asserted against the spec: the Sentinel PSK,
  `SHA-256("sendspin-sentinel-psk-v1")`, and its `psk_id`,
  `base64url(SHA-256("sendspin-psk-id-v1" || PSK))`.
- The Sentinel's `psk_id` has neither `-` nor `_`, so it cannot tell base64url
  from standard base64. `SHA-256("5")` as a PSK gives a `psk_id` with both. The
  wrong alphabet would fail about 3 pairings in 4.
- **No encoder or decoder ships.** Nothing can use a token until pairing exists.
- A future decoder must be lenient about input (trim, upper-case, tolerate a
  missing `SP:`) and strict about the rest (unknown version, non-base32 body,
  short payload). Bytes past the 64 defined are ignored.

#### Keeping it out of the log

- The token is `client_key || pairing_psk`: printing it hands over the key
  file. `/data/local/tmp` is `drwxrwx--x` and `overdub.log` is `-rw-r--r--`,
  so every uid can read the log, and it travels in any `adb pull`.
- The daemon logs the `client_id` only. To build the token, as root:

```sh
adb shell 'su -c "od -An -tx1 /data/local/bin/.overdub-sendspin-key"'
```

- The last 32 bytes are the pairing PSK. The first 32 are the private key, which
  the token does not carry. Take the **public** key from the logged
  `client_id`. The token is `SP:0 || base32(public_key || pairing_psk)`,
  padding stripped, every 2 a 9.
- There is no flag to print it: that is the same hazard with a switch.
- **A token that reached a log is disclosed.** Rotate by deleting the key file
  and restarting. The Dot then appears to every server as a **new client**,
  because the `client_id` is the identity.

### The handshake

- **The Dot is the WebSocket server and the Sendspin client, and speaks first.**
  Music Assistant dials; the Dot sends `client/init` first.
- **The Noise roles are reversed.** The server is always the Noise initiator.
- **As responder the Dot encrypts with the second CipherState and decrypts with
  the first.** `flynn/noise` returns the pair in canonical order to both sides.
  Getting it backwards looks exactly like a wrong key. The two-way transport
  tests run a real server side to pin it.
- `KKpsk2` mixes the PSK at the end of message 2, but the server names its PSK
  in the payload of message 1. This works because the message 1 payload is
  encrypted without the PSK, and `flynn/noise` lets a `psk{2+}` handshake take
  its key later through `SetPresharedKey`.
- The prologue is the **exact wire bytes** of `client/init` then `server/init`.
  A re-encoding that differs by one space fails the handshake with no reason.

#### A miss and a misbinding

- The server's `psk_id` is compared only against candidates **of the declared
  category**. The category binds.
- A **miss** completes with the Sentinel PSK (the Sentinel Fallback), so a
  client that lost its pairing record can still reach a server to re-pair. The
  Sentinel authenticates nobody.
- A **misbinding** (a `psk_id` matching a long-term key issued to a different
  `server_id`) must fail. **Not implemented**: no long-term key exists until
  pairing does, so `PSKSet` holds the pairing PSK only. The pairing flow must
  add it.
- `DecodeID` then `EncodeID` stores the `server_id` in one spelling.
  `base64.RawURLEncoding` ignores the final character's unused bits: 4
  spellings decode to one 32-byte key.
- `lt` is still accepted on the wire. A server declaring it gets a miss and the
  Sentinel, not a refusal. Without `lt` in the accepted set the handshake times
  out in the unknown-category error.

### Two fragmentation layers

- WebSocket fragmentation, and Sendspin's own inside the encrypted channel
  (message type `1`, a flags byte, the original type on the first fragment).
  They nest and are unrelated.
- The inner layer exists because a Noise transport message is at most 65,535
  bytes: 65,518 of payload after the tag and type byte.
- Reassembly holds one message per direction. Each refusal has a test, and the
  size is checked on every fragment.
- The read limit is `maxCleartextFrame` (2,048 bytes) before encryption, where
  real messages are a few hundred. It opens to 65,535 after.

### What the matched PSK allows

`server/activate` declares a connection's purpose. What is allowed depends on
the PSK matched in the handshake:

| matched | allowed activity sets |
|---|---|
| long-term | `[]`, `['playback']` |
| pairing | `[]`, `['pairing']` |
| Sentinel | `[]`, `['pairing']`, `['playback']` only with unpaired access |

- Playback and pairing never share a set. A pairing-PSK connection is never
  playback-capable.
- Refusals apply in order, because more than one can be true:
  - `pairing_required`: a setting would have made it admissible. Example: a
    Sentinel connection, unpaired access off, `['playback']` with
    `['player@v1']`.
  - `unauthorized`: nothing could make it admissible. Example: the same
    connection sending `['pairing']` with roles.
  - `method_not_supported`: a `pair/abort` for a pairing method not allowed or
    not offered. The connection stays open.
- `active_roles` persists across activations that omit it. A first activation
  that omits it reads as empty roles, not an error.
- A later activation that makes the connection not playback-capable and
  **omits** `active_roles` empties the roles. One that **resends** them is
  `unauthorized`.

## Streaming

### What the Dot declares

`player@v1`, 2 formats, and `unpaired_access` enabled. The last lets Music
Assistant play over a Sentinel-keyed connection once its operator approves the
Dot.

- `buffer_capacity` is 2 seconds of 48 kHz PCM, 384,000 bytes. The server counts
  FLAC bytes against it too, so FLAC leads further: about 3 seconds at the
  ratios above, and up to `aiosendspin`'s 30-second `max_duration_us` through a
  quiet passage, where a block of silence is an 11-byte frame. `streamHold` is
  sized for that cap.
- `min_buffer_ms` is `sendspinBuffer`, 500. Over Bluetooth it is 900; "Over
  Bluetooth" below says why.
- `static_delay_ms` starts at **0**. It is not the place for the ~95 ms
  docs/audio.md measures: the spec says it is the delay *past* the audio port,
  and a Dot's speaker is before the port. Music Assistant exposes it as
  `CONF_SENDSPIN_STATIC_DELAY`, the knob an operator sets to 0 for a speaker
  with no external amp. A compensation declared there would vanish.
- **The client applies the delay, not the server.** `aiosendspin`'s
  `effective_ts_us = entry.timestamp_us - role.get_static_delay_us()` is only a
  log field and the late-drop decision. A chunk is one message for a whole
  group, so its timestamp cannot carry a per-client delay. The server adds the
  delay to its send-ahead: `max(min_buffer, required_lead) + static`. The
  client subtracts it: `compute_play_time = client_time - static_delay`.
- So a larger delay makes the player sound **earlier**.
- `required_lead_time_ms` is `sendspinLead`, **350**, derived from hardware.
  Before the first chunk can be placed, the frame-to-moment mapping needs 134 to
  164 ms of silence to settle, and the player and HAL hold 131 to 144 ms. At a
  200 ms lead the first 118.7 ms of a track was lost.
- On the speaker it changes nothing on the wire. For a **buffered** stream the
  server sends `max(min_buffer_ms, required_lead_time_ms) + static_delay_ms`
  ahead, so 500 governs. For a **live** stream `aiosendspin` uses
  `min_buffer_ms + static_delay_ms` and ignores the lead.
  `DEFAULT_INITIAL_DELAY_US` (250 ms) applies only with no audio roles. The
  field keeps a smaller `min_buffer_ms` from taking the start of every track.

#### Over Bluetooth

- Over a Bluetooth speaker the lead is `sendspinBluetoothLead`, **1,100**.
  With a JBL Go 3 the player read 429 to 494 ms ahead of the speaker, where
  the Dot's own speaker reads 131 to 144 ms. The mapping then needs its settle
  as well, so a new stream needs about 650 ms of lead.
- At a lead of 350, 500 governs. Across 8 streams the first chunk came 390 to
  476 ms before it was due, and once 26 ms after: the server spends about 25 to
  110 ms before the first chunk goes out. `aiosendspin` stamps it
  `now + send_ahead` in `_resolve_channel_play_start`, before the encode, and
  the stamp does not move.
- So each new stream lost its start: 142 to 679 ms dropped as late. With debug
  lines in, one start dropped its first 2 chunks whole and 48 ms of the third,
  257 ms in all.
- At 1,100 the first chunk of a Plex track came 974 ms before it was due, and
  nothing at the start was dropped. That is 1 stream.
- A skip loses nothing at either lead. Music Assistant ends the stream and
  starts the next at once, the Dot keeps its stream open, and the mapping holds.
  The loss is at a stream the Dot opens: the first play, after a pause, after a
  restart.
- A live stream needs `min_buffer_ms` as well. `aiosendspin` ignores the lead
  there. Music Assistant counts radio, audio sources and plugin sources as
  live, and any track whose provider marks `is_realtime`. Spotify's soloist
  backend does. A Spotify stream still lost 257 ms at its start at a lead of
  1,100.
- So over Bluetooth `min_buffer_ms` is `sendspinBluetoothBuffer`, **900**: the
  650 ms the Dot needs, plus the 110 ms the server spent at most, rounded up.
  The one first chunk that came late is not covered. The first chunks of 2
  Spotify streams then came 823 and 850 ms before they were due.
- Those streams still dropped 20 and 17 ms as late, not at their start. The
  mapping then had 190 ms to spare. The likely cause is the burst average's
  one jump, about 2 seconds in (docs/audio.md). That is not measured.
- The cost: every buffered stream to this Dot starts about 600 ms later over
  Bluetooth, and every live stream plays about 400 ms later for its whole
  length. The send-ahead is the largest across the group, so every member
  waits the same. For a buffered stream the lead adds no latency after the
  start: the queue grows past it within seconds.
- `watchOutput` declares both in `client/state` before it sends
  `stream/request-format`. `aiosendspin` reads both in order, and joins the
  role at the new rate `max(100 ms, send_ahead)` ahead, from the new figures.
  A speaker connecting mid-stream should open a gap of about 1.1 seconds on a
  buffered stream and 0.9 seconds on a live one, and drop nothing. That is not
  measured.
- `aiosendspin` moves a timeline only forward: `_resolve_channel_play_start`
  shifts every channel up to `now + send_ahead` at each commit. A stream fed at
  exactly playback pace sits at its floor, so a speaker connecting mid-stream
  would move the whole group about 400 ms later, and every member would hear a
  gap. A disconnect would not move it back until the stream restarts. That is
  read from the code; radio is the likely case, and it is not measured.
- Spotify's soloist backend feeds at up to about 1.1 times playback pace, so
  its queue grows past the floor. In a group of 2 Dots on Spotify, a speaker
  connected to 1 of them 65 seconds in. The other placed 4 minutes 42 seconds
  with no drop, its chunks came 3.04 to 3.54 seconds ahead before and after,
  and it played no silence past its start. The Dot that switched joined at
  44.1 kHz with its first chunk 699 ms ahead, and dropped nothing on that
  stream.
- The declared figures follow each read of the output, and are sent again only
  when the output changes. The speaker's come back when the speaker
  disconnects.

### The messages that bracket a stream

`stream/start` opens one, `stream/end` ends it, `stream/clear` discards the
buffer and keeps it open. Each reaches the player and the session.

- **`stream/end` is a discard.** The spec: clients "MUST stop output and clear
  its buffers" unless the role defines otherwise, and the player role does not.
- Two `aiosendspin` lines look like drain and are not. `PushStream.stop`'s
  "call `clear()` first" is in its `keep_stream=True` branch, which skips
  `stream/end`. The reference client's `_handle_stream_end` touches no audio
  because `connection.py` holds none.
- On pause Music Assistant sends a bare `stream/end` and moves the group to
  `stopped` 89 to 850 us later. A pause discards about 1.817 seconds and goes
  quiet in 30 ms. A track change or a track running out discards 0: Music
  Assistant stops sending, lets the buffer empty, then ends the stream.
- A finished stream drops its queue, refuses new audio, and retires at the next
  block. The 134 to 164 ms already in the player still plays. The close report
  counts what was discarded.
- Stopped outright: giving up the player role, a format this client cannot
  take, and the connection going away (a `defer`, or a writer feeds silence
  for the daemon's life). The role check is on the *player* role, not every
  role; with one supported role no test can tell them apart.
- A **repeated** `stream/start` at the same rate, or one before the writer
  retired the last, takes up the existing stream. Timestamps are absolute, so
  the audio places itself; a fresh stream would spend another 134 to 164 ms
  relearning the mapping. Music Assistant restarts 60 to 90 ms after a
  `stream/end`.
- Any format other than the declared ones is refused, and **closes** an open
  stream so following audio is not read in the old format. A server chooses
  from `supported_formats`, so a mismatch is a fault. Played anyway it is noise
  or wrong pitch.
- A `stream/start` with no player object is not an error: artwork and
  visualizer streams use the same message.
- **Roles match by family.** `aiosendspin` checks families (`player`), while
  `active_roles` names versions (`player@v1`). Everything before `@` decides.
- An **omitted** role list means every role. An **empty** list means none and
  ends nothing. `[]string` collapses `null` and `[]`; `*[]string` separates
  them.
- Giving up roles needs an explicit empty list: omitted means the roles
  persist. A Go pointer to a nil slice marshals as `null`, which reads as
  omitted.
- `stream/clear` applies only with the player role and an open stream.
  `stream/start` is refused with no player role, because the run loop keeps
  reading after an activation that leaves no roles.

### The audio itself

- Binary type bytes 4 to 7 are the player's; only **4** carries audio.
  `aiosendspin` defines nothing for 5 to 7.
- The header is 9 bytes, `>Bq`: type, then a **big-endian signed 64-bit**
  timestamp in microseconds. The PCM after it is little-endian. One endianness
  for the whole message reads plausible rubbish instead of failing.
- A bad chunk is **dropped, not fatal**, and its message holds none of the
  server's numbers. The run loop logs each distinct message once, keyed on its
  filled-in arguments, and the list holds 16. A server stamping 25 ms chunks
  wrongly would fill it in under half a second and silence the connection's log.
- The session survives a bad chunk. Otherwise a server that stamps every chunk
  wrongly disconnects on every stream, and Music Assistant stops retrying after
  about 8.5 minutes.
- A body shorter than 8 bytes has no timestamp. Reading it anyway panics, which
  is the supervisor's 5-second restart with the button ungrabbed.
- Audio that is not a whole number of frames is refused. At 16 bits one stray
  byte misaligns every later sample for the rest of the stream.
- A chunk with no stream open is dropped: only a `stream/start` names its
  format.
- **`send_ahead` is not a wire field.** The server computes it from
  `min_buffer_ms`, `static_delay_ms` and `required_lead_time_ms`. The first
  chunk follows `stream/start` by about 1 ms.
- `stream/request-format` asks for a rate; "Following the output" below.

### FLAC

- `github.com/mewkiz/flac` decodes it. echolocal runs the same library on a
  Dot. On a Dot, a 96 ms stereo frame takes 6.9 to 8.2 ms, about 8% of one
  core, and allocates 56 KB that nothing keeps.
- Music Assistant sends it. On a Dot, stereo FLAC of a real track took 0.99
  Mbit/s of Wi-Fi: 65% of stereo PCM, and about 1.3 times the mono PCM stream it
  replaced. Every chunk was placed, and the daemon used 19% of one core over 30
  seconds, against 14.6% for mono FLAC on the speaker.
- `codec_header` is base64 of `fLaC` and a STREAMINFO block, 42 bytes. A bare
  34-byte STREAMINFO is accepted too, as `aiosendspin`'s own decoder accepts it.
  A header that does not describe the `stream/start`'s rate, stereo and 16-bit
  refuses the stream.
- The header is read by hand, not by the library. `flac.New` parses a whole
  metadata block before checking its type: a 40-byte header whose PICTURE block
  claimed 128 MB allocated 128 MB, and every `stream/start` is read.
- `aiosendspin` encodes through ffmpeg at compression level 5, which picks
  4,608-frame blocks: 96 ms. Each chunk is 1 frame. A chunk of several frames
  decodes within the cap below; a frame split across chunks does not.
- Stopping the group drops the encoder's partial block. In the interop test the
  last 3,840 of 96,000 frames never arrived.
- The frame's sync, rate, channel and bit-depth codes are read before the
  library sees it. The rate code must name the stream's rate or defer to
  STREAMINFO. Any of FLAC's 4 stereo layouts passes; the library rebuilds
  left and right from a side channel. `mewkiz/flac` writes to the standard log
  for the 24 and 176.4 kHz codes, which would be a line a frame outside
  `untrustedlog`. It also decodes a bit depth left to STREAMINFO as 0 bits.
- Its errors carry the frame's numbers. A refusal is 1 of 3 fixed messages, for
  the once-per-message log above.
- A chunk decodes to at most 4,608 frames: the streamable subset's largest block
  up to 48 kHz, and what `aiosendspin` sends. Each frame's block size is read
  before its samples.
- A 16-byte stereo frame can claim 65,535 frames of silence. Uncapped, 1 message
  of such frames decodes to about 1 GB. On a Dot, 1 such frame took 5.6 ms and
  787 KB to decode, so about 180 a second would hold a core. A 4,608-frame one
  takes 0.36 ms and 56 KB.
- The cap bounds frames, not the library's allocations. It allocates up to
  32,768 Rice partitions for each channel before reading them. A 9-byte frame
  allocated 256 KB on ARM before failing. 1 message of the smallest valid stereo
  frames allocated 1.3 MB, and 1.6 MB with that 9-byte frame at its end. It is
  garbage nothing keeps. Byte for byte it costs about the CPU of real audio:
  0.85 times natively, and 1.2 times under ARM emulation.

### Following the output

- Each output runs at one rate: 48 kHz on the speaker, 44.1 kHz on a Bluetooth
  speaker (docs/audio.md). AudioFlinger resamples a stream at the other rate,
  so a 44.1 kHz track to a Bluetooth speaker was resampled twice: by the
  server to 48 kHz, then back.
- So the Dot asks the server for its output's rate. `stream/request-format`
  carries only `sample_rate`, and the server keeps the codec.
- The spec at `8fc2f8f` has no such message. Sendspin/spec#195 folded it into
  `client/state`, where a player states a preference as `format`. `aiosendspin`
  9.1.1 does not read that field.
  The table at the end lists it.
- Music Assistant converts at 2 stages. It feeds `aiosendspin` at one rate per
  play, picked when playback starts from the rate the leader's role prefers
  (`_select_session_pcm_formats`), and converts the track to it with ffmpeg.
  `aiosendspin` then converts that feed to each player's rate with soxr. In a
  group, a member at another rate than the leader's is converted there.
- So a play that starts on the output's rate converts a track at most once. A
  request mid-play changes only the second stage, until the next play: a
  44.1 kHz track moved from the speaker to a Bluetooth speaker goes to 48 kHz
  and back. Asking for nothing costs the same: the second conversion is then
  AudioFlinger's.
- Music Assistant's signal path shows the rate picked at the start of the play,
  so it changes only after a pause and play. The Dot's log line
  `started a flac 44100 Hz` shows what arrives.
- The spec lets a server pick a track's native rate instead ("MAY ... to match
  a track's native sample rate and avoid resampling"). Neither `aiosendspin`
  nor Music Assistant does, and on this Dot it would save nothing: a track at
  the other rate is converted once either way.
- Not 44.1 kHz everywhere. The speaker would then resample every stream, and a
  48 kHz source twice.
- The output is read every 2.5 seconds (`outputEvery`), about 1 ms a read.
  The request goes only while the session holds the player role:
  `aiosendspin` flags a payload for a role that is not active.
- The server keeps the request on the role object. A role taken again starts
  at the first format offered, so the Dot asks again after a roleless spell.
  A reconnect sends a fresh `client/hello` and starts over the same way.
- A request mid-stream is a new stream. `aiosendspin` sends `stream/start` in
  the new format with its next chunk, and joins the role near the playhead,
  `max(100 ms, min_buffer_ms + static_delay_ms)` ahead for a live source and
  `max(100 ms, max(min_buffer_ms, required_lead_time_ms) + static_delay_ms)`
  for a buffered one. The Dot closes the old stream and opens one at the new
  rate.
- Measured with Music Assistant and a JBL Go 3: the new `stream/start` came 163
  to 349 ms after the request, 3 times. Connecting the speaker lost about 0.3
  seconds of audio, about what a change of output costs the mapping on its own
  (330 ms), and disconnecting it about 1 second (docs/audio.md).
- `TestInteropStreamsAt44kToADotThatAsks` runs the request against the
  reference server: the stream arrives as 44.1 kHz FLAC, 2 seconds of it about
  88,200 frames.

### What the log says while a stream runs

- Most things log once per connection. Stream transitions log every time, or a
  connection that plays several tracks reports only the first.
- Playback lines use a second `untrustedlog.Log`. The run ceiling (5,000) never
  refills, and 2 summaries a minute reach it in about 40 hours of playback.
  Disk use is the sum of two bounded budgets, and each says when it drops.
- Chunks are not logged one by one: 40 lines a second. The first chunk logs its
  lead. A summary follows every 30 seconds, at stream end, and at connection
  end: chunks, frames, bytes, lead range.
- A summary fires when any message arrives after the interval, not only a
  chunk. A stream that stops sending audio while the server keeps talking is
  the starvation it must show.
- The interval is a duration, not a count: a count is a rate the server picks
  (1 ms chunks at 1 summary per 500 is 120 lines a minute).
- **Each summary gives audio placed and silence played in its window.** One
  stream placed 103.875 seconds against 3.3 seconds of silence with nothing
  lost: received equalled placed plus dropped. The silence was time with
  nothing due.
- Window totals are keyed on the stream itself. A replaced stream restarts its
  counters at 0, and a carried-over baseline undercounts without going negative.
- Per-window silence is **1.0 to 1.9 ms** across 3 Dots and about 20 windows:
  frame rounding between chunks.
- **Music Assistant keeps one stream open across a track change**: no
  `stream/end`, no `stream/start`, no gap in the summaries. The gap between
  tracks arrives as a hole in the timeline.
- **A lead mixes two things**: it is `ClientTime(stamp) - now`, so it moves when
  the server sends late and when the clock estimate moves. So each summary
  carries the filter's spread and how far the offset moved since the last
  summary. A lead that fell while the offset held is the server.
- The offset baseline carries across windows. The spread does not (the same
  chunk rewrites it). Lead bounds do not either, or a window could never seed
  its own.
- **Music Assistant opens streams short of the declared floor.** First leads
  of 182, 218 and 490 ms against 500. At 218 ms the clock was good to 313 us
  and moved 1.09 ms in 30 seconds, so the shortfall is the server. It fills
  toward the ceiling after: 1.97 seconds by the end of that window.
- So the buffer waits for no quantity of audio. A stream is placed against the
  player's own reading and plays what is due. It needs lead, not depth, which
  `required_lead_time_ms` declares. "Over Bluetooth" above says where a
  shortfall goes.
- Across 5 streams the first chunk was due 481 to 493 ms ahead and the lead
  grew to about 2.4 seconds: `min_buffer_ms`, then that plus
  `buffer_capacity`.
- A `stream/start` with nothing for a player flushes nothing, so a visualizer
  stream does not split a summary. `stream/clear` flushes the summary and keeps
  the stream. Giving up the player role flushes it then.
- Leads print as durations. Integer milliseconds would show -999 us as `0 ms`,
  and late is the failure this number catches.
- `onAClock` bounds a chunk timestamp. Stamps are microseconds on the server's
  monotonic clock; an epoch stamp is already past the ceiling. Music
  Assistant's stamps pass.
- The filter keeps `ServerTime`, `ClientTime` and `Converged` for the tests:
  the first two are an inverse pair that checks the drift arithmetic.

### The numbers may change during a session

- A client MAY update `required_lead_time_ms` and `min_buffer_ms` at any time
  (SHOULD debounce). `output_delay_ms` may change when the output does, and is
  persisted per output across reboots.
- The spec derives `min_buffer_ms` from the upper tail of chunk arrival delay,
  `arrival - compute_client_time(timestamp - send_ahead)`, discarding samples
  from before convergence. `required_lead_time_ms` cannot be derived that way:
  chunks after `stream/start` arrive in a burst.
- So 500 is a placeholder, and the wire can correct it without reconnecting.
- **No pairing method is advertised**, against the spec's "at least Pairing
  PSK". Claiming one the client cannot honour is worse. `offeredPairMethods`
  feeds both `client/hello` and the admission rules.
- Only roles in `supported_roles` can become active. `display@v1` is dropped.
- A test pins the declared rates to `audio.Rates()` and the channels to
  `audio.ChimeChannels`. A mismatch fails as a refused stream or wrong pitch.

## Presence

### Being found

- The advert carries TXT `path` (required) and `name` (should match
  `client/hello`).
- `tcp/8928` needs a firewall rule, added **after** the socket binds. A rule for
  a port nothing listens on is re-asserted every 30 seconds and removed by
  nothing.
- `sendspin.Advert` hands `internal/mdns` a service as `esphome.Advert` does,
  so `internal/sendspin` never imports `internal/esphome`.
- Both services share `<name>.local.` and its A record. mDNS conflict is
  different data under one name, so this is not one.
- The advert goes up only with the listener behind it. A Dot that could not
  read its key or bind advertises nothing.

### The advert changes when the Dot reboots

- TXT `overdub_boot`, not in the spec: the first 4 bytes of SHA-256 over
  `/proc/sys/kernel/random/boot_id`. Stable for a boot, different after it.
- A server that loses the Dot retries at 1, 2, 4 ... 256 seconds, 10 attempts,
  the last 8 minutes 36 seconds after the loss, then never again. That matches
  `aiosendspin`'s arithmetic.
- After that only discovery revives it. A power cut sends no goodbye, so the
  records stay cached for the PTR TTL, 75 minutes, and python-zeroconf calls
  back only for records it does not hold. Music Assistant stayed away 20
  minutes after the Dot returned.
- A **changed** record fires `Updated`, which `aiosendspin` treats as `Added`.
  Identical records fire nothing. A sleeping server is woken (91 and 84 ms from
  listener to handshake), and one that gave up is revived.
- A revived task has `retry_initial_connection=False`: it dials once.
- Boot id, not a per-start value: the daemon restarts far more often than the
  device reboots, and a token per respawn would be a reconnect every 5 seconds
  in every server. The cost: a daemon down over 8.5 minutes *without* a reboot
  is not revived.
- Not the clock: the RTC is dead after a long unplug, and every cold boot starts
  at `2010/01/01 00:00`. Hashing keeps the kernel's UUID off the wire.
- It costs 22 bytes per response carrying the TXT, and one update to every
  browser at each reboot.
- **The advert goes up only once the port is reachable.** The listener binds
  and the rule goes in at about 25 seconds of uptime. `wlan0` does not exist
  for 15 seconds, the lease comes later (50 seconds on one boot), and netd
  rebuilds the INPUT chain in between. An early announcement advertises a port
  that drops the one dial it buys, with nothing logged at either end.
- So the switch waits for an IPv4 address and asserts the rule again just
  before advertising. `HoldTCPOpen` re-asserts only on its 30-second tick.
  `AllowTCP` checks before appending, so it cannot duplicate.
- The wait is bounded by `addressWait` (5 minutes); then the switch logs and
  advertises anyway. With no address `Advertise` records the service and sends
  nothing, so it is listed when an address arrives.
- Nothing is advertised after the switch is off. The check is `on` under the
  switch's lock, which `disable` clears before withdrawing. A check of the hold
  channel alone would race the withdrawal.
- This works around one library version. Sendspin/spec#207 removed the advice
  to give up; Sendspin/aiosendspin#348 is open. When it lands, the token can
  go.

### Turning it off

`switch.<name>_sendspin` removes the surface: advert withdrawn with a goodbye,
listener closed, rule deleted and no longer re-asserted, sessions ended.

- Down: advert, listener, sessions, rule. Up: bind, rule, serve, advertise. The
  rule must never outlive the socket.
- A kill cannot say goodbye, and then the rule does outlive the socket.
  `SIGTERM` withdraws the advert but does not delete the rule, because that
  waits on netd's lock. The next start cleans up.
- Nothing at startup sends a goodbye. The name is not claimed yet this boot,
  and nothing can be sent before the responder has an address.
- Deleting the rule waits for the re-assert goroutine to exit. A tick already
  delivered would put the rule back.
- `Serve` returning does not end admitted sessions, so `Client.Close` closes
  every live connection.
- `Set` never blocks: disabling runs `iptables`, and the caller is the ESPHome
  API goroutine. A one-deep channel, drained before refilled, hands the wanted
  state to a worker. `On` reads what the worker achieved; the worker wakes the
  live poll.
- `persist.overdub.sendspin` records what was **asked**. A failed bind still
  writes it, so the next boot retries. Entity and property may disagree until
  a restart.
- The `persist.` prefix is the persistence; a test asserts it. Off survives a
  cold reboot, with the value in `/data/property` at `0600`.
- Never written: **on**. `getprop` **failed**: off, logged, property not
  rewritten. A failed read is not an instruction, and off is recoverable from
  the switch. `Flag` reports "unset" and "could not read" separately for this.
- Whenever the surface did not come up, the daemon deletes the `tcp/8928` rule
  once. A previous run may have left an ACCEPT.
- A command for the current state does nothing: no transition, no property
  write.
- The switch is behind the ESPHome key only. It is an operator control, not a
  second credential.

### One connection at a time

- The spec ranks connections by activity, with provisional connections, a
  30-second window to declare, and pairing held beside playback.
- Implemented: **one admitted connection**. A second server reaching activation
  gets `client/goodbye` with `concurrent_attempt`, which the spec permits. The
  slot frees when the holder goes.
- Not implemented: ranking, displacing a lower-priority holder, and pairing
  beside playback. With two servers the Dot keeps the first. This is a
  deliberate floor.

### Reporting itself unavailable

- On activation of `player@v1` the Dot sends `client/state` with **`available:
  false`**, then `true` when the clock converges (about 205 ms). Both are the
  spec's precondition.
- A Dot with no player (`audio.NewChime` failed) stays false. Available would
  make the group wait for a speaker that cannot sound.
- **An `available: true` is never withdrawn.** If `OpenStream` fails the Dot
  drops chunks, logs 1 line and keeps its slot. Every reachable `OpenStream`
  failure clears at the next `stream/start`; withdrawing would trade one silent
  track for a session outside the group.
- There are two lists called `supported_commands`: `client/hello`'s
  `player@v1_support` carries `volume` and `mute` (when the Dot can set them);
  `client/state`'s player carries `set_static_delay` alone. Music Assistant
  shows its delay control only for a player whose `client/state` names it.

### Uninstalling takes the identity

`uninstall.sh` removes the Sendspin key with the ESPHome one and reads both
back. A leftover key would not look like a failure, and the token from it would
stay valid.

## Timing

### Keeping the clock

- Audio is stamped in the server's monotonic clock. `client/time` carries the
  Dot's time; `server/time` answers with 3 stamps. With the arrival time they
  give offset `((T2-T1)+(T3-T4))/2` and uncertainty `((T4-T1)-(T3-T2))/2`, in
  microseconds, never epoch time.
- The filter is **a port of the reference**, constants included: a 2-D Kalman
  filter over offset and drift with adaptive forgetting. The spec makes it
  normative, because the server plans for that error behaviour. Converged: 2
  measurements and a finite covariance.
- The Dot's clock is Go's monotonic reading, which NTP slews. `aiosendspin`
  prefers `CLOCK_MONOTONIC_RAW`. The kernel caps slew at about 500 ppm, and it
  arrives as a rate error, which the filter tracks. A step never reaches a
  monotonic clock.

#### Asking, and how often

- The spec suggests bursts (8 exchanges every 10 seconds, keep the best).
  `aiosendspin` sends one at a time on an adaptive interval, and so does this
  client, because that is what Music Assistant runs against. Best-of-a-burst is
  a second filter in front of the filter.
- **One question is outstanding at a time.** Ask, wait for the answer or 5
  seconds, rest the interval. The transport is ordered, so asking early only
  discards the sample on its way.
- The answer signal is a one-deep channel, and asking **drains it first**. A
  late answer leaves a token; without the drain the next question takes it as
  its own, and the real reply then fails the echo check.

#### What a server cannot talk this clock into

Each is refused, and each has a test that fails without it.

- **An answer must echo the question.** Otherwise a server can hand the Dot any
  offset.
- **A repeated answer is one measurement.** Otherwise one exchange meets the
  2-measurement bar.
- **Both server stamps must be in `[0, 2^50]`**, about 35 years of
  microseconds. They are peer int64s in int64 arithmetic; the reference is
  Python and does not wrap. `MinInt64` and `MaxInt64` wrap their difference to
  -1, a confident +1000 us delay.
- **A reply cannot be sent before the question arrived.** `server_transmitted`
  below `server_received` inflates the delay's certainty. The reference server
  builds its payload with `server_transmitted=0` and rewrites it at send time.
  The first 2 measurements matter most: the filter takes them unweighted.
- **A rate below `slowestRate` is refused**, and so is a mapped moment past the
  stamp ceiling. The inverse mapping divides by `1 + drift`. A server can drive
  drift to -1 by answering each `client/time` with the offset less the interval
  since the last: the filter reports convergence within 150 us and the division
  gives infinity. **This deviates from the reference**: `aiosendspin`'s
  `compute_client_time` has no guard and would raise `ZeroDivisionError`.
- **A delay of zero or less is not a measurement.** Zero is a zero-uncertainty
  sample, and the filter has no covariance floor: 2 such samples then 20 honest
  ones move the estimate under 1 us, and it reports `to within 0 us` for the
  connection. A negative delay squares into a positive variance.
- **A timestamp that did not move forward** is dropped by the filter, as in the
  reference: the prediction divides by the interval.
- Each outcome logs its own line; from outside the causes look alike.
- **A connection with no roles keeps asking.** The reference pauses only in its
  pairing flow, which this client does not offer. The roleless allowance is 30
  seconds, so at most 150 questions.
- `Session.When` maps a chunk stamp to the Dot's monotonic clock. A chunk
  before convergence is dropped and said once per connection: at 40 chunks a
  second, a line each would spend the run's budget in about 2 minutes.
- Convergence logs one line with the offset's standard deviation. Over wifi
  against Music Assistant: about 205 ms after the player role was activated,
  at 1,784, 596, 2,437 and 1,817 us on 4 installs.
- The exchange rate cannot be read from the device. The `tcp/8928` rule's
  counter stays at **0** during a live session: the stock chain accepts
  established traffic ahead of it. The counter shows only whether a handshake
  got in.

### The delay a server sets

- `server/command` `set_static_delay`. A figure outside 0 to 5,000 ms
  (`MaxStaticDelayMS`) is clamped, because the spec says clients MUST clamp,
  and the clamp is logged. Refused: no figure (absent is not 0), and no player
  role. Neither drops the connection.
- Applied as one subtraction where a server timestamp becomes a local moment.
- It applies to audio written after it. The queue keeps what it holds. The spec
  says so for lowering; re-placing a queue at every step of a drag is worse
  than the gap.
- The same figure again is not answered.
- **A taken delay is reported straight back in `client/state`, and that is what
  makes it work.** The server schedules at least `min_buffer_ms +
  output_delay_ms` ahead and counts a chunk outstanding until `timestamp +
  duration - output_delay_ms`, both from the *reported* figure.

| delay applied | reported | leads (seconds) | placed (seconds) | silence |
|---|---|---|---|---|
| 5 ms | yes | 1.716 - 1.973 | 27.83 | 2.19 seconds |
| 1.814 seconds | **no** | 1.767 - 1.973 | **0.50** | 29.53 seconds |
| 2.716 seconds | no | 0.493 - 2.371 | 6.95 | 23.18 seconds |
| 1.5 seconds | **yes** | 1.990 - 3.473 | 29.64 | 405 ms |

- In the last row the server's floor moved to exactly **1.990 seconds**, 500
  ms plus the 1,500 adopted, and the ceiling to 3.473 seconds.
- **The buffer does not pay for the delay.** A larger delay moves completion
  earlier, so the lead can reach `buffer_capacity + output_delay_ms`, and the
  jitter buffer holds that lead less the delay. `bufferSeconds` stays 2.
- A change logs 1 line per connection; the current figure rides on the
  30-second summary. Music Assistant's control is `immediate_apply`: one drag
  sent 23 values, 1.268 down to 1.097 seconds. A line each spent the 20-a-minute
  peer budget and lost 13 other lines.
- **Two writers send `client/state`** (the run loop for the server, a reporter
  goroutine for Home Assistant), so the figure is read under the lock that
  writes it. Otherwise the older figure could be reported last.

#### Keeping it across reboots

- **The spec says a client MUST keep the delay across reboots and
  reconnections.** It lives in `persist.overdub.sendspin_delay`, 30
  characters; the device refuses past 31 (docs/device.md).
- Zero is a figure, not "unset". One atomic on the `Client` holds it, seeded
  from the property the first time anything needs it. The playback path reads it
  per chunk, so a change applies mid-track.
- **At most one write a minute (`KeepApart`), from a goroutine of its own.**
  `setprop` costs about 40 ms, and the control applies while dragged: 23 values
  inline on the chunk reader would stall it nearly a second. A change wakes a
  keeper, which waits out the window and writes the current figure if it
  differs from the property.
- So every stop that can writes first: switch off, signal, stuck key, ESPHome
  listener returning, and `serve` returning an error. Each closes the listener
  and waits for the client to stop serving. `os.Exit` runs no deferred
  function, and `install.sh` stops the old daemon with `SIGTERM`.
- Each wait is bounded by `sendspinFlush` (2 seconds); a write still running
  is abandoned. That needs a write 50 times slower than any observed.
- The keeper's wake is cleared before the keeper stops. A set arriving then
  finds no keeper and writes itself, instead of posting to a dead channel.
- Still lost: a panic, `SIGKILL`, a pulled plug, and a signal during a
  switch-off already under way.
- A refused write retries on the next window, `KeepTries` (3) times per figure,
  then waits for a new figure. The failure is likely spurious: `SetNumber` reads
  back at once, and on this device an immediate `getprop` can read empty.
- With the stored figure **unreadable**, the keeper writes once at start. An
  absent property reads as 0 and does not count.
- A figure the direct write refused is primed: the keeper writes it on its
  first window.
- **Two writers reach the property**, so one mutex covers both and each reads
  `delay` inside it: the last write is the last read. Without it, a direct 900
  was overwritten by the keeper's 700. There is no snapshot to go stale.
- Open: the mutex does not cover the switch's writer. `keepOwed` retries a
  failed write, so a `setprop` refused just before a client starts can race
  the client's keeper.

#### Set from Home Assistant

- One figure, two writers: `number.<name>_sendspin_output_delay` over the
  ESPHome API, and `set_static_delay` over Sendspin.
- **Last writer wins.** The figure describes this Dot, so the operator is as
  authoritative as the server. The entity shows what is applied.
- Both writers share the keeper's window when a client is up.
- A local figure not reported leaves Music Assistant scheduling for the old
  one: the 0.50 seconds placed against 29.53 of silence in the table.
- The same figure again writes nothing and logs nothing. The `client/state`
  sent on activation carries it anyway.
- A set from Home Assistant logs 1 line, on the API's side: one command per
  operator action, not per drag step.
- **Reporting runs off the caller's goroutine.** The write's deadline is 150
  seconds, and the caller is the switch's only worker. A per-session reporter
  takes a wake and reads the figure fresh, so rapid sets coalesce.
- Per session, not per client: a server can drop and retake the player role on
  one connection. A figure moved while no session holds is reported when the
  role returns. A wake in flight when the hold moves is dropped.
- The number runs on the switch's worker. On 2 workers a figure can land while
  an `enable` is creating a client: persisted, and never sent to the client
  that plays. A figure queued behind an `enable` is late, which is recoverable.
- **`available` is repeated, not re-decided**, on every `client/state`.
- **With Sendspin off, the control still works.** The switch holds the figure,
  clamped the same, and the next client is built from it.
- With Sendspin off, a set wakes `liveWake` when the figure is taken and again
  when the write lands, so the control does not show the old value while the
  write window holds it. With a client up the figure goes to the client, and
  the property write is the keeper's.
- The switch writes under the same window, using `KeepApart` and `KeepTries`.
  An automation recomputing the delay would otherwise write flash per value.
- Held and stored are two figures. The control shows what is held at once. What
  is owed is written when the window closes, when the switch turns on (`enable`
  reads the property to build the client), and when the daemon stops.
- Deciding, making and recording an owed write is one step under its own mutex,
  or two callers spend two flash writes. Not the field lock: a `setprop` is 40
  ms and the entity's poll reads through that lock.
- A write records what the **property** holds. It updates the switch's figure
  only when a client made it; unconditionally, a write in flight would restore
  its own figure over one set meanwhile.
- Switching off takes the client's figure, because its keeper writes it on the
  way out. Otherwise nothing is owed and the operator's older figure is refused
  as a repeat.
- `keptDelay` returns `(0, false)` when `getprop` could not run, unlike an
  absent property. A switch-on falls back to the switch's figure and marks only
  the disk unknown. At boot there is nothing to fall back to.
- An unreadable disk earns one write, not an exemption from the window.

#### Measured with a track playing

- A figure set in Home Assistant applied mid-track, before the next summary.
  Music Assistant's first chunk was `due in 1.475334s` against a 985 ms delay:
  500 ms plus the figure. Its web page shows the old figure until reloaded.
- The control's arrows send one command per click, about 200 ms apart. 11
  commands in 2 bursts inside a minute made 1 flash write, read back after a
  restart.
- A mid-track change costs a gap its own size: a 489 ms jump placed 490 ms of
  silence.
- A delay Music Assistant is told can land at the next stream boundary, up to a
  minute later. Set at both ends inside that minute, Music Assistant's wins.
- Switch off: 21 commands over 29 seconds made **2** writes, the first at once
  and the second 61 seconds later with the final value.

### The volume a server sets

- A player that names no `volume` command is **excluded from group volume**:
  the spec applies the delta only to players that support it.
- **Where it is offered is `aiosendspin`'s answer, not the spec's.** The spec
  moved all 3 commands into `client/state`; 9.1.1 refuses `volume` there and
  the handshake never completes.
- **`mute` is offered too.** Setting it grabs the volume keys while held, so a
  press lifts the mute instead of moving an inaudible level.
- The ESPHome server reads and sets the level. `serve.go` wires them, so
  neither package imports the other. `CanSetVolume` answers for both.
- **The Dot's buttons reach the server.** A poll (`volumeEvery`, 2.5 seconds)
  reports a level that differs from the last one reported. Comparing against
  what was reported catches a change between the first state and the first
  tick. It is its own `dumpsys audio`; the ESPHome one stops when Home
  Assistant unsubscribes.
- **The reported level is the device's**, not an echo. The scale is 30 steps:
  65 becomes step 20 and is reported as 67 within 2.5 seconds.

### Group sync

- `available: true` is not the finish line: a player in the group and 0.25
  seconds behind is worse than one that never joined.
- Settled on one Dot: a stream placed 134 to 164 ms after it opens, against a
  pipeline 131 to 144 ms deep; 4 runs placed every frame with the mapping
  still.
- **3 Dots, one server: one sound.** Pipelines 144, 142 and 141 ms; every clock
  under 1 ms; no dropped chunk, re-placement, refused write or missed reading.
- They share a build, so they could be wrong together. The test for that is a
  Dot against a player that is not a Dot.
- Against Music Assistant's web player (Firefox, Mac): "not perfectly aligned
  but not annoying", so more than a few ms and less than an echo. This is not
  evidence against the Dot: the browser is the weaker end.
- The Dot's figure is arithmetic: 8 blocks of 10 ms plus 58 to 64 ms of HAL
  buffer is 138 to 144 ms, against 141 to 144 measured. The rest (I2S, codec
  group delay, amplifier) is microseconds.
- A browser's is not. Web Audio to CoreAudio is tens of ms on a Mac, varies
  with device and buffer size, and must be declared through
  `AudioContext.outputLatency`. The browser was also sent FLAC stereo, the Dots
  PCM mono.
- To resolve it: nudge `CONF_SENDSPIN_STATIC_DELAY` until aligned, which sizes
  the gap but not its owner. 1 metre of path difference is 2.9 ms.
- Measured with a microphone: each speaker in its own group, the same chirp
  scheduled 0.3 seconds apart on one server clock, a matched filter on the
  recording. 2 Dots on their speakers played 4.5 ms apart, within 0.5 ms over
  20 seconds.
- A Dot playing through a JBL Go 3 settled a median 2.9 ms from a Dot on its
  speaker, and within 15 ms; docs/audio.md has the Bluetooth path.
- Two Dots test that the offset is the same on every unit. A Dot against
  another player tests that it is right.
- By ear: within about 5 ms sounds like one speaker, 5 to 20 ms combs, past
  that is an echo.

## Peers

### What a peer can make the daemon do

- The Sentinel PSK is public and `client/init` sends the `client_id` in
  cleartext, so **any host that can route to `wlan0` can complete the
  handshake** and reach the logging code.
- Peer strings are cut and rendered with `%q`. A newline in `group_name` would
  forge log lines; `%q` renders a 65 KB name at about 4 times its size.
- Peer bytes also arrive inside errors: a `json` error quotes the literal,
  folded into a `%v` no `Cut` touched. So `internal/untrustedlog` caps every
  line at 512 bytes. Three bounds: 64 bytes per string, 512 per line, and the
  line budget.
- The budget is separate from the ESPHome API's but **shared** with the
  switch's own lines, so a peer that spends it silences the switch's record. A
  third `Log` would double what reaches disk.
- Per connection, a line is logged once per call site **plus** rendered values,
  up to 16. The call site is in the key because the peer chooses the values: a
  group named `stream/start` would otherwise silence the real line. The set
  records a kind whether its line was written or dropped.
- `group/update` and the 2 lines a server's declaration draws go through the
  same gate. Music Assistant sends `group/update` on every playback change;
  logged each time, a session would exhaust the ceiling in about 4 hours.
- At most 8 connections (`maxConns`).
- The timing windows are unexported `Client` fields that fall back to constants
  when zero. Only tests set them; package variables would race connections a
  previous test is still closing.
- **The handshake deadline is absolute**: 30 seconds to finish, then 30 more to
  declare, lifted only by an activation that takes a role. Nothing a peer sends
  refreshes either; otherwise a message every 29 seconds holds a slot forever.
  After activation, a 150-second idle timeout refreshed by any frame, with a
  ping every 60.
- Giving up every role starts a timer that closes the connection. Releasing the
  slot is not enough: 8 lingering connections fill `maxConns` and refuse Music
  Assistant.
- The roleless allowance is per connection, not per episode, so cycling a role
  earns no fresh window.
- It is a timer because the alternatives fail. A deadline is overwritten:
  `readHeader` and `write` arm their own on every call. A check between messages
  is never reached: `Conn.Read` answers pings and pongs internally, so a peer
  that only answers keepalives stays inside one `Read`.
- `SetDeadline` also sets the write deadline, so an absolute window would fail
  writes 30 seconds into a healthy session. `readHeader` and `write` arming
  their own deadlines prevent that.
- **The goodbye takes two bounds.** `Client.Close` narrows the idle window to
  `goodbyeWait`, which bounds every later write. A write already in flight keeps
  its old deadline (3.1 seconds against a 100 ms budget), so `Close` also sets
  the socket's write deadline, interrupting the syscall. A partial frame is the
  honest result.
- **And a watchdog.** A session is tracked before `Greet` returns and `run`
  starts by arming a 30-second deadline, so a `Close` in between is replaced.
  Each goodbye drops the connection after twice the budget.
- `Close` says every goodbye at once. Serially, 4 stalled sessions took 2.4
  seconds, all ahead of `DenyTCP` and the flag write.
- **A failed write ends the connection.** `seal` advances the Noise nonce
  before the socket write, so after a failure every frame fails authentication
  (`chacha20poly1305: message authentication failed`), like a wrong key.
- The 2 errors above `seal` (a payload that will not marshal, `WriteTyped`
  refusing the fragment type) do not advance the nonce, and neither is
  reachable from these writers.

### The reference server

Every other test drives both sides from one reading of the spec, so a misreading
agrees with itself. `TestInteropWithTheReferenceServer` runs a real
`aiosendspin` server against this client:

```sh
SENDSPIN_INTEROP=1 go test -run Interop ./internal/sendspin/
```

- CI runs it in its own job, since it alone needs the network. With the
  variable set it **fails** when `uv` is missing.
- Pass `--client-id=<value>` as one argument: about 1 in 64 `client_id`s start
  with `-`.
- The suite shares the standard logger. A `Client` left running past its test
  writes into a later test's count (about 1 in 40 runs).
- It asserts what the client made of the answers: the server fails under 3
  `client/time` exchanges, and the Go side fails unless the daemon logged its
  clock agreed. The count alone passes a client that refuses every answer.
- It fails on `non-compliant` or `Malformed` in the server's warnings, because
  one difference below is tolerated rather than reported.

Differences between the spec at `8fc2f8f` and **aiosendspin 9.1.1**:

| what | spec | aiosendspin 9.1.1 |
|---|---|---|
| noise message 1 payload | `psk_id`, `psk_category` | `psk_id` alone |
| `supported_pair_methods` | object by method | `list[PairMethodDescriptor]` |
| `supported_commands` | in `client/state` | also in `player@v1_support` |
| fixed output delay | `output_delay_ms` | `static_delay_ms` |
| its command | `set_output_delay` | `set_static_delay` |
| a player's preferred format | `format` in `client/state` | `stream/request-format` |

- All point the same way: the document is ahead of the library. The spec's
  "Clarify output delay in the player sync target" is dated 2026-09-08. This
  daemon follows the library, because the library is on the wire.
- **An absent `psk_category` is the old shape.** The `psk_id` is matched
  against every candidate, and a miss falls back to the Sentinel. An empty
  string reaching the `default` arm kills every real handshake as
  `unknown psk_category ""`. A present category binds as the spec requires.
- **No pairing method is advertised.** The two shapes cannot share one key,
  and the flow does not exist.
- **`supported_commands` goes in both places, with different contents.**
  `player@v1_support` takes `volume` and `mute`; `client/state` takes only
  `set_static_delay`. Either in the other is a validation error.
- **`static_delay_ms` goes on the wire.** The spec name is dropped as unknown,
  and the server logs `non-compliant client: initial client/state omitted
  required player timing fields` and, with `allow_noncompliant_clients` true by
  default, carries on. That is why the interop test scrapes warnings.
