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
`static_delay_ms` and `required_lead_time_ms` are both **0**, which is honest and
temporary: nothing plays audio yet, so there is no output path whose latency could
be measured, and the spec allows both to be updated mid-session once there is.

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

The advert goes up only once the listener is behind it. `startSendspin` opens the
socket before the responder is built, and a Dot that could not read its key or
could not bind the port advertises no Sendspin service at all rather than
advertising one that refuses every connection.

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

## Reporting itself unavailable, on purpose

Once a server activates `player@v1`, the Dot sends `client/state` with
**`available: false`**. That is the honest answer until two things exist: a
converged time filter, which the spec makes a precondition for a player
reporting itself available, and an audio path to play into. `supported_commands`
is present and empty, because the field advertises settability rather than
reportability, and a player that accepts no commands still has to say so.

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

The four tunable windows are `Client` fields that fall back to the constants
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

The literal fixtures elsewhere in this package exist because this test is not
always available. They pin what is known to matter -- `client/init`,
`client/hello`, `client/state`, the frame layout, the message types. This test is
what catches the field nobody thought to pin.

It was worth writing the moment it ran. Four differences turned up between
github.com/Sendspin/spec at `8fc2f8f` and **aiosendspin 9.1.1**, which is the
version Music Assistant's provider pins. Both ends are pinned on purpose: pinning
one and floating the other is what makes a difference impossible to attribute
later.

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
- **`supported_commands` is sent in both places.** It costs nothing and 9.1.1
  refuses `client/hello` without it in the support object.
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
