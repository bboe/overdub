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

## The rate limits are the only cap on amplification

Forty bytes of query draw 328 to 700 back, at every host on the segment. A
record is multicast at most once a second, and that budget is the responder's
rather than each service's, so two multicast questions in the same second draw
one reply between them. python-zeroconf sets the unicast bit on a browser's
first query and so lands on the twenty-a-second unicast limit instead; measured,
two browsers 36 ms apart were both answered. Unicast is held to twenty rather
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
