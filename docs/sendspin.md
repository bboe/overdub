# Sendspin

Sendspin (ex-"Resonate", Open Home Foundation) is the multi-room audio protocol
Music Assistant ships as its native playback provider. `internal/sendspin`
implements the **client** side of it, so the Dot's speaker joins a synchronised
group rather than only answering Home Assistant as an ESPHome device.

Spec: github.com/Sendspin/spec, pinned at commit `8fc2f8f` (2026-09-12). The
repository carries no tags or releases, so a commit is the only citable reference.
Read it rather than this page for what the protocol says; this page carries what
was measured here and what was decided against.

The table at the end of this page records differences against that revision. The
interop test is what checks them continuously; the table is a snapshot.

The commit matters because that repository carries **no tags and no releases** and
is revised continuously -- there is no "version 1.2" of this protocol to cite,
only a commit. `version: 1` on the wire is the *core* version and does not move
when the document does: every difference in the table at the end of this page sits
inside declared version 1. So an unpinned citation rots silently, and a table of
differences nobody can re-check reads as current when it is not.

## The shape

Plain `ws://`, and the confidentiality is end to end inside the payloads rather
than in the transport: Noise `KKpsk2`, with the **server as initiator** whichever
side dialled. Both static keys are known in advance -- `client_id` and
`server_id` are the base64url Curve25519 public keys -- and a pre-shared key is
mixed in at the end of the handshake's second message.

Connections go one of two ways and a client must pick exactly one. This is
**server-initiated**: the Dot advertises `_sendspin._tcp.local.` on 8928 with a
`path` TXT record, and Music Assistant dials it. That reuses the mDNS responder
and the firewall helpers this tree already has, at the cost of the multi-server
admission rules, which the client-initiated direction leaves
implementation-defined.

The role is `player@v1`, and the format offered is **`pcm`, 48000 Hz, mono,
16-bit**. Mono because the Dot has one speaker, so a server downmixing for us
beats AudioFlinger doing it later, and it halves the stream to about
0.77 Mbit/s. PCM because servers must support it, so nothing here decodes
anything -- see the mp3 paragraph in `docs/constraints.md` for why that matters.
It also happens to be exactly what `chimePCM` already generates, so the chime
and the stream share one format and one player.

## The WebSocket is hand-rolled

There is no WebSocket in the standard library, and this tree does not import one.
Measured on a copy of the real daemon, built for linux/arm:

| variant | delta |
|---|---|
| a `net/http` server, no library | +15 KB |
| `gorilla/websocket` | +200 KB |
| `coder/websocket` | +205 KB (2.13%) |

Only about 56 KB of that is the library's own code. Most of the rest is
`compress/flate` for permessage-deflate, which this protocol can never use --
every application byte is a Noise ciphertext, and ciphertext does not compress.
`CompressionDisabled` does not remove it either: measured, it saved **zero
bytes**, because `CompressionMode` is a runtime value and the linker cannot prove
the path dead. The flate writer is in the binary twenty symbols deep whatever you
ask for.

A hand-rolled layer lands near 15-25 KB by the rate the rest of this tree
compiles at -- `internal/esphome` is 2,255 lines for about 122 KB -- and it needs
no `net/http` server at all, because it upgrades off a raw listener the way
`esphome.Server` already accepts one.

**Size is not the argument, though.** 205 KB on a 10 MB binary is affordable.
The argument is the one `docs/constraints.md` makes for owning code: a peer-facing
parser is worth *not* owning when a passing test suite proves less than it looks
like it does, which is why `dnsmessage` is imported for mDNS. That reasoning does
not reach here, and the reason is the AEAD. mDNS parses unauthenticated input
from the whole subnet, with a compression-pointer graph, and nothing behind it to
catch a mistake. Every Sendspin application byte is authenticated: a framing
error yields a payload that fails Poly1305, and the connection aborts. It fails
closed. The only cleartext this layer ever carries is the init exchange and the
two `noise/handshake` frames -- four frames, all small and length-capped.

So the risk left is ours, and it is bounded: an off-by-one in masking or in
reassembly. Two things hold it. `FuzzFrameReader` runs the reader over arbitrary
bytes -- 9.1 million executions found nothing. And this needs to interoperate
with one peer rather than pass general RFC 6455 conformance, which is a much
smaller claim than a library has to make.

What the reader refuses, each with a test: an unmasked frame (a client MUST mask),
a reserved bit, a control frame that is fragmented or over 125 bytes, a length
that is not minimally encoded, a 64-bit length with its high bit set, a
one-byte close frame, a continuation with nothing in flight, a new message
arriving mid-fragment, an unknown opcode, and a message built from more frames
than the ceiling allows. Lengths are checked against the read limit **before**
the payload is allocated, and again as fragments accumulate, so neither a single
frame nor a long chain of them can make the daemon allocate past the limit.

Bytes are not the only budget, which is why there are frame counts as well. A
chain of empty continuation frames allocates nothing, so it trips neither length
check, and costs a read and a loop apiece: the peer spends almost nothing and the
daemon spends the rest of the connection. Two counters end it, both 4,096 and
each with its own test. `maxWSFrames` bounds the fragments of one WebSocket
message; `maxMessageFrames` bounds the Sendspin frames reassembled into one
message above it. They are separate because the layers are, and a chain still
legal at one is refused at the other. Neither number is anywhere near something
legitimate: both layers refuse a message past 65,535 bytes as it accumulates, so
two frames carry the largest message either layer allows, and the counters exist
only to end chains of near-empty ones.

The upgrade is bounded before any of that, though not by a single number.
`maxRequestBytes` (8,192) is checked inside the line reader, so it caps each line
rather than the whole request; with `maxRequestLines` (64) alongside it the
request is bounded at about 512 KB. What that buys is that a peer cannot hold the
socket open feeding headers one at a time.

The frame writer takes a mutex, and not for the framing. Each frame is one
`Write`, and Go holds a per-connection write lock of its own, so frames still
arrive whole with `Conn.mu` removed -- measured over a real socket with payloads
the kernel has to split. The mutex is there for the pair either side of the
write: arming the deadline and then writing have to be one step, or one caller
arms a deadline that another caller's write spends.

## Identity, and the one secret that is not staged

`client_id` is the base64url form of a Curve25519 public key, 43 characters, and
it is both the routing identifier and the static key the Noise handshake
authenticates. A `pairing PSK` sits beside it: per device, from a CSPRNG,
long-lived, and not consumed by a successful pairing, so it can pair the Dot with
any number of servers.

**The daemon generates both itself, on first run, and they never cross adb.**
That is a departure from the ESPHome key, which `install.sh` generates locally and
stages through `/data/local/tmp`. Two reasons. Deriving an X25519 public key is
not something shell can do, so the private key would have to be made off-device
and pushed. And `docs/pitfalls.md` already records what staging a secret costs:
`adb push` does not carry the local mode, `/data/local/tmp` is `0771` with the
`o+x` that lets any uid reach a file by name, and a key written locally as `0600`
lands there `0666`. The ESPHome key pays for that with a private staging
directory created and removed around the push. A secret that is never pushed
needs none of it.

The key file is 64 raw bytes: the static private key, then the pairing PSK. The
public key is derived rather than stored, because a stored copy can go stale
against the private key it claims to match. `flynn/noise` derives it as
`curve25519.X25519(priv, Basepoint)`, and `crypto/ecdh` -- already in this tree's
dependency closure, so not a new import -- computes the same value.
`TestStoredPrivateKeyAgreesWithTheNoiseLibrary` pins that, because the two
disagreeing would not fail: it would give the Dot a `client_id` no server could
match against the handshake, which reads like a rejected key.

A load **refuses** a file any other uid can read, and that check is not
decoration: the pairing token is exactly `client_key || pairing_psk`, so a
readable key file is a readable token, and a token is all anyone needs to pair as
this Dot. Group counts as well as other -- `0604` is the whole hazard and `0640`
hands the key to `shell`.

A key file of the right length is not yet a key. Either half arriving all zeros is
refused rather than used: X25519 clamps whatever it is handed, so a zero private
key yields a public key and a working handshake, and a zero pairing PSK is a token
anybody can guess. A length check alone passes a 64-byte file of zeros, and
nothing downstream of it complains.

The file arrives whole or not at all. It is written to a sibling named
`<key>.new-<random>` at `0600`, flushed with `fsync`, closed, and then put in
place with `link`, which fails if the key already exists. Two daemons racing on a
first boot -- which the five-second supervisor respawn makes reachable -- then
either win the link or read a file that is already complete. Creating the real
path first and writing into it afterwards would leave the loser reading nothing,
which is why it is not done that way. What this does not survive is a kill
between the write and the link: the sibling stays, holding a valid but unused
key, so `uninstall.sh` sweeps `<key>.new-*` as well as the key itself.

## The pairing token

Clients must implement the Pairing PSK method and no other, which is the one with
no PAKE round and no pairing code: the operator carries a token from the Dot into
the server, out of band. The token is `SP:`, a version character, and a base32
body.

Its encoding has one step that looks like a mistake and is not: after
base32-encoding per RFC 4648 and stripping the `=` padding, **every `2` becomes a
`9`**. Decoding maps `9` back to `2`. The reason is not the QR alphanumeric set:
base32 emits only `A-Z` and `2-7`, every one of which is already in that set, so
the transliteration buys nothing there. Why the spec does it is not recorded in
the spec. It is recorded here rather than matched: no encoder or decoder ships,
so nothing in this tree spells a token either way. It is written down because a
token is only useful if both ends spell it the same way, and the published vector
is what settles that -- meeting the vector without the transliteration is
impossible, and getting it wrong produces a token that looks right and decodes to
rubbish.

Two published constants are asserted against the spec rather than trusted: the
Sentinel PSK, `SHA-256("sendspin-sentinel-psk-v1")`; and its `psk_id`, which is
the general rule `base64url(SHA-256("sendspin-psk-id-v1" || PSK))` applied to it.
Both matched on the first run, which is the only reason the derivations above can
be stated as facts.

The sentinel is a poor vector for one thing, though, and a second is asserted for
it. Its `psk_id` happens to carry neither `-` nor `_`, the two characters where
base64url and standard base64 differ, so the wrong alphabet would encode it
identically and the test would agree. `SHA-256("5")` used as a PSK produces a
`psk_id` carrying both, so it separates them. The wrong alphabet would otherwise
fail about three pairings in four with nothing to say why.

**No encoder or decoder ships.** Nothing can consume a token until the pairing
flow exists, and a token the daemon can neither show nor receive would be code
that only looks like a feature. The encoding is written down above because the
spec's version-0 reference vector settles it, and the pairing pass should not
have to derive it again.

What a decoder will owe when it arrives, from the spec: leniency about what an
operator types -- trimmed, upper-cased, a missing `SP:` tolerated -- and
strictness about everything else, refusing an unknown version, a body that is
not base32, and a payload shorter than the version defines. Payload bytes
**beyond** the 64 defined are ignored rather than refused, because the spec
reserves them for later versions.

## The handshake, and three inversions in it

**We are the WebSocket server and the Sendspin client, and we speak first.**
Music Assistant discovers the Dot by mDNS and dials it, so the Dot accepts the
TCP connection -- and then sends `client/init` as the first application message
anyway. Accepting a connection and opening a conversation on it is not the usual
pairing of roles, and reading the sequence as "whoever dialled speaks first" gets
it backwards.

**The Noise roles are the other way round from the WebSocket roles.** The server
is always the Noise **initiator** and the client the **responder**, whichever
side dialled. So the Dot accepts the socket, sends the first application message,
and is the responder in the handshake carried inside it.

**As responder we encrypt with the second CipherState and decrypt with the
first.** `Split()` returns the pair in Noise's canonical order -- the first is the
initiator's sending key -- and `flynn/noise` returns them in that same order from
both `WriteMessage` and `ReadMessage`, so the two sides must read the same pair
oppositely. Getting it backwards does not look like a bug in framing or in
direction; it looks like a wrong key, because that is what a wrong key is. The
two-way transport tests pin it by running a real server side rather than by
reading the library.

## What made `flynn/noise` usable here

`KKpsk2` mixes the PSK at the end of the **second** message, and the server names
the PSK it used in the payload of the **first** -- so a client cannot know which
key it needs until after it has decrypted something. That works because the
message-1 payload is encrypted without the PSK mixed in, and because
`flynn/noise` explicitly permits a `psk{2+}` handshake to be constructed with no
key set and take one later through `SetPresharedKey`. Its own source says so:
"for psk{2+} we may not know the correct psk yet so it might not be set." Without
that the library could not be used for this pattern at all, and the whole plan
would have needed a different one.

The prologue is the **exact wire bytes** of `client/init` followed by
`server/init`, not a re-encoding of the parsed messages, so both are kept as
received and sent rather than marshalled a second time. A re-encoding that
differs by one space fails the handshake and says nothing about why.

## A miss and a misbinding are not the same thing

The client compares the server's `psk_id` against its own candidates **of the
declared category only**: a key held as a pairing PSK but declared `lt` is a
miss, not a match, and the spec is explicit that the category binds.

A **miss** -- no candidate matches -- completes the handshake with the Sentinel
PSK rather than failing. That is the Sentinel Fallback, and it exists because a
client that lost its pairing record should still reach a server that can offer
re-pairing. The Sentinel is a published constant and authenticates nobody; what
it buys is a session in which pairing can happen.

A **misbinding** -- a `psk_id` matching a stored long-term key that was issued to a
different `server_id` -- must fail the handshake rather than fall back. It is not a
lost record; it is a key being used by something other than what it was issued to,
and the Sentinel fallback would paper over exactly the case worth refusing. **That
rule is not implemented, because there is nothing for it to check.** No long-term
key can exist until pairing does, so the store that would hold one is not here
either: `PSKSet` carries the pairing PSK alone. The rule is written down because
the pairing flow has to bring it back, and re-deriving it from the spec later is
how it gets got wrong.

The `server_id` a session records is re-encoded rather than kept as it arrived,
which is what would make that check a comparison of keys rather than of text.
`base64.RawURLEncoding` ignores the unused bits of the final character, so more
than one string decodes to the same key: measured on a 32-byte key, **four**
spellings decode identically, the canonical one and three others. `DecodeID`
takes the key out and `EncodeID` puts it back in the single spelling the Dot will
ever store or compare. Nothing stores one today, for the reason above; what this
costs now is one re-encode per handshake.

What *is* implemented is that `lt` remains a category the Dot accepts on the wire.
A server may declare it -- one that paired with a different client, or a build
whose categories we do not store -- and the answer is a miss and the Sentinel
fallback, not a refusal. Dropping `lt` from the accepted set instead sends the
handshake into the unknown-category error, which a test catches by the connection
timing out.

## Two fragmentation layers, and they are unrelated

WebSocket has its own fragmentation, and Sendspin defines another inside the
encrypted channel: message type `1`, with a flags byte, and the original type
carried on the first fragment only. Both are implemented, they nest, and they are
not the same mechanism -- a message can arrive whole at the WebSocket layer and
still be one Sendspin fragment of several.

The inner layer exists because a Noise transport message cannot exceed 65535
bytes, which leaves 65518 for a payload once the 16-byte tag and the type byte
are taken. Reassembly holds one message per direction, refuses a first fragment
while one is in flight, a continuation with none in flight, a plain data frame
arriving mid-message, a reserved flag bit, and a fragment naming the fragment
type as its own -- each with a test. The accumulated size is checked on every
fragment, so a long chain cannot grow past the ceiling one kilobyte at a time.

The read limit is 2048 bytes through the cleartext phase, where every legitimate
message is a few hundred, and opens to 65535 only once the channel is encrypted.

## What the matched PSK is allowed to buy

`server/activate` declares what a connection is for, and which declarations are
legitimate depends entirely on **which PSK matched during the handshake**. The
table is short and the consequences are not:

| matched | allowed activity sets |
|---|---|
| long-term | `[]`, `['playback']` |
| pairing | `[]`, `['pairing']` |
| Sentinel | `[]`, `['pairing']`, and `['playback']` only with unpaired access enabled |

Playback and pairing are mutually exclusive, so no set holds both. A
pairing-PSK connection is therefore **never** playback-capable, whatever it
declares, and a test asserts that rather than leaving it implied.

Refusing correctly is the fiddly part, because two different refusals look alike
from the code and mean opposite things to an operator. The spec settles it with a
worked example, which is checked here as a test: a Sentinel connection to a client
with unpaired access **disabled**, sent `activities: ['playback']` and
`active_roles: ['player@v1']`, is refused `pairing_required` -- because enabling
unpaired access would have made it admissible, so the operator's fix is to pair
or to enable it. The same connection sent `activities: ['pairing']` with the same
roles is refused `unauthorized` -- because no unpaired-access setting makes a
pairing connection carry roles, so nothing the operator toggles will help. One
reason says "you need a credential", the other says "you asked for something that
cannot exist". Answering the wrong one sends the operator to the wrong place.

There is a third refusal that is not a refusal of the connection: a `pairing`
activity naming a method the matched PSK disallows, or one this client does not
offer, gets `pair/abort` with `method_not_supported` and **leaves the connection
open**. The rules are applied in that order -- `pairing_required`, then
`unauthorized`, then `method_not_supported` -- because more than one can be true
at once.

`active_roles` is what a first activation is expected to carry, and it persists
across later ones that omit it. A first activation that omits it is not refused,
because the spec does not say it must be: the roles are read as empty, which is
the same state a server reaches by giving every role up.

The asymmetry to watch: if a later activation changes `activities`
so the connection is no longer playback-capable and **does not** resend
`active_roles`, the persisted roles are treated as empty rather than the message
being rejected. If it resends them explicitly on such a connection, that is
`unauthorized`. So the same end state is reached by silence and refused when
stated, which is deliberate: silence is a server narrowing a connection, and a
statement is a server claiming something it may not have.

## What the Dot declares

`player@v1`, one format -- `pcm`, 48000 Hz, **mono**, 16-bit -- and
`unpaired_access` enabled, which is what lets Music Assistant reach playback over
a Sentinel-keyed connection once its operator approves the Dot, with no pairing
exchange at all.

Four numbers go out with it, and a server plans playback around them, so each one
is wrong silently rather than loudly. `buffer_capacity` is two seconds of the
declared format, 192,000 bytes, derived from the format rather than written down
beside it, and a test holds it to whole seconds. `min_buffer_ms` is 500.

`static_delay_ms` is **0**, and the reason is worth reading before changing it,
because the field looks exactly like the place to declare the ~95 ms that
docs/audio.md measures between handing the player a frame and hearing it. It is
not. The spec is explicit: the delay it carries is the one *past* the device's
audio port -- an external amplifier, a powered speaker -- and "processing delays
before the port (DAC latency, audio buffers)" are the client's own to compensate.
A Dot's speaker is on the near side of that port, so the honest declaration is
zero and the delay inside the Dot is compensated here rather than announced. Not
by subtracting a constant, either: the player is asked how far ahead of the
speaker it is and the audio is placed against the answer, which is 131 to 144 ms
rather than the 95 ms a constant would have carried. docs/audio.md says why those
two numbers differ and which one a scheduler needs.

Declaring it anyway would appear to work, which is what makes it a trap, and it
would be one operator click from silence-shaped breakage: the field is settable by
a server through the command `set_static_delay` -- the library's name, the spec
calls it `set_output_delay`, which is the same rename as the field it sets --
Music Assistant exposes it as `CONF_SENDSPIN_STATIC_DELAY`, and it is the knob
somebody turns to *zero* when they notice this speaker has no external amp. A
compensation declared here would vanish with it, and the Dot would be 95 ms late
with nothing in any log to say why. The spec's own accuracy target is stated after
removing this field, which says plainly that it is not part of what the client is
supposed to get right.

**The delay is applied once, at the client, and an earlier reading of this page
had that wrong.** It said both ends subtract it, citing
`effective_ts_us = entry.timestamp_us - role.get_static_delay_us()` in
`aiosendspin`. That line is real but it is not the wire: it appears in a log field
and in the late-drop decision, and the timestamp the server actually writes into a
chunk is the stream's own, untouched. It has to be. The delay is per client and a
chunk is one message for a whole group, so a server cannot shift the stamp for one
listener without shifting it for all of them. What the server does with the figure
instead is add it to its send-ahead floor -- `max(min_buffer, required_lead) +
static` -- so the audio arrives early enough for the client to still be able to
move it. The client is what subtracts: the reference does
`compute_play_time = client_time - static_delay`, handing audio over that much
sooner so a delay past the port makes it come out on time.

That inverts the sign somebody guesses from the name. A larger delay makes this
player sound **earlier**, not later, because the figure describes latency the
player is about to add rather than a wait to perform.

`required_lead_time_ms` is **350**, and it is the one number here derived from
hardware rather than from the format. It says how far ahead of playback a server
must deliver, and the jitter buffer that now exists needs two things before it can
place a server's first chunk: the mapping between a frame and a moment, which
takes 134 to 164 ms of silence to settle, and the pipeline the player and the HAL
already hold, which measured 131 to 144 ms. A first chunk due sooner than those
together is dropped, measured: at a 200 ms lead the first 118.7 ms of a track went
and everything after it played. docs/audio.md carries the runs.

It changes nothing on the wire today, which is the point of declaring it anyway.
A server sends each chunk `max(min_buffer_ms, required_lead_time_ms) +
static_delay_ms` ahead for a **buffered** stream, so the 500 ms above still
governs and still covers this. For a **live** stream `aiosendspin` floors at
`min_buffer_ms + static_delay_ms` and ignores the lead entirely, on the grounds
that a realtime queue cannot grow once playback starts and extra lead would only
add latency -- so a live source is covered by the 500 ms alone, which is still
past what this player needs. `DEFAULT_INITIAL_DELAY_US`, 250 ms, is neither of
those: it is the fallback for a stream with no audio roles at all.
What the field buys is that the two are no longer the same claim: a `min_buffer_ms`
derived downward later cannot silently take the start of every track with it.

## The three messages that bracket a stream

`stream/start` opens one, `stream/end` ends it, `stream/clear` throws away what
was buffered and leaves it open. Each one now reaches the player as well as the
session: a playable `stream/start` opens an `audio.Stream` on it or takes up the
one already there, `stream/clear` empties its jitter buffer without closing it,
and `stream/end` **throws away what it still holds** and then goes.

**`stream/end` is a discard.** The spec says clients "MUST stop output and clear
its buffers unless that role explicitly defines different completion behavior",
and the player role defines nothing for `stream/end`. Player Buffer Accounting
agrees from the server's side: a player `stream/clear` or `stream/end` "resets
it, removing previously sent chunks".

This tree drained for a while, on two readings of `aiosendspin` that do not
hold. `PushStream.stop`'s "call `clear()` first" is scoped to its
`keep_stream=True` branch, which *skips* sending `stream/end`. And the reference
client's `_handle_stream_end` touches no audio because `connection.py` holds
none. One spec line does read the other way -- `server/activate` clears buffers
"even if an earlier `stream/end` allowed buffered data to finish playing" -- but
no role defines that behavior, so the MUST stands.

Draining cost a pause the whole buffer, since Music Assistant sends a bare
`stream/end` there and moves the group to `stopped` 89-850 us later, exactly as
at a queue end. Measured on a Dot: a pause discarded **1.817 s** and went quiet
in 30 ms, where draining took 1.853 s. A track transition and a track that ran
out discarded **0 s** -- MA stops sending, lets the buffer empty, and only then
ends the stream, so nothing is clipped off a natural end.

A finished stream drops its queue, refuses new audio, and retires at the next
block. The 134 to 164 ms already inside the player still plays, being past the
jitter buffer. The close report counts what was discarded, so how early a server
ends its streams is readable rather than inferred.

Giving the player role up, a format this client cannot take, and the connection
going away **do** stop it outright -- and the first of those is gated on the
player role being gone rather than on every role being gone, which is a
distinction no test here can reach. `supportedRoles()` has one entry, so a role
set is either `["player@v1"]` or empty and the two predicates cannot be told
apart today. They come apart the moment a second role is added: a server dropping
the player role while keeping the other one would leave the stream attached for
the life of the connection, four seconds of queued audio playing out for a role
that was withdrawn. `Session.Activate` already uses the narrower predicate, and
this now agrees with it. What stays keyed to holding **no** role is the
connection-level allowance below, which is a different question from whether
there is any audio to stop. The last of the three is a `defer` rather than
a case of its own -- a stream left attached is a writer feeding silence to a
player for the life of the daemon, which holds the amp awake and never reaches
standby. The difference is the whole point: those three are the stream being taken
away, and `stream/end` is the stream finishing.

A **repeated** `stream/start`, or one arriving before the writer has retired the
last, takes up the stream that is there rather than opening another. The
timestamps are absolute, so the audio places itself in the same timeline, where a
fresh stream spends another 134 to 164 ms learning a mapping that had not changed.

After a `stream/end` that window is one block wide, since the queue is empty and
the writer retires an empty stream at its next pass. Music Assistant restarts 60
to 90 ms later and re-anchors -- as it did before the queue was discarded, because
it ends a transition holding nothing and an empty queue retired the stream then
too. So carrying on is what a `stream/start` on a stream nobody ended gets, which
is the case the spec asks servers to use for a track transition anyway.

The format check is the substance of `stream/start`. Its player object names a
codec, a sample rate, a channel count and a bit depth, and anything but the
`pcm` 48 kHz mono 16-bit this client advertised is refused. A server should never
send one, since it picks from `supported_formats`, so a mismatch means something
is wrong rather than something is negotiable -- and playing it anyway is noise or
the right audio at the wrong pitch, neither of which announces itself. A refused
format also **closes** a stream that was open rather than leaving it, because the
alternative is audio that follows being taken for the old format.

A `stream/start` carrying no player object at all is not an error: the same
message opens artwork and visualizer streams, and a server sending one to a
client that only plays audio is just talking about something else.

**Roles are matched by family.** `stream/clear` and `stream/end` carry role names,
and `aiosendspin` validates them against families -- `player`, `visualizer` --
while `active_roles` elsewhere names versions like `player@v1`. So both are
accepted and anything before the `@` is what decides, since a server naming the
version is not wrong and ignoring it would drop a real end.

An **omitted** list means every role, which is the common case and the one a server
sends at the start of a session. An **empty** list means no role at all, and ends
nothing. The reference client gates on `roles is None or "player" in roles`, so the
two readings differ by exactly one message a server may really send, and the Go
type has to carry the difference: a `[]string` collapses `null` and `[]` onto the
same empty slice, which reads an empty list as every role and closes a stream the
server left running. `*[]string` is what separates them, the same way
`active_roles` does.

A `stream/clear` is this client's only when it holds the player role and a stream
is open. Nothing is buffered otherwise, so a clear arriving before a
`stream/start`, or after the format of one was refused, has nothing of ours to
throw away -- and reporting one would spend a peer-driven log line on nothing
today and flush the player's buffer for a role the server never activated.
`stream/end` already reads this way, since it reports whether a stream was open.

A `stream/start` is refused when this client holds no player role, for the same
reason giving up the roles closes the stream. The run loop keeps reading after a
`server/activate` that leaves it holding nothing, so a `stream/start` arriving
afterwards would otherwise re-open a stream for a role the server never granted.

Giving up the roles closes the stream with them. A `server/activate` whose
`active_roles` is an explicit empty list leaves this client holding nothing, and a
stream that was open would otherwise still read as open for the rest of the
connection -- audio scheduled for a role it no longer has. The empty list has to
be explicit: an *omitted* one means the roles persist, which is the asymmetry the
admission section describes, and it is easy to reproduce by accident, since a Go
pointer to a nil slice marshals as `null` and reads back as omitted rather than as
empty.

## The audio itself

Audio arrives as binary messages rather than JSON. The player's range is the four
type bytes 4 to 7, and only **4** carries audio: `aiosendspin` defines
`AUDIO_CHUNK = 4` and nothing for 5, 6 or 7, so a client meeting one of those has
met a server from the future rather than a chunk it should try to play. The
reference client refuses them the same way, by failing to construct its enum.

The header is nine bytes, `>Bq`: the type byte, then the timestamp in
**microseconds as a big-endian signed 64-bit integer**. This transport already
peels the type byte off as the message kind, so what reaches the parser is the
eight-byte stamp followed by the audio. The endianness is the trap worth naming:
the header is big-endian and the PCM after it is little-endian, so a parser that
picks one for the whole message reads plausible rubbish rather than failing.

A refused chunk is **dropped rather than fatal**, and what it says carries none of
the server's own numbers. The run loop reports most things once per connection by
remembering what it has said, and what it remembers is the line *with its
arguments filled in* -- so a stamp in the text makes every bad chunk a new thing
to remember. That list is sixteen long, which a server stamping 25 ms chunks
wrongly fills in under half a second, and after that nothing else on the
connection is ever logged: not the roles, not the clock, not an unknown message.
A refusal therefore names the rule it broke and not the number that broke it.

The session survives it and says so once, because a server that stamps every chunk the same wrong way would
otherwise be disconnected on the first chunk of every stream for ever, and Music
Assistant stops retrying after about eight and a half minutes. That also matches
how the same `onAClock` check is treated on a `server/time` reply, where an
off-clock stamp refuses the measurement rather than the connection.

Two refusals matter more than they look. A body shorter than eight bytes has no
timestamp to read, and reading one anyway slices past the end -- which is a panic,
and a panic here is the supervisor's five-second restart loop with the button
ungrabbed each time. Audio whose length is not a whole number of frames is worse
than a truncated chunk: at 16 bits a stray byte pairs every later byte with the
wrong neighbour, so the stream stays desynchronised for as long as it runs rather
than glitching once.

A chunk arriving with no stream open is dropped rather than refused, because the
format that would decode it is whatever the last `stream/start` named. The
reference does the same, gating each binary type on whether its role's stream is
active.

**`send_ahead` is the server's number, not a field.** Nothing on the wire carries
it. The server computes it from what this client declared -- `min_buffer_ms`,
`static_delay_ms` and `required_lead_time_ms` -- and sends each chunk that far
before it is due. So the timestamps are in the future by an amount this end chose,
and the client's own declaration is what it will have to live with. Measured on
hardware, the first chunk follows `stream/start` by about a millisecond, so a
player is never given a quiet moment between being told the format and being
handed audio in it.

## What the log says while a stream runs

The run loop reports most things once per connection, because a peer that repeats
itself should not be able to fill `/data`. Stream transitions are the exception:
`stream/start`, `stream/end` and `stream/clear` log every time. Measured on
hardware, the dedupe made this log useless for exactly the question it was being
read for -- several tracks played, and only the first one said anything, so
nothing distinguished "the second track worked" from "the second track never
arrived".

Those lines spend a **budget of their own**. `internal/untrustedlog` allows twenty
peer lines a minute and five thousand for the run, and the run ceiling never
refills: once it is gone, nothing a peer does is logged again until the daemon
restarts. Playing music is a peer talking constantly and legitimately, so a
per-connection dedupe was the only thing keeping ordinary listening from retiring
the log -- two summaries a minute alone reach five thousand in about forty hours
of cumulative playback, and a server flapping its stream reaches it in minutes.

So playback gets a second `untrustedlog.Log` rather than a share of the first. The
counters live per `Log`, so the stream lines, the first-chunk lines and the
summaries can spend themselves out without taking the handshake, the roles, the
clock or an unknown message type with them. What reaches the disk is the sum of
two bounded budgets rather than one unbounded one, and each says so when it starts
dropping lines.

Audio chunks are not logged one by one: Music Assistant sends 1,200 frames at a
time, which is 25 ms, so that is forty lines a second. The first chunk of a stream
is logged with how far ahead of now it is due, and a summary follows every thirty
seconds, when the stream ends, and when the connection does: how many chunks,
frames and bytes arrived, and the range of lead times.

A summary is written when a message arrives and the interval has passed, rather
than when a chunk does. A stream that stops sending audio while the server keeps
talking is the starvation this number exists to make visible, and a summary that
only ever followed a chunk would go quiet exactly then. A server that stops
talking altogether is caught by the idle timeout instead, and the connection's
last act is to report what it had heard.

The interval is a **duration rather than a count**, because a count is a rate the
server picks. At one summary per five hundred chunks, a server sending 10 ms
chunks writes twelve lines a minute and one sending 1 ms chunks writes a hundred
and twenty, which is over the twenty-a-minute limit and would sit at the limiter
for as long as music played -- suppressing every other peer line, and spending the
five thousand for the run in under an hour. Thirty seconds is two a minute
whatever the server chooses.

The lead is the number worth watching, because it is the server's `send_ahead` as
this client actually sees it, and a stream whose lead shrinks toward zero is one
that will starve.

**Each summary also says what the player did with the window: how much audio it
placed, and how much silence it played instead.** Those two come off the stream
itself and are reported as the window's own totals rather than the stream's, for
the same reason every other number on the line is: a cumulative figure beside a
windowed one reads as a player that is permanently short.

It answers a question the rest of the line cannot, and it answered it on the first
run. A stream that closed having placed 103.875 s of audio against 3.3 s of
silence had lost nothing -- audio received came to exactly audio placed plus audio
dropped, to the microsecond -- so the silence was time when nothing was due rather
than audio that went missing. What the close report could not say is *where* it
fell, and a gap at a track boundary and a player starving steadily look identical
in one total.

The window's own totals are counted from the stream they belong to, which is
fiddlier than it sounds: a stream that is replaced starts its counters again at
zero, so a baseline carried over from the stream before it undercounts the first
window by that whole stream. Subtracting and watching for a negative answer is not
enough, because the difference only goes negative while the new stream is still
behind the old one's total -- a short stream followed by a long one reports a
plausible, wrong number for ever. So the baseline is keyed on the stream itself and
reset when that changes.

Per window they do not. Measured across three Dots and about twenty windows of
continuous playback, the silence in a window is **1.0 to 1.9 ms** -- a frame or
two, which is the rounding where one chunk's frame index meets the next. So the
3.3 s was not a trickle: it fell in a few large pieces, which is what the gap
between two tracks looks like, and nothing about the player was short of audio.

**Music Assistant keeps one stream open across a track change**, which is what
makes that distinction worth having. Measured on a Dot: a track changed mid-stream
with no `stream/end`, no `stream/start` and no gap in the thirty-second summaries
-- the next track is simply more audio with later timestamps, and the mapping
carries across it for nothing, because the timeline is absolute. So the silence
between two tracks arrives as a hole in that timeline rather than as a stream
ending, and it is counted as silence because that is what is due.

**A lead is two numbers pretending to be one.** It is
`ClientTime(stamp) - now`, so it moves when the server sends late *and* when this
end's estimate of the server's clock moves, and the log cannot tell those apart
from the lead alone. Measured on a Dot: one stream opened at 182 ms against a
declared floor of 500, and one thirty-second window reported a chunk due 835 ms in
the past. Neither could be attributed, because convergence was logged once per
connection and nothing said whether the filter had moved underneath.

**Measured, and the answer was the server.** A stream opened at 218 ms against the
declared floor of 500, and the window that carried it reported a clock good to
313 us whose offset moved 1.09 ms over the thirty seconds. A 1 ms clock cannot
account for a 282 ms shortfall, so Music Assistant really does open a stream with
far less lead than `max(min_buffer, required_lead) + static` predicts, and fills
toward the ceiling afterwards -- 1.97 seconds by the end of that same window. Three
first streams have now opened at 182, 218 and 490 ms.

That was a design constraint on the buffer rather than a curiosity, and the buffer
answers it: it **does not wait for a quantity of audio at all**. A stream is placed
against the player's own reading and then plays whatever is due, silence where
nothing is, so it opens on what it is given and grows without ever holding audio
back for a floor that may not arrive. What it does need is *lead* rather than
depth, which is what `required_lead_time_ms` now declares, and a chunk that is
already past when the mapping settles is dropped rather than played late.

So each summary carries the clock that timed it: the spread the filter reports,
and how far its offset moved since the previous summary. A lead that fell while
the offset held still is the server; a lead that fell as far as the offset moved
is this end. Both numbers come off the filter the summary already consults, so the
line costs nothing extra. The baseline carries across windows rather than
restarting, since a window that measured from zero would report the whole offset
as movement every time. The spread does not carry: it is rewritten by the same
chunk that makes it printable, so carrying it would only suggest a stale one could
reach the log.

The lead bounds do not carry either, and for the opposite reason to the baseline.
A window that inherited them would never seed its own, and whichever end the real
leads did not reach would be reported as the zero value -- which reads as a chunk
due exactly now, or as a late arrival that never happened. One flag cannot do both
jobs, so there are two.

The filter keeps `ServerTime`, `ClientTime` and `Converged` although the daemon
now reaches it through `sample` alone. `ServerTime` and `ClientTime` are an
inverse pair, and the test asserting they undo each other is what checks the drift
arithmetic that nothing else reaches; `Converged` is the readable form of `state`
that ten tests are written against. That is surface kept for the tests that check
the contract, rather than surface waiting for a caller.

Measured on hardware across five streams, the first chunk is due 481 to 493 ms
ahead and the lead grows to about 2.4 seconds. Both numbers are this client's own:
the floor is the `min_buffer_ms` of 500 that `serve.go` declares, and the ceiling
is that plus the two seconds of `buffer_capacity`. The server fills what it was
told and stops, so a player that wants a different lead asks for it by declaring
one rather than by asking the server for it.

A `stream/start` carrying nothing for a player flushes nothing. The same message
opens artwork and visualizer streams, and one of those arriving while music plays
is not this player's stream ending -- flushing there would split a summary in two
and make the next chunk of a stream that never stopped read as a new one starting.

A `stream/clear` flushes the summary but keeps the stream, because the stream is
still the one that was announced -- a clear discards what was buffered and the
audio carries on. Giving up the player role mid-stream flushes it too, and there
the flush has to happen at the moment the role goes: the counts would otherwise be
reported against whatever stream started next, which reads as a summary for audio
that had not arrived yet.

Lead times are printed as durations rather than whole milliseconds. Integer
division toward zero renders anything from -999 us to 0 as `0 ms`, so a stream
already arriving late would read exactly like one running tight -- and late is the
failure this number exists to catch.

A chunk's timestamp is bounded the way a `server/time` stamp is, by `onAClock`.
The stamps are microseconds on the server's own monotonic clock rather than since
the epoch -- an epoch stamp is already past the ceiling, which is what makes the
bound worth having: scheduling against one would put every frame decades away.

That the stamps really are monotonic is measured rather than assumed, and the
measurement matters because the failure would be total and quiet: every chunk
refused, one line to say so, and a Dot that looks like it was sent no audio at
all. Music Assistant was played to a Dot running this bound, and its chunks were
accepted across four streams with leads of 182 to 490 ms. A wall-clock stamp would
have refused every one of them.

What is not implemented is `stream/request-format`, the message a client sends to
ask for something it can play. With one declared format and a server that honours
it, the path is unreachable; a refused stream logs and stops there.

## The numbers may move while a session runs

Sendspin expects a player to learn, and says so for each field. A client MAY
update `required_lead_time_ms` and `min_buffer_ms` **at any time**, and a server
MUST factor the new values into subsequent playback, with the caveat that a client
SHOULD debounce -- change them when conditions have shifted, not on a transient.
`output_delay_ms` may be updated too, though for a narrower reason: when the audio
output itself changes, such as a speaker being plugged in, with a delay persisted
per output and across reboots.

The spec also says how to derive one of them rather than guess it: `min_buffer_ms`
comes from the distribution of chunk arrival delay, where a chunk's delay is
`arrival - compute_client_time(timestamp - send_ahead)`, sized from the upper tail
over a window long enough to catch intermittent interference, and with samples
taken before the time filter converged thrown away. `required_lead_time_ms` is
explicitly *not* derivable from that distribution, because it is measured from a
start trigger and the chunks after a `stream/start` arrive in a burst that says
nothing about steady state.

So the 500 ms this client declares is a placeholder in the same sense the zeros
are: the machinery to measure it does not exist yet, and when it does, the wire
already supports correcting it without reconnecting.

**No pairing method is advertised**, and that is a deliberate deviation: the spec
says every client offers at least the Pairing PSK method, and this one does not,
because the flow is not implemented. Claiming a method the client cannot honour
is worse than claiming none, and the admission rules refuse every `pairing`
activity as `method_not_supported` on the same basis. `offeredPairMethods` is the
single source for both what `client/hello` advertises and what those rules
accept, so the two cannot drift apart. See the skew table below for the second
reason the shape is not settled yet.

Only roles named in `supported_roles` can become active. A server activating
something else -- `display@v1`, say -- has its unknown roles dropped rather than
obeyed, so the Dot never holds a connection slot or reports state for a role it
does not implement.

A test pins the declared format against `audio.ChimeRate` and
`audio.ChimeChannels` themselves rather than against a copy of their values,
because the two share one OpenSL player and a mismatch would not fail at the
format: it would fail as silence, or as a chime at the wrong pitch. Comparing
against literals is what makes that claim look kept while the chime moves.

## Being found

The Dot advertises `_sendspin._tcp.local.` on **8928** with a `path` TXT record,
which is required, and a `name` one, which is not but should match the `name` in
`client/hello`. `tcp/8928` needs a firewall rule exactly as `tcp/6053` does, and
for the same reason: FireOS runs `INPUT policy DROP` with a port allowlist, so
without the rule Music Assistant times out with nothing in the daemon log. The
`udp/5353` the responder answers on needs nothing, because the stock chain
already accepts it -- Alexa runs her own mDNS. `docs/pitfalls.md` carries the
`tcp/6053` case and the `udp/5353` one.

The rule is opened **after** the socket binds, not before. A rule added for a port
nothing listens on is re-asserted every thirty seconds for the life of the daemon
and deleted by nothing, which `docs/pitfalls.md` records as its own hazard, so the
order is what keeps a failed bind from leaving one behind.

The responder itself is `internal/mdns`, and it answers for a list of services:
`sendspin.Advert` hands it one the way `esphome.Advert` hands it the other, so
`internal/sendspin` never imports `internal/esphome` and the two stay
independent. docs/mdns.md carries why there is one responder rather than two;
the rule that a service with no TXT records is refused rather than advertised is
in `internal/mdns`'s own tests.

One host record covers both. The two services claim the same `<name>.local.` and
the same A record, which is not a conflict: mDNS conflict detection is about
different data under one name, and this is the same data, one device advertising
two services.

The advert goes up only once the listener is behind it. `enable` opens the socket
before it advertises, and a Dot that could not read its key or could not bind the
port advertises no Sendspin service at all rather than advertising one that
refuses every connection.

## Turning it off entirely

`switch.<name>_sendspin` removes the surface rather than declining to use it. All
of what the section above describes goes: the mDNS advert is withdrawn with a
goodbye, the listener closes, the `tcp/8928` rule is deleted and stops being
re-asserted, and the sessions already running end. What is left is a Dot that
does not answer on the port and is not discoverable on it, which is the only kind
of "off" an operator can check from somewhere else on the network.

The order is not a reversal, and the rule is why. Going down: the advert first,
so nothing new is told where to connect while the rest goes, then the listener,
then the sessions, then the rule. Coming up: bind, open the rule, serve,
advertise. The rule is last down and second up, because it must never be the
thing that outlives the socket -- a rule for a port nothing listens on is
re-asserted for the life of the daemon and deleted by nothing, which the section
above gives as its own hazard. Everything else is torn down in the order that
stops a new server arriving mid-teardown.

A kill cannot say goodbye either, and the rule outlives the socket in exactly that
case. `SIGTERM` withdraws the advert and exits without deleting it, because
deleting it is an `iptables` call that waits on netd's lock while the daemon is
trying to stop, and the supervisor has it back within five seconds. What closes
that window is the next start rather than the last stop: it either brings the
surface up, which needs the rule, or sweeps it. `uninstall.sh` covers the case
where there is no next start.

A reboot cannot say goodbye, and nothing at startup should try. Switching off
through the switch withdraws the advert, and so does a clean stop -- `SIGTERM`
reaches `withdraw()`, which retires every service at TTL 0, measured flushing the
record out of a real querier's cache. A reboot or a `SIGKILL` gets no such chance,
so a server that browsed earlier can hold a record pointing at a closed port for
as long as `ttlShared` has left. That is mDNS rather than a defect: retracting a
name the daemon has not claimed this boot would be a goodbye for something that
was never announced, it could not go out until the responder had an address --
which is later than it sounds, and measured below -- and by then the stale
record has already produced the right answer, which is a server finding the
player unreachable.

Deleting the rule also has to wait for the goroutine that re-asserts it. Closing
that goroutine's channel only asks it to stop; a tick already delivered would
re-append the rule after the delete had run, leaving an ACCEPT for a port nothing
listens on and nothing left to remove it. So `disable` waits for the goroutine to
return before it calls `DenyTCP`, and `HoldTCPOpen` checks the channel again
after the tick rather than only before it.

Switching off and straight back on inside the goodbye budget has one cost worth
knowing. `Client.Close` returns once the goodbyes are said, but each session's run
loop retires its audio stream on the way out, which happens when the socket read
fails rather than when `Close` returns. So a toggle inside that window leaves the
old stream attached to the player, and the new session's first `stream/start` is
told a stream is already open, says so once, and that first track is silent.
Recovery is the next `stream/start`. Two seconds of operator impatience is not
worth machinery to close, and it is the one way `available: true` with no working
stream arises from ordinary use.

Closing the listener is not enough by itself, which is what `Client.Close` is
for. `Serve` returns when its listener closes, and that alone does nothing to a
server already admitted: it keeps its connection, keeps the exclusive slot, and
keeps sending. So `Close` closes every live connection as well as refusing new
ones, and a switched-off Sendspin means the sessions are over rather than only
that no new one can start.

`Set` never blocks. Disabling calls `iptables`, which waits on the xtables lock
netd holds constantly, and the caller is the ESPHome API goroutine answering a
`SwitchCommandRequest`. So the switch hands the wanted state to a worker
goroutine of its own, through a one-deep channel that is drained before it is
refilled, and the API answers at once. Nothing reports the state optimistically:
`On` reads what the worker achieved. The worker wakes the live poll when it is
done, because `readLive` is read on `HeavyEvery` ticks rather than every tick --
two and a half seconds -- and the worker outlives that anyway; without the wake
the switch would sit in its old position for seconds after the operator moved it.
docs/api.md carries the poll.

The property is the other way round: it records what was **asked** for, not what
was reached. A bind that fails writes the request anyway, so a port that was busy
this once is retried at the next boot instead of the failure becoming permanent.
The entity and the property can therefore disagree until a restart, and that is
the intended reading rather than an oversight.

The setting survives a reboot, in `persist.overdub.sendspin`. Android's property
service writes a `persist.`-prefixed property to `/data/property` itself, so
plain `setprop` is enough and no Magisk dependency is added. That is **not**
`resetprop`, which exists for read-only `ro.*` properties: `internal/device` uses
it for `ro.adb.secure` and for nothing else. The prefix is the whole of the
persistence, so a test asserts it -- without it the switch silently comes back on
at the next boot, and nothing else here would say so. Measured on a Dot: off
across a cold reboot came back off, with the value in `/data/property` at mode
`0600`. That is the direction that proves anything, because an unwritten property
also reads as on -- a lost value and a remembered `1` look identical from outside.
docs/device.md carries the readings.

A Dot whose property has never been written defaults to **on**, which keeps the
switch from changing what an existing install does. A `getprop` that **fails** is
the opposite default: the surface stays off and the failure is logged, because a
read that did not happen is not an instruction, and off is the recoverable
direction -- the switch entity is still listed, so an operator can turn it back on,
and the property is not rewritten, so the next restart reads it properly. Those
two are different answers to different questions, which is why `Flag` reports
"unset" and "could not read" separately rather than folding both into a default.
A `setprop` that does not take is reported and the switch then holds only until
the next restart, because `SetFlag` reads the property back rather than trusting
the write.

A daemon that starts with the surface off deletes the `tcp/8928` rule once, rather
than assuming there is nothing to delete. A previous run that had the surface up
and did not get to switch it off -- a `SIGKILL`, the supervisor killing it, a
restart while the operator had it on -- leaves an ACCEPT in a chain this daemon
does not own, and nothing else removes it: the port then stands open for the rest
of the boot with nothing behind it while the switch truthfully reports off. Only
hardware showed that; every test here passes either way, because the chain is not
something the tree can read. So the sweep runs whenever the surface did not come
up, which is broader than "the flag says off" on purpose: a key file that will not
load builds no switch at all, and a bind that fails leaves the surface down with
the flag still saying on, and both of those are runs that can inherit a rule. Its
own failure is a log line rather than an error, because a rule that was not there
is the ordinary case -- `DenyTCP` on an absent rule is one `iptables` call that
reports nothing.

A command that asks for the state the switch already holds costs nothing. The
worker takes it, finds no transition to make, and stops there rather than writing
the property again: `setprop` plus the read-back is two processes and a write to
`/data/property`, and the ESPHome API will accept a `SwitchCommandRequest` as fast
as a peer holding the key can send them. The ESPHome mute select turns away a
command aiming at the state already reached for the same reason, and
docs/device.md gives it for the adb select.

The switch sits behind the ESPHome key and nothing else, so whoever holds that
key can turn the surface back on. It is a control for the operator rather than a
second credential, and SECURITY.md carries what that means for the port.

## The advert changes when the Dot reboots

The TXT records carry `path` and `name`, which the spec defines, and
`overdub_boot`, which it does not: the first four bytes of a SHA-256 over
`/proc/sys/kernel/random/boot_id`, so it holds still for a boot and is different
after the next one. It exists because of what happens when this Dot dies without
warning.

A server that loses the Dot retries and then stops. Measured against Music
Assistant on a Dot, by counting its SYNs against the `tcp/8928` rule while
nothing listened: attempts at 1, 2, 4, 8 and so on to 256 seconds, ten of them,
the last 8 minutes 36 seconds after the daemon was killed, and then nothing ever
again. That matches `aiosendspin`'s own arithmetic -- 511 seconds of sleeping,
after which its backoff reaches a 300-second ceiling and the reconnect loop
breaks rather than caps.

After that only discovery can revive it, and discovery is where the second half
sits. A device killed by a power cut sends no goodbye, so its records stay in
every querier's cache for the PTR TTL, 75 minutes. python-zeroconf, which Music
Assistant browses through, fires a callback only for a record it does not already
hold: identical data is a refresh, and the browser returns without telling anyone.
So the Dot can come back, bind, open its rule and announce itself as often as it
likes, and the server that gave up never hears about it. Measured: the Dot
returned at 16:29 and Music Assistant stayed away for twenty minutes, until a
restart sent a goodbye that cleared the cache.

A record that **differs** from the cached one is news, and the callback fires --
`Updated`, which `aiosendspin` acts on exactly as it acts on `Added`. Measured
three ways: driving a real `zeroconf` browser, identical records produce no
callback at all and a changed TXT produces one `Updated`; a server whose retry is
merely *sleeping* is woken by it, because `connect_to_client` sets the event the
loop waits on, seen at **91** and **84 milliseconds** from listener to handshake;
and a server that has truly given up is revived, because the finished task is
popped and a new one started.

What the token does **not** buy is more than one attempt. A revived task carries
`retry_initial_connection=False`, so it dials once and stops if that dial fails.
Which is why the advert waits until the port is reachable.

The boot id rather than a fresh value per start, because the daemon restarts far
more often than the device reboots. The supervisor respawns it every five
seconds, and a token that moved with it would announce a change on every respawn,
so a crash loop would become a reconnect attempt every five seconds in every
server on the segment. A crash loop is not quiet either way -- the port is
flapping -- but it does not need amplifying. What that gives up is a daemon
absent for more than eight and a half minutes *without* a reboot, and a crash
loop qualifies: `aiosendspin` resets its backoff only for a session that lasted
ten seconds, and a five-second respawn never reaches that.

With no boot id to read there is no record at all rather than a value of this
daemon's own: one that moved per process start would announce a change on every
five-second respawn, which is the storm the boot id is chosen to avoid.

Not from the clock either. This device's RTC is dead after a long unplug -- every
cold boot starts at `2010/01/01 00:00` and is corrected twice as the network
arrives -- so anything derived from the time would repeat across exactly the case
this is for. The `boot_id` is a kernel-generated UUID, and hashing it keeps the
kernel's own identifier off the wire and the record short.

What it costs is 21 bytes on every response carrying the TXT, and an update seen
by every browser on the segment at each reboot rather than only by the servers
that care.

The advert goes up only once the port is reachable, and that ordering is the
other half of the fix. Coming up, the listener binds and the rule is added at
about 25 seconds of uptime, but the responder cannot announce until the interface
has an address, which is a later event than the interface appearing: `wlan0` does
not exist for the first 15 seconds, and the lease lands later still and moves
between boots -- 50 seconds on the boot this fix was measured against, inside 36
on the one docs/pitfalls.md records -- and netd rebuilds the INPUT chain in between, discarding the rule. The
announcement therefore went out advertising a port that answered nothing, the
one dial it bought was dropped, and the SYN never reached userspace to be logged
at either end. Measured: a Dot that
returned from a reboot with a fresh token stayed unreachable to Music Assistant
for the whole run.

So the switch waits for an IPv4 address and asserts the rule again immediately
before it advertises. That is a second assert rather than a replacement: the rule
also goes in when the listener binds, about 25 seconds in, and `HoldTCPOpen`
re-asserts it every 30 seconds after that. Neither is enough on its own, because
the re-assert only fires on a tick -- there is no assert before the first one --
so the announcement can fall up to 30 seconds after netd wiped the rule. The early
assert is harmless and worth keeping: nothing can reach the port before the
address exists, and `AllowTCP` checks before it appends, so it cannot duplicate. A
wipe after the announcement is harmless too, because the connection is already
established and only new SYNs are dropped. Checking the rule instead of asserting
it would not do: the check is stale the moment it returns, and netd is the one
writing. Switching on by hand is unaffected -- the address is already there, so
the wait returns at once.

The wait is bounded at `addressWait`, five minutes. Past it the switch says so and
advertises anyway, which sounds worse than it is: with no address the responder
has never opened its socket, so `Advertise` records the service and sends nothing.
What the fallback buys is that the service is in the list when an address does
arrive, instead of the surface staying unannounced for the rest of the boot.

Nothing is published once the switch has been turned off, and the check for that
is `on` read under the switch's own lock rather than the hold channel. `disable`
clears `on` under that lock before it withdraws the advert, so only two orderings
are possible: either the advert goes up first and the withdrawal takes it away, or
`on` is already false and it never goes up. A hold check alone leaves the gap
between the check and the `Advertise`, which is the window the withdrawal races --
and losing that race leaves `_sendspin._tcp` on the segment, cached for
`ttlShared`, pointing at a listener that is about to close.

A clean stop needs none of this: `SIGTERM` withdraws the advert at TTL 0, the
cache drops it, and the next announcement is an `Added` like any first sighting.
The token is for the kills, the crashes and the power cuts, which are the cases
that cannot say goodbye.

It is also a workaround for one library version rather than a protocol feature.
The spec settled this the other way in Sendspin/spec#207: the guidance telling
servers to give up was removed, because clients are not obliged to re-announce
and a speaker that stays dark until someone power-cycles it is the result.
`aiosendspin` has not implemented that yet -- Sendspin/aiosendspin#348 is open --
and when it does, a server will keep retrying on its own and this key can go.

## One connection at a time, and what that leaves out

The spec's multi-server rules rank connections by their highest declared activity
and arbitrate incoming ones against the holder, with provisional connections, a
30-second window before a connection declares itself, and an allowance for
holding a pairing connection alongside a playback one.

What is implemented is the floor of that: **one admitted connection**. A second
server that reaches activation while one is held is answered `client/goodbye`
with `concurrent_attempt` and closed, which the spec permits --
clients may cap how many connections they hold and reject the rest as lower
priority. The slot is released when the holder goes away, and a test covers a
second server being admitted afterwards.

The ranking, the displacement of a lower-priority holder, and the pairing-beside-
playback allowance are **not** implemented. With one server on a home network
none of it is reachable; with two, the Dot will stay with whichever arrived first
rather than preferring the one that is playing. That is a real difference from the
spec and it is a deliberate floor, not an oversight.

## Keeping the clock

Audio arrives stamped in the server's own monotonic clock, so the Dot has to
hold a mapping between that clock and its own. `client/time` carries one number,
the Dot's clock now; `server/time` answers with three -- that number echoed, when
the server received it, and when it sent the answer. With the fourth taken
locally on arrival, the four give one NTP-style measurement: an offset of
`((T2-T1)+(T3-T4))/2` and an uncertainty of `((T4-T1)-(T3-T2))/2`, which is half
the round trip and the most the offset can be wrong by. Everything is
microseconds, and nothing here is epoch time: both ends are monotonic and only
their difference means anything.

The filter over those measurements is a **port of the reference one**, constants
included -- a two-dimensional Kalman filter over offset and drift, with adaptive
forgetting to recover from a disruption. It is not a choice: the spec makes the
algorithm normative, because the server plans playback assuming the client's
error behaves the way that filter's does. The C++ reference and `aiosendspin`'s
`time_sync.py` agree line for line, and this is the third copy rather than a
fourth design. Converged means what it means there: two measurements, and a
covariance that is no longer infinite.

The Dot's own clock is Go's monotonic reading, taken from a package-level start.
`aiosendspin` prefers `CLOCK_MONOTONIC_RAW` and says why: NTP slewing poisons the
filter, and Go's runtime clock is `CLOCK_MONOTONIC`, which is slewed. What that
costs is bounded -- the kernel caps the correction at about 500 ppm and stops
when it is done -- and it arrives as a rate error, which is the one shape this
filter is built to track and to forget. A step never arrives at all: nothing that
sets the wall clock moves a monotonic reading. So the exposure is a slow rate
error during a correction, unmeasured on a Dot so far, and a raw clock is one
syscall away if audio ever drifts audibly while one is running.

### Asking, and how often

The spec points at the time-filter library's burst baseline: eight exchanges
back to back every ten seconds, keeping the one with the smallest uncertainty.
It points rather than requires -- the words are "known-good baseline" -- which is
why this is not a row in the skew table at the end of this page. `aiosendspin`
does not do that. It sends one exchange at a time on an adaptive
interval -- 200 ms until the filter converges, then 3 s, 1 s, 500 ms or 200 ms
as the reported spread crosses 1, 2 and 5 ms -- and that is what this client
does, for the reason the whole package follows the library: it is what Music
Assistant actually runs against real players. Picking the best of a burst is a
second filter in front of the filter, and the one behind it already weights each
measurement by the uncertainty that would have done the picking.

**One question is outstanding at a time.** The loop asks, waits for the answer or
five seconds, then rests the interval and asks again. The transport is ordered,
so a delayed exchange delays the next one anyway, and asking again before the
answer arrives would only throw away the sample that is about to land.

The answer is signalled over a one-deep channel, and asking **drains it first**.
An answer landing after the five seconds are up -- late, but still matching the
stamp -- leaves a token behind that nothing consumed. Without the drain the next
question reads that token as its own answer, returns from the wait immediately
and asks again, and the reply that was on its way then fails the echo check and
is discarded. The exchange recovers a round trip later, having thrown away a
measurement and having run two questions at once, which is exactly what the
invariant above says it does not do.

### What a server cannot talk this clock into

Every one of these is a peer-supplied number reaching a filter that the audio
path will later trust, so each is refused rather than absorbed, and each has a
test that fails without it.

- **An answer must echo the question.** A `server/time` whose `client_transmitted`
  is not the stamp just sent is not a measurement. Without that check a server
  that never answered anything can hand the Dot whatever offset it likes, at
  whatever rate it likes.
- **The same answer twice is one measurement.** The outstanding question is
  cleared when it is answered, so a repeat lands on nothing. Otherwise one
  exchange reaches the filter's two-measurement bar, and "converged" stops
  meaning two independent round trips.
- **The server's two stamps have to be on a clock.** Both are peer-supplied
  int64s and every term of the two formulas is int64 arithmetic, where the
  reference is Python and its integers do not wrap. `server_received` at
  `MinInt64` with `server_transmitted` at `MaxInt64` makes their difference wrap
  to **-1**, which turns the delay into a confident +1000 us and the offset into
  nonsense -- the exact input the next bullet refuses, absorbed instead as the
  best measurement of the session. Both stamps are therefore required to sit in
  `[0, 2^50]`, which is about 35 years of microseconds, far past any monotonic
  clock that will ever answer and far short of where any of these sums can
  overflow.
- **A reply cannot have been sent before the question arrived.** `server_transmitted`
  below `server_received` is the mirror of the bullet after next, and it slips
  past every check there: the difference the delay subtracts goes *negative*, so
  the delay grows and the sample arrives looking more certain than it is while
  the offset is wrong by however far apart the two stamps were. It is not a
  hypothetical shape, either -- the reference server builds its payload with
  `server_transmitted=0` and rewrites it at send time, so a zero is what a
  missed rewrite would put on the wire. The first two measurements are the ones
  that matter here: the filter takes them unweighted, assigning the offset and
  the drift outright, so two of these set the clock to whatever they say and it
  then reports itself converged.
- **A mapping is not a clock at every rate.** The filter's drift is a rate error,
  and the inverse that turns a server stamp into one of ours divides by
  `1 + drift`. A server can drive that to exactly zero: answer the first
  `client/time` with some offset and each later one with that offset less the
  interval since it, which is a clock running backwards at 1:1. Measured against
  this filter, the drift reaches **-1** on the second measurement, the filter
  reports itself converged to within 150 us, and the division answers infinity --
  `MaxInt64` once rounded, which the audio path then refuses as a moment decades
  away. Every chunk dropped, one line to say so, and a Dot that stays available in
  the group while sounding nothing. So a rate below `slowestRate` is refused, and
  so is a mapped moment past the stamp ceiling, which catches the finite-but-absurd
  case the rate check does not.

  **This is a deviation from the reference, not a port of it.** `aiosendspin`'s
  `compute_client_time` performs the same division with no guard, so the same
  server would raise `ZeroDivisionError` there rather than return a number. Both
  are wrong; refusing the measurement is what a client can do about it, and
  nothing in the spec says the mapping has to answer.
- **An exchange that spent no time on the wire is not a measurement either.** A
  delay of exactly zero is a *zero-uncertainty* sample, and the filter has no
  floor under its own covariance: the measurement variance is zero, which drives
  the state covariance to zero, and with the process variance zero as well the
  Kalman gain is then zero for every later update. Measured: two such samples,
  then twenty honest ones 500 ms out, move the estimate by less than a
  microsecond, and the clock reports itself converged `to within 0 us` for the
  rest of the connection. A server reaches that state by claiming the whole
  round trip as its own processing time -- which it can compute from the
  exchanges before it -- so the sample is refused rather than the filter being
  taught to distrust it afterwards.
- **A negative delay is not a small uncertainty.** A server claiming it spent
  longer on the exchange than the whole round trip took gives
  `((T4-T1)-(T3-T2))/2 < 0`, and the filter squares that into a *positive*
  variance -- a confident measurement built out of nonsense. It is dropped, and
  it is dropped as its own outcome rather than as an answer to nothing: the
  question *was* asked and *was* answered, so the log says which of the two
  happened, and the loop stops waiting and moves on to the next exchange instead
  of spending its whole five-second window on a reply that already arrived.
- **A measurement whose timestamp did not move forward is dropped by the filter
  itself**, as in the reference, because the prediction step divides by the
  interval since the last one.

**A connection that has given every role up keeps asking.** The reference client
has a pause, and it is easy to read as a role thing and copy; it is not. It sits
in the pairing flow, where the wire is reserved for the exchange and every other
send is suppressed, and on the role path the reference *resumes* unconditionally
-- an activation naming no roles leaves its clock running. This client offers no
pairing method at all, so the case the pause exists for cannot arise here. What
is left is a bounded amount of traffic on a connection that holds nothing: the
roleless allowance is 30 seconds in total, so a converged clock asks ten to
thirty times over it and one that never converged asks at most 150, and then the
connection goes. Stopping and restarting the goroutine to save that is a second
lifecycle on the thing whose first one already carried two of the bugs this page
describes.

Each refusal names itself in the log, rather than the four sharing one line. A
Dot whose clock never converges says which of them is happening, and the causes
are not distinguishable from the outside: a server whose stamps sit past the
ceiling and one that claims the whole round trip both look like a player that
never reports itself available.

The audio path reads the converged mapping, one call per chunk: `Session.When`
turns a chunk's server-clock stamp into an instant on this Dot's own monotonic
clock, and the player is handed that rather than the stamp. A chunk arriving
before the filter converges has no moment to be played at and is dropped, said
once per connection -- at forty chunks a second, a line each would spend the whole
run's log budget in about two minutes.

The filter also produces one log line per
connection, the offset's standard deviation at the moment it converges, which is
the number to look at on a Dot before any audio depends on it. Measured against
the real Music Assistant over wifi: the clock converged about 205 ms after the
player role was activated on each of four installs, and the line read
`to within 1784 us`, then `596`, `2437` and `1817` -- which is the spread of two
round trips over wifi, and the reason the number is logged rather than assumed. The session then held for six minutes with nothing else logged: no
unanswered question, no answer refused, and no reconnect.

The rate the exchanges ran at is not readable from the device. The obvious place
to look is the `tcp/8928` ACCEPT rule's packet counter, and it counts **0** over a
minute of a live session -- the stock chain accepts established traffic in a rule
ahead of ours, so only the SYN ever reaches it. That counter answers "did the
handshake get in", which is what `docs/pitfalls.md` uses it for, and it cannot
answer anything about a connection already up.

## Reporting itself unavailable, on purpose

Once a server activates `player@v1`, the Dot sends `client/state` with
**`available: false`**, and a **second** `client/state` with `available: true` at
the moment its clock converges. Both halves are the spec's own precondition: a
player may report itself available once its time filter has converged, and until
then it cannot say when a timestamp falls on its own clock. Measured on hardware
that is about 205 ms after activation, so the false is short-lived and honest
rather than a formality.

A Dot with no player stays false for the life of the connection. That is the case
where `audio.NewChime` failed at startup -- a ROM without OpenSL ES, a player
another process holds -- and the daemon already logs a warning there and keeps the
button working. Reporting available would put the Dot in the group and make the
group wait for a speaker that cannot sound, which is the one outcome worse than
not joining.

**What is never withdrawn is an `available: true` already sent, and that is a
decision rather than an omission.** A Dot that has said it is available and then
cannot open a stream, or whose clock has been driven somewhere its own guards
refuse, stays available for the rest of the connection: it drops every chunk, logs
one line, and holds its slot in the group. Withdrawing it looks like the obvious
fix and is worse, because of what can actually make `OpenStream` fail. Every
reachable failure is transient -- a stream already open, a slot lost to a
concurrent open, or a player already closed because the daemon is going away -- and
each of those clears on the next `stream/start`. So withdrawing availability would
trade one silent track for the rest of the session outside the group, which is the
same trade the idle timeout in docs/audio.md is refused for. A permanently broken
player is a different case and is already covered: it is a `NewChime` that failed,
and that Dot never sends the true at all.

`supported_commands` names `set_static_delay`, and it is the whole of what makes
the delay settable. There are two lists with that name and they hold different
things: `client/hello`'s `player_support` carries `volume` and `mute`, which this
player does not take and which stays empty; `client/state`'s player object carries
a subset of `set_static_delay` alone, which is what 9.1.1's model allows there and
nothing else. Music Assistant reads the second one and shows its delay control
only for a player that names the command:

```python
if player_role is not None and PlayerCommand.SET_STATIC_DELAY in player_role.state_supported_commands:
    entries.append(ConfigEntry(key=CONF_SENDSPIN_STATIC_DELAY, type=INTEGER,
                               range=(0, 5000), immediate_apply=True, advanced=False))
```

So an empty list there was not a neutral default. It was the reason no delay box
appeared for a Dot, and why the only way to nudge one against another player in
the room was to change the build.

## The delay a server sets

`server/command` carries a player object, and the one command this player takes is
`set_static_delay`. The figure is bounded 0 to 5,000 ms by `aiosendspin`'s own
model and refused outside it here, along with a command naming no figure at all --
a missing field would otherwise read as the zero it is not -- and one arriving for
a client that holds no player role. None of those drop the connection: a command
this player will not take is a line in the log and nothing else, because the
alternative is a server losing its speaker over a value it can simply send again.
`volume` and `mute` still answer that they are not handled yet, and the line saying
so now names the command rather than the message, so the two cases read apart.

Applying it is one subtraction at the point a server's timestamp becomes a moment
of ours, and the section above says which way it goes and why.

**The delay is persisted, because the spec says a client MUST keep it across
reboots and server reconnections.** An earlier version of this page argued the
opposite -- that Music Assistant re-sends the figure whenever a player's
configuration loads, so there was nothing to keep. That is true of one server
and is not the rule: a server that does not re-send would leave the Dot playing
at a delay nobody chose. Zero is one of those figures rather than the absence of
one -- a Dot with no external amp is meant to sit at zero -- so nothing here
treats it as "unset": the figure lives in one place, seeded from the property
when the client starts serving, and a server that sets zero gets zero back on
its next connection. An earlier version kept the delay in two places and read
the stored one whenever the live one was zero, which resurrected the old figure
on every reconnect and, since Music Assistant writes back whatever a client
reports, overwrote the operator's zero in the server's own configuration.

**Writing it happens at most once a minute, from a goroutine of its own, and a
drag that lands inside one window reaches flash once.**
 The first version did none of that:
`setprop` costs a measured 40 ms on this device, Music Assistant's control applies
as it is dragged, and the write sat inline on the goroutine that reads audio
chunks. Twenty-three values in one drag is nearly a second of not reading the
socket, and a server sending values in a loop was an unbounded number of writes to
flash with no budget at all -- next to a log path budgeted to twenty lines a
minute. Now a change only wakes a keeper, which waits out the window and then
writes whatever the figure has become, and only if that differs from what the
property already holds. So a drag writes once, after it stops; a control nudged
and put back inside one window writes nothing; and a server sending values forever
costs one write a minute whatever it does. The window is what those promises are
measured against rather than the gesture: a drag somebody holds for longer than a
minute writes once while it is moving and once when it stops, and a nudge whose
window closes on it writes twice. One write a minute is the ceiling, not one write
a drag.

What that trades away is the last change before the daemon stops, so every stop
that can see it writes first. There are five: the Sendspin switch turned off, a
signal, a key found stuck, the ESPHome listener returning, and `serve` itself
returning an error to `main`. Each closes the Sendspin listener and waits for
that client to stop serving, which is when its keeper has written what it held.
`Serve`'s defer does the writing; the callers wait on it.

The waiting is the part worth stating, because two things read the property back:
a switch-on rebuilds the client from it, and `os.Exit` runs no deferred function.
`SIGTERM` is what `install.sh` stops the old daemon with, which made a reinstall
the ordinary way to lose a figure.

Each wait is bounded at `sendspinFlush`, two seconds, because a write that will
not finish must not hold the daemon open -- the supervisor cannot see a wedge,
and one `setprop` is a measured 40 ms. A write still running after that is
abandoned, so the figure it finally lands on the property is the one from before
the switch went off. On the property only: what the switch holds, and so what the
control shows and the next client starts at, is not moved by a write the switch
did not make. The trade is deliberate, and the case needs a write fifty times
slower than any observed.

The keeper's wake is cleared before the keeper is told to stop, rather than after,
and that ordering is the whole of whether this works. A figure set while `Serve`
is returning would otherwise find the channel still there, post to it, and be read
by nobody -- and that window is one property write wide, since the flush is
writing while it happens. Cleared first, a set arriving that late finds no keeper
and writes the figure itself, and one arriving earlier stored its figure before
the keeper's last read of it.

What is left to lose is the stop nothing can be done about: a panic, a
`SIGKILL`, a pulled plug. A signal landing while the switch is already part-way
through turning off is the one narrow case still in that list rather than the
one above it, because the listener is already gone and there is nothing left to
wait on. That is the right side to lose on: the figure matters only across a
reboot or a reconnect, an operator who has just moved a control can move it
again, and Music Assistant re-sends its own value whenever a player's
configuration loads. Flash on a 2016 Echo Dot is the part that does not come
back.

Waiting became worth doing when the figure stopped being a music server's alone.
A delay only a server could set is one a server re-sends; a delay somebody typed
in Home Assistant a moment ago, and is standing in front of, is not.

A refused write is tried again on the next window -- three times in all, counted
per figure rather than per run -- and then given up on until the figure changes.
Both halves are load-bearing. `SetNumber` reads the property straight back, and a
`setprop` followed immediately by a `getprop` on this device can report empty for
a name that was in fact accepted, so the likely failure is a spurious one and a
single attempt would spend the figure on it. Retrying forever is the other error:
a name flash genuinely will not take would cost a `setprop` a minute for the rest
of the boot, which is the unbounded write this window exists to prevent.

The keeper writes once at startup, before waiting for anything, when the stored
figure could not be **read** -- the one case where it cannot tell whether the disk
agrees with it. It does not wait out a window first, because a window spent on
this dot's own correction is one in which a stop loses it. A failed read leaves
the client reporting zero while flash may hold something older, and a server that
then sets zero is answered from the live figure and never reaches the keeper, so
nothing would correct the disk and the next boot would play the old delay. An
absent property is not that case: absent already reads as zero.

A figure the direct write **refused** is primed rather than written: the keeper is
woken as it starts, so it writes on its first window rather than waiting for a
change that may never come. A window's delay is the point here -- the figure is
already held and reported, and only flash is behind. The unreadable case above is
the one that cannot wait, because there the client does not know what flash
holds.

The figure is kept in `persist.overdub.sendspin_delay`, beside the switch flag and
for the same reason, and `uninstall.sh` clears both. The name is
short because it has to be: 31 characters is all this device takes, and the first
one written here was 33 and refused on hardware after passing every test.
docs/device.md carries the sweep. Within a run
it outlives a connection as well, so a reconnect does not start from zero. The
value that goes back out in `client/state` is whatever is currently applied.

**A change costs the log nothing, which took a second attempt.** The obvious line
-- one per change, saying what the delay is now -- is a peer-driven write to
`/data`, and Music Assistant's control is `immediate_apply`, so it sends a value
for every step of a drag. Measured on a Dot the first time somebody used it: 23
lines, values walking from 1.268 s down to 1.097 s, which spent the peer log's
twenty a minute and took **13 other lines** with it, a playback summary among
them. A slider is the most ordinary thing in the world to drag, so this is the
common case rather than a hostile one. What is logged now is one line per
connection saying a server is setting the delay, and the current figure rides on
the thirty-second summary, which is already bounded at two lines a minute and
always says what the audio was actually placed against.

**A change applies to audio written after it, and the queue keeps what it already
holds.** The server has already sent audio under the old figure, so raising the
delay leaves some of that audio due in the past -- the spec spells this out for
*lowering* a delay, where the excess is extra audio buffered, and says nothing
about the other direction, where it is audio that can no longer be placed. The
reference does the same: the delay is applied where a server's timestamp becomes a
moment, not to audio already scheduled. Re-placing a queue of audio under the
listener at every step of a drag is worse than the gap.

The 4.9 seconds of silence measured in one window during a drag is **not** a clean
measurement of that gap, and the attribution here used to say it was. The same
drag also wrote the property once per value, on the goroutine that reads chunks
off the socket, and that write is a measured 40 ms of `setprop` on this device --
50 pairs took 2 seconds, while the reads alone are too fast to time. Twenty-three
values is nearly a second of a read loop that is also feeding the speaker. Both
causes were present in that window and nothing separates them. The write is off
the loop now, so a repeat of the measurement would mean something; until then the
honest statement is that a delay change costs *some* discontinuity, bounded by the
size of the change.

**A delay this player takes is reported straight back, and that is what makes it
work at all.** The server schedules at least `min_buffer_ms + output_delay_ms`
ahead -- `max(min_buffer_ms, required_lead_time_ms) + output_delay_ms` for a
buffered stream, which is the same figure here only because this dot's 500 ms
`min_buffer_ms` is above its 350 ms lead -- and
it counts a chunk outstanding until `timestamp + duration - output_delay_ms`. Both
of those read *the delay the client last reported in `client/state`* -- not the
one the server just commanded. messaging.md says a client sends `client/state`
"whenever any state changes thereafter", so reporting it is the client's job and
skipping it is a conformance bug with an audible cost.

Measured here, before that was understood. A delay of 1,814 ms was applied and
not reported, because an earlier version of this code only answered back when it
had to clamp a figure. Music Assistant went on sending as though the delay were
zero -- leads of 1.767 to 1.973 s, the same as always -- and every chunk then
landed behind where it had to be placed:

| delay applied | reported back | leads that arrived | audio placed | silence |
|---|---|---|---|---|
| 5 ms | yes | 1.716 - 1.973 s | 27.83 s | 2.19 s |
| 1.814 s | **no** | 1.767 - 1.973 s | **0.50 s** | 29.53 s |
| 2.716 s | no | 0.493 - 2.371 s | 6.95 s | 23.18 s |
| 1.5 s | **yes** | 1.990 - 3.473 s | 29.64 s | 405 ms |

The last row is the mechanism proving itself. The moment the figure went back, the
server's floor moved to exactly **1.990 s** -- 500 ms of `min_buffer_ms` plus the
1,500 ms it had adopted -- and the ceiling rose to 3.473 s. Audio that had been
unplayable played clean.

**Which also means the buffer is not what pays for a delay.** The arithmetic runs
the other way from the way it reads: a larger delay moves a chunk's completion
time *earlier*, so it leaves the outstanding count sooner, and the lead a server
can reach is `buffer_capacity + output_delay_ms` rather than `buffer_capacity`
alone. The 3.473 s ceiling above is the two seconds this client declares plus the
1.5 it had adopted. What the jitter buffer holds is the other side of the same
subtraction -- a lead less the delay -- so it never needs more than the capacity
either. `bufferSeconds` stays at 2 and `streamHold` at four seconds; raising both
to carry a 5-second delay was a wrong turn taken on the way to this paragraph, and
the measurement that looked like a capacity ceiling was a missing `client/state`.

A figure outside 0 to 5,000 is still held at the end it passed, because the spec
says clients MUST clamp to that range, and the log says so when it happens. What
is never answered is a delay set again to what it already is: a server
re-asserting its own figure gets silence rather than a reply, which is what keeps
an `immediate_apply` control from being answered at every step of a drag.


## The same delay, set from Home Assistant

The delay is one figure with two writers. `number.<name>_sendspin_output_delay`
sets it over the ESPHome API and `set_static_delay` sets it over this connection,
and docs/api.md carries the entity.

**Last writer wins, and neither end is deferred to.** The figure describes this
Dot -- the latency past its audio port, and in a group the alignment somebody
tunes by ear -- so the operator moving it locally is as authoritative as the
server that sent the last value. What would be worse is either alternative: a
local set a server silently overrides is a control that springs back, and a
server's set refused because somebody once touched the local one is a player that
drifts out of its group with nothing to say why. So a set from either end applies
at once and is reported in `client/state` at once -- and the entity reports what
is applied rather than what it last asked for, so a server moving the delay shows
up in Home Assistant within a tick of the live poll rather than leaving the two
disagreeing.

Keeping it is the one thing that is not immediate, and with a client up both
writers share the keeper's window: a figure typed in Home Assistant reaches flash
on the same terms as one a server sends. Two writers is exactly why the property
has one -- the alternative is two policies racing for one name, and the figure
that lands is whichever fork returned last.

Reporting it back matters as much here as it does for a server's own command, and
for the same measured reason: the server schedules `min_buffer_ms +
output_delay_ms` ahead and counts a chunk outstanding against *the delay the
client last reported*. A figure set locally and not reported would leave Music
Assistant sending for the old one, which is the 0.50 s of audio against 29.53 s
of silence in the table above.

**One value, read where it is used rather than copied.** The figure lives in an
atomic on the `Client`, and the connection no longer holds a copy of it: the
playback path asks for the current figure as it places each chunk. That is what
makes a local set apply mid-track instead of at the next reconnect, and it is the
same subtraction either way -- so what the thirty-second summary says is "this
player's output delay" rather than the one a server set, because by then it may
not be.

The figure kept from the last run is read into that atomic the first time
anything needs it, rather than being treated as a fallback for a zero. A zero
that means "unset" cannot be told from a zero somebody chose, and it made the
control un-turn-off-able: a Dot that kept 700 ms and was set to 0 before any
server connected went back to 700.

**The same figure set again writes nothing and says nothing**, whichever end
sends it. That was already the rule for a server re-asserting its own value; it
now also means a server that sets what Home Assistant already set gets silence
rather than a reply. It has the figure either way -- the `client/state` sent when
it activates the player role carries whatever is currently applied.

**A set from here writes one line, and it is not a server's to spend.** A server
gets one line per connection saying it is setting the delay, whatever it sends
after. The API side notes each figure it commits instead, through the same
rate-limited log, because what arrives there is one command per operator action
rather than one per step of a drag. A refused write is the keeper's problem: the
figure stays applied and reported, and the retry and the giving-up are the ones
above. That does spend a server's budget -- three lines for a figure only an
operator asked for -- but it is bounded and it is the same log the other writer
uses, which is the reason not to open a second.

**Reporting runs off the caller's goroutine, and the wake belongs to the session
that holds.** The report is a socket write whose deadline is 150 seconds once the
connection is up, and it can queue behind another writer; the caller is the
switch's one worker, so a server that stopped reading would otherwise take the
switch with it -- no toggle, no further sets, no owed write. A per-session
reporter takes a wake instead and reads the figure fresh, so the report carries
the latest value and rapid sets coalesce into one.

Per session rather than per client, because a server can drop the player role and
take it back on one connection. A wake posted while nothing holds has nowhere to
go, so a figure moved in that window is reported when the role returns -- and
only then, since a repeated activation that never lost the role costs neither a
state nor a keepalive.

A write this player cannot make ends the connection, and that is not a choice
about tidiness. `seal` advances the Noise nonce before the bytes reach the
socket, so a write that fails leaves the sender at *n+1* and the peer expecting
*n*: every later frame on that session fails authentication at the far end, with
`chacha20poly1305: message authentication failed` the same as a wrong key. A
partial write is worse again, since the peer is then mid-frame -- the write-side
form of the boundary problem docs/api.md describes for reads. So there is no
losing one message and carrying on: either the write landed, or the session is
finished and only a reconnect gets it back.

The claim is about what is **reachable** rather than about TCP. Two errors sit
above `seal` -- a payload that will not marshal, and `WriteTyped` refusing the
fragment type -- and neither advances the nonce or says anything about the
socket. Neither is reachable from these writers: the payloads are plain integers
and strings, and nothing here sends a fragment. Everything below that line is
either the cipher or the socket, and both finish the session. The read side is
the opposite case and always has been, where an over-long message, a bad opcode
or a failed decrypt is a peer breaking the protocol on a socket that is perfectly
healthy.

Three writers run off the read loop and each closes the connection now rather
than only ending itself: the delay report, the time sync, and the keepalive. The
time sync is the one that hid best. `send` and `recv` are separate cipher states,
so a session that can no longer send goes on receiving and playing audio
correctly while its clock stops being corrected -- the figure drifts, the audio
is placed against it, and the read deadline never expires because the server's
own frames keep refreshing it. The keepalive is the plainest: it takes no payload
and skips `seal`, so its only errors are the socket, and it is also the mechanism
that would otherwise have noticed the peer was gone.

**The state a local set writes has to carry the availability the connection
already reported.** `available` is the connection's own progress -- false until
the clock has converged -- and it rides on every `client/state`, so a state
written elsewhere that guessed at it would tell the server this player had gone
away and take the Dot out of its group for a figure somebody typed. The session
records what it last reported, and a local set repeats it rather than deciding
it.

**With Sendspin switched off there is no client, and the control still works.**
The switch holds the figure and writes it itself, since there is no keeper to
hand it to, and the next client that comes up is built from it -- which is also how a
figure survives the switch being toggled, since each switch-on builds a fresh
`Client`. The switch holds the figure to the same 0 to 5,000 before keeping it,
because what reads that property back is a client that would clamp it anyway.

**It writes under the same window, and that was missed the first time.** Writing
each figure as it arrived left the switched-off path with no limit at all. The
keeper exists because a figure can be set faster than flash should take it, and
a box committing one figure per operator action is a statement about a control
rather than a bound on what may arrive: an automation recomputing the delay puts
every value on flash at a `setprop` apiece, where the identical traffic with
Sendspin on costs one write a minute. So the switch takes `KeepApart` and
`KeepTries` from the keeper rather than copying the minute, and the property has
one policy whichever writer reaches it.

What the switch holds and what the property holds are then two figures. The
control shows what is held and shows it at once -- the wake goes out when the
figure is taken rather than when it is written, or a figure inside the window
would leave the control a minute behind. What is owed is written on three stops:
the window closing, the switch being turned on, and the daemon stopping. The
switch-on is the one easy to miss, because `enable` builds its client by reading
the property, so an owed write has to go in front of it or the client starts at
the figure before last.

Deciding a write is owed, making it, and recording it are one operation, under a
mutex of their own. Two callers that each decide before either records both spend
a flash write and both count an attempt against a bound neither can see -- and
they do arrive together, the window closing on the worker and a signal reaching
`flush` from its own goroutine. The mutex is not the field lock because a
`setprop` is 40 ms and the entity's poll reads the figure through that lock.

What a write records is what the **property** holds. It records what the switch
holds only when a client made it, since a client's figure is where the switch
learns what to report once that client is gone. Recording it unconditionally let
a write in flight put its own figure back over one set while it ran -- on the
control, in flash, and into the next client, with nothing owed and nothing
logged. The same line made an abandoned write after a switch-off revert the
control rather than only the property.

Switching off then has to take the figure its client was holding, because that
client's own keeper writes it on the way out and the switch would otherwise go on
reporting an older one. Nothing would correct it either: with the property
already holding the client's figure, nothing is owed, and an operator who typed
the older figure back would be refused as a repeat.

A switch-on's read can fail, and a read that failed is not a figure. `keptDelay`
answers `(0, false)` when `getprop` could not be run at all, which is not the
answer for a property that is absent: absent is a known zero. Taking the zero
would hand the new client 0 and a disk it believes unreadable, and that client's
immediate correcting write would destroy whatever flash held. So a failed read
falls back to the figure the switch already holds, and only the *disk* is marked
unknown.

That covers a later switch-on and not the first one. At boot there is nothing to
fall back to -- the failed read is the only read there has been -- so a Dot whose
`getprop` fails at startup does write 0 over whatever flash held. The fallback is
worth having for the case where something better exists, and the boot case is the
one where nothing does.

An unreadable disk is a reason to write once, not an exemption from the bound.
The write is attempted on the same terms as any other -- `KeepTries` and then
given up on -- because a `setprop` that will not take would otherwise cost a
write and a log line every window for the rest of the boot, which is the
unbounded write this whole section exists to prevent.

Measured on a Dot with a track playing. Every claim above held.

A figure set in Home Assistant applied mid-track, inside the five seconds to the
next summary, and the summary named it. Music Assistant's send-ahead floor
followed the figure this player reported -- a stream's first chunk arrived `due
in 1.475334s` against a 985 ms delay, which is 500 ms of `min_buffer_ms` plus the
figure -- and its own control read what Home Assistant had set. What it does not
do is push that to a browser, so its page shows the old figure until reloaded,
which looks exactly like a report that never arrived and is not one.

The keeper held: the control's arrows send one command per click about 200 ms
apart, so eleven commands arrived in two bursts inside a minute and flash took
one figure. A restart read it back and logged it before any server connected, and
Music Assistant reconnected without overriding it.

Two more from the same run. A change applied mid-track costs a gap its own size
-- a 489 ms jump placed 490 ms of silence, the discontinuity measured above
arrived at from the local end. And a delay Music Assistant is told to use can
land a minute later, at the next stream boundary, so a figure set at both ends
inside that minute settles on Music Assistant's: last-writer-wins with a late
writer rather than a server overriding anything.

Measured again with the switch off, which is the path with its own window: 21
commands over 29 seconds took **two** flash writes -- the first immediately, the
rest coalesced into one 61 seconds later carrying the value the operator ended
on. The log shows the commands stopping at 118 while flash holds 120, which is
the peer log's twenty a minute rather than a dropped figure.

## What finishing the player means

`available: true` is sent now, and it is still not the finish line, because a
player that is in the group and a quarter of a second behind it is worse than one
that never joined. What is settled is everything one Dot can check by itself:
measured on hardware, a stream is placed 134 to 164 ms after it opens against a
pipeline the player reports as 131 to 144 ms deep, and four runs of five seconds
of 25 ms chunks placed every frame with none dropped late and the mapping never
moving. docs/audio.md carries those runs.

What that cannot check is whether the mapping is *right*. The test is **two
players and one server**: put the Dot in a group with a second Sendspin player and
listen for the two to be one sound rather than two.

**That test has now been run, with three Dots on one stream, and they were one
sound.** What the logs say about it, over about ten minutes of group playback:
each reported its pipeline within 3 ms of the others -- 144, 142 and 141 ms --
every clock held under a millisecond, and between the three of them there was not
one dropped chunk, re-placement, refused write or missed reading. docs/audio.md
carries the depths, which are the cross-unit agreement the cancellation argument
needs.

What that settles is that three units on one build agree with each other. What it
cannot settle is whether all three are wrong together, because they share the
build that would make them so. A Dot against a Sendspin player that is **not** a
Dot is the test for that.

That has been tried once, against Music Assistant's own web player in Firefox on a
Mac, and it was reported as not perfectly aligned but not annoying either --
which by the ear resolution above puts it somewhere past a few milliseconds and
short of an echo. **It is not evidence against this client, and it is important not
to record it as such**, because the browser is the weaker end of that comparison
in three separate ways.

The number this client compensates by is arithmetic rather than an estimate: eight
blocks of queue at 10 ms each, plus the 58 to 64 ms of HAL buffer the driver
reports, is 138 to 144 ms, and 141 to 144 is what the three Dots measured. What is
left unaccounted on this side is only what happens after the frames the HAL counts
-- the I2S transfer, the codec's own digital filter group delay, the amplifier --
and those are microseconds to a fraction of a millisecond.

A browser's is none of those things. Web Audio to CoreAudio to the speakers is
typically tens of milliseconds on a Mac, it moves with the output device and the
buffer size, and the player has to declare it -- through
`AudioContext.outputLatency` or equivalent -- for the server to take it off the
timestamp. A web player that does not is late by exactly that, which is the
direction and roughly the size of what was heard. The two outputs are not even the
same pipeline: the browser was sent FLAC 48 kHz 16-bit **stereo** and the Dots raw
PCM 48 kHz 16-bit **mono**, so only one of the two has a decoder to account for.

So the comparison is real but unresolved, and resolving it needs an instrument
rather than an ear. Two ways, in increasing cost. Music Assistant's per-player
`CONF_SENDSPIN_STATIC_DELAY` can be nudged until the two align, and the value that
aligns them is the size of the disagreement -- ten or twenty milliseconds would sit
inside what this end's own instrument can see, and a hundred would mean something
here is wrong. That measures the gap without saying which end owns it, and it is
the operator's dial for a room rather than a number to bake into this client,
which would then be carrying a browser's error on every other player. An absolute
answer needs one microphone recording both speakers: the same content arrives
twice in one recording, so the offset is the secondary peak of its
autocorrelation, with a metre of path difference worth 2.9 ms and to be measured
and subtracted. Two Dots recorded the same way are the control, since they are
known to agree to 3 ms.

That test is worth more than any instrument on the device, and the reason is in
docs/audio.md. The Dot can read the DAC's own clock to well under a millisecond,
but it cannot see which of *its* frames is on that clock -- AudioFlinger mixes its
track into a stream that runs regardless -- so the 95 ms bridge between the two is
known only to about 3 ms. A second player cancels exactly that: the error either
speaker cannot measure in itself shows up immediately as the difference between
them.

What each pairing buys is different. **Two Dots on one build** test whether the
offset is the same on every unit, which is what a shared 95 ms has to be for it to
cancel rather than accumulate; they cannot catch the offset being wrong, because
both are wrong together. **A Dot against any other Sendspin player** catches
exactly that, because the other speaker compensates its own latency honestly, so
an error in what this client declares as `static_delay_ms` is audible as the two
drifting apart by that amount.

Ear resolution is the limit worth knowing: two speakers within about 5 ms sound
like one, 5 to 20 ms combs and hollows out, and past that it is an echo. So
listening settles anything over a few milliseconds and nothing under it. Below
that, one microphone recording both speakers and a click through the group turns
the question into a cross-correlation, and the mic's own 48 kHz is finer than
anything else in this stack -- with the acoustic path the thing to control, since
a metre of distance is 2.9 ms all by itself.

Music Assistant carries a per-player delay in its own configuration
(`CONF_SENDSPIN_STATIC_DELAY`), which is how a real installation absorbs whatever
is left after all of this. That is the operator's dial rather than a reason to
declare a wrong number: a client that reports its delay honestly starts aligned,
and the dial is for the room.

## The pairing token must not be logged

The token is `client_key || pairing_psk`, so printing it is the same act as
handing over the key file, and the daemon log is not a private place. Measured on
a Dot: `/data/local/tmp` is `drwxrwx--x` and `overdub.log` is `-rw-r--r--`, so
every uid on the device can read it by name, and the log travels in any `adb
pull` and any pasted excerpt. Logging the token would make the `0600` check in
`LoadOrCreateKeys` decorative: a mode that keeps the file from every other uid
buys nothing while the same bytes sit in a world-readable log.

So the daemon logs the `client_id` and nothing else. To read the token, be root
and derive it from the key file:

```sh
adb shell 'su -c "od -An -tx1 /data/local/bin/.overdub-sendspin-key"'
```

The last 32 bytes are the pairing PSK. The first 32 are the static private key,
which the token does not carry: it carries the **public** key, so take that from
the `client_id` the daemon logs at startup, which is that same key in base64url.
The token is then

```
SP:0 || base32(public_key || pairing_psk), '=' padding stripped, every 2 a 9
```

and the spec's own version-0 reference vector is the worked example to check an
assembly of it against. There is deliberately no flag to print it, because a flag
that writes a secret to a world-readable file is the same hazard with a switch on
it -- and no encoder in the tree either, for the reason the pairing-token section
above gives.

**If a token has already reached a log, treat it as disclosed.** Rotating is
deleting the key file and restarting: the daemon generates a fresh identity, and
the Dot then appears to every server as a **new client**, because the `client_id`
*is* the identity.

## What a peer can make the daemon do

`docs/pitfalls.md` records that a log line is an unauthenticated write to
`/data`, and `internal/untrustedlog` answers it with a 64-byte cut on peer
strings and a limit of 20 lines a minute and 5,000 a run. The Sendspin path
needs the same protection and for a sharper reason: the Sentinel PSK is a
published constant and the Dot's `client_id` goes out in cleartext in
`client/init`, so **any host that can route to `wlan0` has everything it needs**
to complete the handshake and reach the code that logs. Nothing is paired, and
nothing needs to be.

So every peer-supplied string is cut and rendered with `%q` rather than `%s`.
`%s` is not a style preference here: a `group_name` or a message type
containing a newline would otherwise forge whole log lines, and `%q` on a 65 KB
name renders at about four times its size.

Cutting at the call site is not enough on its own, which is why the line itself is
bounded as well. Peer bytes reach the log inside errors rather than only as
strings: a `json` error quotes the offending literal verbatim, and it arrives
already formatted, folded into a `%v` that no `Cut` at this end ever touched. So
`internal/untrustedlog` truncates every line it writes at 512 bytes whatever the
call site passed, and two tests hold that -- one over the log itself, one over a
number a peer chooses the length of. The count is therefore three bounds and not
two: 64 bytes per peer string, 512 per line, and the line budget below.

The limit itself is `internal/untrustedlog`'s, spent under a `Subject` of this
package's own, so the numbers cannot drift from the ones the ESPHome API keeps.
The budget is not shared with it: each `Log` counts its own lines, so what a peer
can put on the disk through both surfaces is the sum of two budgets rather than
one. It **is** shared with the switch's own lines. `switched on`, `switched off`,
`sendspin listening on` and the line that reports why `Serve` returned all go
through the same `Log`, so a peer that spends the run's ceiling takes the
operator's record of the switch with it. That is the trade SECURITY.md already
records for the button writes, and the alternative is worse: a second `Log` for
the daemon's own lines would double what reaches the disk, and these lines are
worth having inside a bound rather than outside one.

Per connection, what gets reported is what has not been said before, keyed on the
call site together with the rendered values, and capped at sixteen distinct things.
The call site is part of the key because a peer chooses the values: keyed on the
values alone, a `group_update` naming its group `stream/start` would take that key
and silence the line a real `stream/start` would have drawn. A
server that walks through message types it knows this client ignores is then
bounded twice over, by the budget and by the cap, and a seventeenth is silent. The
cap is the load-bearing half: the set records a kind whether its line was written
or dropped, so the log budget bounds the disk and not the set, and without it a
peer holding the slot grows it for the life of the connection.

`group/update` goes through the same gate, and so do the two lines a server draws
by declaring itself -- the roles it activated, and the fact that it holds none.
All three are ordinary traffic a server repeats at will, and `group/update` is the
one Music Assistant sends on every playback-state change. Reported unconditionally it spends
a line each time -- at twenty a minute against the run's five thousand, a session
changing state steadily exhausts the ceiling in about four hours, and past it
nothing a peer does is logged again. Keying on the values rather than the kind is
what keeps a genuine change visible while a repeat says nothing twice.

Connections are capped at eight, as the ESPHome API caps them, because an
unbounded accept loop is a goroutine and a read buffer per connection for as long
as a peer cares to open them, and an OOM kill brings the daemon back with the
button ungrabbed.

The five tunable windows are `Client` fields that fall back to the constants
beside `handshakeWait` when they are zero. Nothing but a test sets them, and a
test needs them short; a package-level variable would have done the same job
while racing the connections a previous test is still closing.

The handshake deadline is **absolute**: 30 seconds to finish the handshake, then
30 more to declare itself, lifted only by an activation that leaves it holding a
role. Neither window is refreshed by anything a peer sends, so the most a peer
can hold a slot without being admitted is the two of them end to end. Refreshing
one on any message would let a peer hold a slot open forever by sending something
unrecognised every 29 seconds. After
activation the idle timeout takes over at 150 seconds, refreshed by any frame -- a
pong included -- and a ping goes out every 60 seconds, so a holder whose server
loses power is noticed and its slot released rather than blocking every other
server for the full idle window.

Giving every role up starts a timer that closes the connection, and declaring a
role again stops it. Releasing the slot is not enough on its own: the slot goes
and the connection stays, and eight connections fill `maxConns`, so Music
Assistant is refused inside `Serve` before it reaches a handshake -- which is
worse than the thing releasing the slot fixed.

The allowance is for the whole connection, not for each episode of it. Taking a
role back and giving it up again earned a fresh window, so a peer could hold a
slot by cycling; eight connections rotating the hold between them then fill
`maxConns` while Music Assistant is refused at accept. Only the roleless time is
charged, so a server that narrows briefly and works in between is untouched.

A timer rather than a deadline or a check, because the two cheaper mechanisms both
fail here. A deadline on the connection does not survive: `readHeader` and `write`
arm their own from the idle window on every call. And a check between messages is
never reached, because a ping or a pong never becomes a message -- `Conn.Read`
answers it and reads on -- while still re-arming the idle deadline, so a peer that
answers keepalives and says nothing else sits inside one `Read` indefinitely,
which was measured rather than reasoned about.

`SetDeadline` sets the write deadline as well as the read one, so an absolute
provisional window bounds writes as well unless something stops it: writes start
failing 30 seconds into a session that is otherwise healthy, the connection drops,
and the server reconnects, which from the outside reads as a peer churning every
half minute. That was watched on a real Music Assistant session rather than caught
by any test here. What prevents it is that `readHeader` and `write` each arm their
own deadline from the idle window on every call, so neither inherits an absolute
one. That also means clearing the provisional deadline achieves nothing once the
idle window is set -- the next read or write overwrites it either way -- so there is
no such call, and a reader looking for one is looking for the wrong mechanism.

The same re-arming decides how the goodbye is bounded, and bounding it takes two
measures rather than one, because a write escapes a budget in two different ways.
`Client.Close` has two frames to write into a socket whose peer may have stopped
reading. A deadline set on the connection is replaced by the next write, so `Close`
narrows the session's **idle window** to `goodbyeWait`, which is the value the write
path arms from, and every write that starts after that is bounded by it.

What the narrowing cannot reach is a write already in flight. It holds the session's
write mutex under the deadline it armed on the way in, so the goodbye queues behind
that mutex for the whole 150-second idle window: measured at 3.1 seconds against a
100-millisecond budget with the window at 3 seconds. So `Close` sets the write
deadline on the socket as well, which does interrupt a blocked syscall where changing
the idle window cannot. The price is a partial frame -- the stream is unreadable from
there, so the goodbye behind it may not parse and what the peer really gets is the
close -- and that is the honest outcome rather than a regression: a session whose
write is stuck cannot be said goodbye to, and pretending it can is what cost the 150
seconds. Both measures are needed, and neither replaces the other.

Neither is sufficient either, because a deadline belongs to the connection rather
than to `Close`, and anything else holding that connection may re-arm it. `run`
does exactly that on its way in: its first act is an absolute 30-second provisional
deadline, and a session is tracked before `Greet` returns, so a `Close` landing in
between arms two seconds and then watches `run` replace it with thirty. So the
bound does not rest on a deadline at all in the end -- each goodbye carries a
watchdog that drops the connection outright once the budget is spent twice over,
and a test pins it with a connection that ignores deadlines and blocks every
write.

The budget is per write and therefore per session, so `Close` says every goodbye at
once rather than one after another. Spent serially, four stalled sessions took 2.4
seconds and eight peers would cost eight budgets, all of it landing in front of
`DenyTCP` and the flag write -- which is exactly where the switch's own promises are
kept. Three tests pin this: a session whose peer never reads, a session whose write is
already blocked when `Close` runs, and four stalled sessions at once.

The `path` TXT record in the advert is not optional: a server that cannot read a
path has nowhere to send the upgrade, and the name in it should match the one
`client/hello` carries, or the Dot is discovered under one name and introduces
itself as another.

## Uninstalling has to take the identity with it

`uninstall.sh` removes the Sendspin key alongside the ESPHome one, and reads both
back in the sweep that decides whether the uninstall succeeded. Leaving it would
not look like a failure: the daemon would be gone, the button would be Alexa's
again, and a file whose 64 bytes are the whole pairing credential would still be
sitting in `/data/local/bin`. The token derived from it stays valid, so anything
that had it could pair as a Dot that no longer runs this.

## The spec is ahead of the server Music Assistant actually runs

Every other test in this package drives both sides from one reading of the spec,
so a misreading agrees with itself and passes. `TestInteropWithTheReferenceServer`
runs a real `aiosendspin` server against this client instead. It skips unless
`SENDSPIN_INTEROP=1` and needs `uv`:

```sh
SENDSPIN_INTEROP=1 go test -run Interop ./internal/sendspin/
```

**CI runs it, in a job of its own.** It is the only check here that needs the
network while it runs, because `uv` fetches the server from PyPI, so it is
separated from the job that makes statements about the tree: an outage fails this
job, and leaves the build job still saying what it says about the tree. And with
the variable set
the test **fails** rather than skipping when `uv` is missing, because a skip would
let CI report green while the one test that checks the wire against a real peer
never ran.

Two things about driving it are easy to get wrong twice. The client id goes in as
`--client-id=<value>` rather than as two arguments, because a `client_id` is
base64url and about one in 64 begins with `-`, which the server's argument parser
would read as another flag. And the suite shares one standard logger, so a
`Client` left running past the end of its own test lands its lines in whichever
later test is counting them -- measured as a 1-in-40 failure before the helpers
took to stopping it.

It asserts what the client made of the answers, not only that the server parsed
the questions. The reference server counts the `client/time` messages it handles
and fails under three, and the Go side fails unless the daemon logged that its
clock agreed -- because the first check alone cannot fail for the failure it
exists to catch. Measured by making the client refuse every answer it gets: the
exchange count still reads 3 and the convergence check is what fails.

The literal fixtures elsewhere in this package exist because this test is not
always available. They pin what is known to matter -- `client/init`,
`client/hello`, `client/state`, the frame layout, the message types. This test is
what catches the field nobody thought to pin.

It was worth writing the moment it ran. Four differences turned up between
github.com/Sendspin/spec at `8fc2f8f` and **aiosendspin 9.1.1**, which is the
version Music Assistant's provider pins. Both ends are pinned on purpose: pinning
one and floating the other is what makes a difference impossible to attribute
later.

The fifth row is the fourth one twice: the command is named after the field, so
the library renaming one renamed the other. It is listed because
`supported_commands` is where it would be sent, not because it is a separate
disagreement.

**These are not four library bugs.** All four point the same way -- `psk_category`
present in the document and absent from the library, `supported_commands`
required in a second place, `output_delay_ms` under another name,
`supported_pair_methods` a different shape entirely -- which is what a document
moving ahead of an implementation looks like, not what four independent mistakes
look like. The spec repository has a commit from 2026-09-08 titled "Clarify
output delay in the player sync target", four days before the commit pinned
above, which is the same area as one of the four. So the library is behind the
document rather than wrong about it, and this daemon follows the library, because
the library is what answers on the wire.

| what | the spec says | aiosendspin 9.1.1 |
|---|---|---|
| noise message 1 payload | `psk_id` **and** `psk_category` | `psk_id` alone |
| `supported_pair_methods` | object keyed by method | `list[PairMethodDescriptor]` |
| `supported_commands` | in `client/state`'s player object | **also required** in `player@v1_support` |
| the player's fixed output delay | `output_delay_ms` | `static_delay_ms` |
| the command that sets it | `set_output_delay` | `set_static_delay` |

What this tree does about each:

- **An absent `psk_category` is not an error.** A missing category means the
  pre-category shape, so the `psk_id` is matched against every candidate the Dot
  holds -- the pairing PSK, then the Sentinel -- and a miss falls back to the
  Sentinel as any other miss does. Reading the category into a typed field and
  letting an empty string reach the `default` arm is what makes every handshake
  against a real server die as `unknown psk_category ""`. When the category **is**
  present it binds exactly as the spec requires.
- **No pairing method is advertised at all.** The two shapes cannot both be sent
  under one key, and the flow is not implemented in this pass, so advertising
  `pairing_psk` would be a claim this client cannot honour. Offering none is the
  honest version, and the admission rules already refuse every pairing
  activation as `method_not_supported` on that basis. The shape gets decided when
  the flow is written, against whatever the target accepts then.
- **`supported_commands` is sent in both places, and they carry different
  things.** 9.1.1 refuses `client/hello` without it in the support object, where
  the valid entries are `volume` and `mute`; the list in `client/state` accepts
  only `set_static_delay`. Sending either list's values in the other is a
  validation error at the server rather than a field quietly dropped.
- **`static_delay_ms` is what goes on the wire**, not the spec's
  `output_delay_ms`. Music Assistant's own configuration corroborates it -- its
  provider carries `CONF_SENDSPIN_STATIC_DELAY`. Sending the spec name is not an
  error either side reports: the field is simply unknown and dropped, and what
  arrives is a `client/state` with a timing field missing. The server logs
  `non-compliant client: initial client/state omitted required player timing
  fields` and, because `allow_noncompliant_clients` defaults true, **carries on
  anyway**. So this one fails by being tolerated, which is the worst way for a
  wire mistake to fail.

That last one is why the interop test scrapes the reference server's own warnings
and fails on `non-compliant` or `Malformed` appearing in them, rather than only
checking that a session came up. And the guard was proved by putting the wrong
field name back: the test fails with that exact complaint, and passes with the
right one. A check that has only ever been observed silent has not been observed
working.
