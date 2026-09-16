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

Nothing reads the converged mapping yet. What it produces is one log line per
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
**`available: false`**. The spec's precondition for a player reporting otherwise
is a converged time filter, which now exists; what does not is an audio path to
play into, so the answer stays false and stays honest. `supported_commands`
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
