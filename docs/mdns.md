# Finding the Dot

The mDNS responder, in `internal/mdns`. Without it the Dot is added to Home
Assistant by address, and a DHCP reservation is the only thing keeping that
address true. The messages are built and read with
`golang.org/x/net/dns/dnsmessage`; docs/constraints.md says why that import is
here and an mDNS library is not.

## Why not the daemons already running

The Dot runs two mDNS implementations before this one starts, so this responder
is the third. Measured:

```
root  168   avahi-daemon: running [none.local]
mdnsr 2530  /system/bin/mdnsd            (init.svc.mdnsd: running)
```

**avahi** has no configuration on the device at all -- no `/etc/avahi`, no
`services/` directory -- so there is nowhere to drop a service file, and it
answers as `none.local`.

**mdnsd** was tried, and it does work as far as registering: `error=0`, the
record on the wire, `dns-sd` resolving it from the laptop. Three things then
stop it.

The host is `Android.local` and does not come from the hostname. This Dot's
kernel hostname is `localhost`; upstream mDNSResponder derives the label from
`gethostname()` and falls back to `Computer`, but this build carries both the
`Android` and the `Computer` literals, so it is compiled in. Setting the
hostname changed nothing, and there is no `hostname` command on the device to
try -- it would only be `sethostname(2)`, the same kernel state. The binary's
own `Local Hostname %#s.local already in use` string says what three Dots
claiming it would do to each other.

Registering under a host of our own answers `error=0` and then **advertises
nothing at all** -- no probe, no announcement, silence on the wire -- and
`reg_record_request`, which would add the matching A record, drops the
connection. The failure is not reported, which is worse than a refusal.

And `init.rc` has it `disabled` and `oneshot`: only `NsdService` starts it, and
init will not bring it back.

`mdnsd` does do the probing and conflict detection this responder does not --
for `Android.local`, which is the whole problem.

## One socket for every service

Each service is an `Advert` in `Services`; ESPHome's is an element of that list
like any other, and `esphome.Advert` builds it because `api_encryption` and the
version string are ESPHome's knowledge.

This is not working around a delivery problem. Measured on Linux, two sockets in
one `SO_REUSEPORT` group **both** receive a multicast datagram -- 2 of 2 -- so a
responder per service would have worked. What decided it: the once-a-second
multicast limit below is per responder, so two of them answer one query twice
with nothing bounding the pair; `Goodbye` retires one responder, so two of them
retire half a device; and `udp/5353` already carries Alexa's sockets, which is
why the bind needs `SO_REUSEADDR` and `SO_REUSEPORT` at all.

## The list can change while the responder is running

`Advertise` replaces the list of services, because the Sendspin switch takes one
away and puts it back without restarting anything. `Services` is what the
responder was constructed with; the list it answers for is separate state under
the responder's lock, copied from `Services` on first use, and every reader goes
through the same accessor. A test changes the list from one goroutine while
another builds records, so removing that lock is a `DATA RACE` rather than a
difference nothing reports.

A withdrawn service gets a goodbye of its own -- its PTR and its SRV and TXT at
TTL 0, multicast twice -- rather than being left to expire. Without it a server
that has already browsed keeps a record pointing at a port the daemon has just
closed, for as long as the original TTL had left, which looks exactly like the
Dot being broken rather than switched off. `Goodbye` at shutdown retires the
whole device; this retires one service and leaves the rest answering, which is
why it is not the same code path.

That a goodbye reaches a cache rather than only leaving the Dot was measured
against a real querier on the segment: browsing `_sendspin._tcp.` found the Dot
once before the switch went off and nothing after it. A record is not proved gone
by the packet going out, because the receiver decides what it keeps, so the
reading is on the querier's side rather than ours.

**Two announcements are the floor, not the plan.** RFC 6762 section 8.3 asks for at
least two unsolicited responses a second apart, and allows up to eight so long as
each gap is twice the last. This responder sends seven: one at once, and
six more at gaps of 1, 2, 4, 8, 16 and 32 seconds, so the rungs land 1, 3, 7, 15,
31 and 63 seconds after the first. What they buy is delivery of the **first** sighting: a
peer that never had these records learns them from whichever rung arrives, so a
pair of lost packets on a busy segment no longer costs a discovery.

What the rungs do **not** buy is a second chance with a peer that already holds
the records, and it is worth saying so here because the opposite is the obvious
guess. A repeat carrying identical data is a refresh, and a querier is entitled to
treat it as one: python-zeroconf, which Music Assistant browses through, fires no
callback at all for a record already in its cache. So a rung cannot re-prompt a
server that was dropped while the firewall rule was missing, and the rungs are
not what recovers that case -- the client's own retry is, or, once it has given
up, the changed TXT record docs/sendspin.md describes.

The rungs run on a goroutine, and that is forced rather than tidy: a ladder
spanning 63 seconds cannot be on any caller's path. `disable` joins the
advertising goroutine before it closes the listener, so a synchronous ladder
would hold switching **off** for the whole minute. `enable` advertises on a
goroutine of its own, so `Advertise` is not on the path to the API bind either.

A withdrawal cancels the pending ladder, and it has to. `Advertise` retires a
dropped service at TTL 0, and a rung firing a second later would announce it
straight back into every cache it had just left -- so switching Sendspin off would
close the port while the network was still being told where to find it. Without
the cancel that is reproducible: two announcements follow the goodbye. `Goodbye`
needs no such call, because it marks the responder gone and every rung checks
that. Neither does the teardown in `serve`, because a rung compares against the
socket the responder holds **now** rather than the one it held when the ladder
started: `m.conn` ceasing to be that socket stops the next rung. Comparing
against what it was at the start would not, and the difference is reachable --
`Advertise` reads `m.conn` and then reaches `announce` a full `goodbyeGap` later
on the withdrawal path, so an `Advertise` overlapping a teardown arrives holding a
socket the responder has already let go of. What that still leaves is one rung's
worth of race: the check is under the lock and the write is not, so a teardown
landing in between costs a write to a closed socket and one recorded send failure,
which the next cycle clears. Prompt rather than absolute.

Cancelling the ladder is not enough on its own, because a rung that is already
past those checks has not built its payload yet. So `announceOnce` reads a
generation counter before it builds, and `announceGen` re-reads it under the
same lock that decides whether to send; `Advertise` bumps the counter in the
critical section where it replaces the advert set. A payload built from the set
being withdrawn is dropped rather than written. Without that the withdrawn
service goes back out at `ttlShared` **after** its own goodbyes, and a server
holds a dead port for the full 75 minutes -- the one outcome the goodbyes exist
to prevent. It is reachable rather than theoretical, and how reachable depends
entirely on the harness: with the counter removed, a barrier releasing both
goroutines together hits it four times in ten, a real ladder against a narrow
window a few times in a hundred, and a single processor never. Any single figure
here would describe the harness rather than the hazard.

Like the socket check above it, this is prompt rather than absolute. The decision
is taken under the lock and the write is not, so a swap landing in that gap still
puts the old set on the wire -- measured with the counter in place, once in four
thousand. Closing it would mean holding the responder's mutex across a UDP write,
which is the lock every query reply waits on, so the window stays and this
paragraph is the record of it.

The withdrawal carries no address record, and brackets its writes with a write
deadline. `withdrawal` sets `noHost`, so `recordsFor` leaves the A record out:
the services are what is going away, the Dot's own address is still true, and a
TTL-0 A record would ask every browser on the segment to forget the host too.
The deadline matters because this is the path that switches Sendspin **off** --
a peer that has stopped draining would otherwise hold the switch open for as
long as the kernel allows -- and `Advertise` clears it afterwards because the
socket outlives the withdrawal. `Goodbye` sets the same deadline and deliberately
does not clear it: it runs once at shutdown, with nothing after it to unblock.

An added service draws a fresh announcement, and one that was already in the list
draws neither: `Advertise` compares by service name and sends only for the
difference, so calling it with the list already in force is silent on the wire.
It is not free of effect: the generation moves whatever the list says, so a rung
inside its own build at that moment drops that write and the rung after it
carries the set instead. Sendspin toggled off before `wlan0` has an address
reaches exactly that, and the cost is one first-sighting announcement of seven.

The validation runs before the swap, and it has to pack **the list being
checked** rather than the one in force -- otherwise a change is validated against
what it replaces, and a service that cannot be packed is accepted because its
predecessor could. Nothing reachable today distinguishes the two: every candidate
is caught by the explicit checks in front of the dry run, so this is a backstop
for a record shape that does not exist yet rather than a bug that was observed.

## The rate limits are the only cap on amplification

Forty bytes of query draw 328 to 700 back, at every host on the segment. A
record is multicast at most once a second **in reply to a query**, and that
budget is the responder's rather than each service's, so two multicast questions
in the same second draw one reply between them. Announcements and goodbyes do
not consult that budget -- they are ours to send, not a peer's to draw -- so an
operator toggling the Sendspin switch produces multicast the limit does not
govern, paced only by the second between the two sends each of them makes. They
do spend it, though: an announcement stamps `lastSent` whether or not it ends up
writing, so a reply in the second after one can be suppressed by it.

python-zeroconf sets the unicast bit on a browser's first query and so lands on
the twenty-a-second unicast limit instead; measured, two browsers 36 ms apart
were both answered. Unicast is held to twenty rather
than one because those replies reach only the host that asked, and a spoofed
source would otherwise make this a reflector.

A known answer is the other cap, and it is easy to get wrong. Suppression is
decided per service **and per question**: a known PTR retires that service's PTR
and leaves every other service in the reply, and it does not retire the SRV and
TXT a resolver asks for next, which the same packet often asks for in its second
question. Deciding it per packet lets a host holding the ESPHome record silence
the answer for every other service it asked about -- which is the shape
python-zeroconf sends. The known answer is read from the answer section, which
begins after *all* the questions, and Home Assistant sends about ninety in one
packet.

## Things that would read as bugs

**Nothing probes for the name, and nothing notices a conflict.** The Dot claims
its host and instance records as unique without asking, and the read loop
discards responses. Two Dots given the same `-name` both assert the same records
and Home Assistant sees one device flapping between two addresses; README says
the name has to be unique, and that is the whole of the enforcement. The same
deafness means a spoofed goodbye retires the Dot from every cache on the
segment.

**Nothing on the reply path is logged**, because a peer causes every reply. A
failed send is kept for the next address poll instead, and counts only when it
was aimed at the group: a querier can name somewhere unroutable, and counting
that as ours hands anyone on the subnet a rebuild every poll.

**Seven divergences from the RFC are deliberate**, and none stops
python-zeroconf resolving the device: a service's SRV and TXT sit in the
additional section rather than beside the PTR answering a browse; no NSEC for
the AAAA this device does not have; the source port is never read, so a legacy
querier is answered as an ordinary one; shared records skip the 20-120 ms
collision delay, meant to spread responders this network has one of; the
`_services._dns-sd._udp.local.` meta-query goes unanswered, so a general browser
never learns the Dot exists; a truncated query is answered rather than held for
the known answers that follow; and a known answer is honoured only when it is a
PTR, so a querier repeating our SRV, TXT or A still draws a full reply, which
section 7.1 covers for any type.

**A reply carries only what was asked about**, and which section a record sits
in follows the question: a browse draws the PTR, a resolve the SRV and TXT with
no PTR at all, a host question the A record -- each in the answer section,
because a response whose answer section is empty, or which answers with a record
of another type and owner, is one a strict peer may never read. One question
asking for a multicast reply makes the whole reply multicast, which moves a
mixed packet onto the once-a-second budget; no client here sends one.
