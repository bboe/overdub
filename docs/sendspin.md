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
