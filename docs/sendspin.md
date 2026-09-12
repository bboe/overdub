# Sendspin

Sendspin (ex-"Resonate", Open Home Foundation) is the multi-room audio protocol
Music Assistant ships as its native playback provider. `internal/sendspin`
implements the **client** side of it, so the Dot's speaker joins a synchronised
group rather than only answering Home Assistant as an ESPHome device.

Spec: github.com/Sendspin/spec, pinned at commit `8fc2f8f` (2026-09-12). The
repository carries no tags or releases, so a commit is the only citable reference.
Read it rather than this page for what the protocol says; this page carries what
was measured here and what was decided against.

The commit matters because that repository carries **no tags and no releases** and
is revised continuously -- there is no "version 1.2" of this protocol to cite,
only a commit. `version: 1` on the wire is the *core* version and does not move
when the document does: every difference found against it so far sits inside
declared version 1. So an unpinned citation rots silently, and a difference
nobody can re-check reads as current when it is not.

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

## Uninstalling has to take the identity with it

`uninstall.sh` removes the Sendspin key alongside the ESPHome one, and reads both
back in the sweep that decides whether the uninstall succeeded. Leaving it would
not look like a failure: the daemon would be gone, the button would be Alexa's
again, and a file whose 64 bytes are the whole pairing credential would still be
sitting in `/data/local/bin`. The token derived from it stays valid, so anything
that had it could pair as a Dot that no longer runs this.
