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
arriving mid-fragment, and an unknown opcode. Lengths are checked against the
read limit **before** the payload is allocated, and again as fragments
accumulate, so neither a single frame nor a long chain of them can make the
daemon allocate past the limit.

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
the spec. It is matched here because a token
is only useful if both ends spell it the same way, and the published vector is
what settles the spelling. Meeting that vector without the transliteration is
impossible, and getting it wrong produces a token that looks right and decodes to
rubbish.

Two published constants are asserted against the spec rather than trusted: the
Sentinel PSK, `SHA-256("sendspin-sentinel-psk-v1")`; and its `psk_id`, which is
the general rule `base64url(SHA-256("sendspin-psk-id-v1" || PSK))` applied to it.
Both matched on the first run, which is the only reason the derivations above can
be stated as facts.

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

## Uninstalling has to take the identity with it

`uninstall.sh` removes the Sendspin key alongside the ESPHome one, and reads both
back in the sweep that decides whether the uninstall succeeded. Leaving it would
not look like a failure: the daemon would be gone, the button would be Alexa's
again, and a file whose 64 bytes are the whole pairing credential would still be
sitting in `/data/local/bin`. The token derived from it stays valid, so anything
that had it could pair as a Dot that no longer runs this.
