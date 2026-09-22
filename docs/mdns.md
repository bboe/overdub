# Finding the Dot

The mDNS responder, in `internal/mdns`. Without it Home Assistant adds the Dot
by address, and only a DHCP reservation keeps that address true. Messages go
through `golang.org/x/net/dns/dnsmessage`; docs/constraints.md says why.

## Why not the daemons already running

The Dot runs two mDNS implementations before this one starts:

```
root  168   avahi-daemon: running [none.local]
mdnsr 2530  /system/bin/mdnsd            (init.svc.mdnsd: running)
```

- **avahi** has no configuration on the device: no `/etc/avahi`, no
  `services/` directory. It answers as `none.local`.
- **mdnsd** registers services, but its host is `Android.local`. The name is
  compiled in, not taken from the hostname (`localhost`). Three Dots would all
  claim `Android.local`.
- Registering under a host of our own answers `error=0` and then sends nothing:
  no probe, no announcement. `reg_record_request` drops the connection.
- `init.rc` marks mdnsd `disabled` and `oneshot`. Only `NsdService` starts it,
  and init does not restart it.

## One socket for every service

- Each service is an `Advert` in `Services`. `esphome.Advert` builds ESPHome's,
  because `api_encryption` and the version string are ESPHome's knowledge.
- One responder, not one per service:
  - the once-a-second multicast limit is per responder, so two would answer
    one query twice;
  - `Goodbye` retires one responder, so two would retire half a device.
- `udp/5353` already carries Alexa's sockets, so the bind needs `SO_REUSEADDR`
  and `SO_REUSEPORT`.

## The list changes while the responder runs

- `Advertise` replaces the list, because the Sendspin switch removes and
  restores one service without a restart. The live list is under the
  responder's lock; `Services` is only the starting value.
- A withdrawn service gets its own goodbye: PTR, SRV and TXT at TTL 0, sent
  twice. Without it a browser holds a record for a closed port for the rest of
  the TTL, which reads as a broken Dot.
- A withdrawal carries no A record (`noHost`). The address is still true, and a
  TTL-0 A record tells every browser to forget the host.
- A withdrawal sets a write deadline, because it is on the path that switches
  Sendspin off, and a peer that stops draining would hold the switch. `Goodbye`
  does not clear its deadline: it runs once, at shutdown.
- `Advertise` compares by service name and sends only the difference. An added
  service gets an announcement; an unchanged one gets nothing.
- `Advertise` validates the new list, not the list in force, so an unpackable
  service cannot pass on its predecessor's record.

## The announcement ladder

- RFC 6762 §8.3 asks for at least 2 unsolicited responses 1 second apart, and
  allows up to 8, each gap twice the last. This responder sends 7: at 0, 1, 3,
  7, 15, 31 and 63 seconds.
- The rungs protect the **first** sighting against lost packets.
- They do not re-prompt a peer that already holds the records. A repeat with
  identical data is a refresh, and python-zeroconf (which Music Assistant uses)
  fires no callback for it. Recovery is the client's own retry, or the changed
  TXT record in docs/sendspin.md.
- The ladder runs on a goroutine, because it spans 63 seconds. `disable` joins
  the advertising goroutine before it closes the listener, and `enable`
  advertises from its own goroutine, ahead of the API bind at startup.
- A withdrawal cancels the pending ladder. Otherwise the next rung announces
  the withdrawn service back into every cache it just left.
- `Goodbye` needs no cancel: every rung checks `gone`. A rung also stops when
  `m.conn` is no longer the socket it started on.
- Cancelling alone leaves a rung that already passed the checks. So
  `announceOnce` reads a generation counter, `announceGen` re-reads it under
  the lock before it sends, and `Advertise` bumps it when it swaps the list.
  Without this, the withdrawn service goes out at `ttlShared` after its own
  goodbyes, and a server holds a dead port for 75 minutes.
- With the counter removed, the race hit 4 times in 10 with a barrier releasing
  both goroutines, a few times in 100 with a real ladder, and never on a single
  processor.
- The counter is checked under the lock, but the write is not. A swap in that
  gap still sends the old set: measured once in 4,000. Closing it means holding
  the mutex across a UDP write, which every query reply waits on.
- An added service still bumps the generation, so a rung mid-build drops its
  write and the next rung carries the set. Sendspin switched off before `wlan0`
  has an address costs 1 of the 7 announcements this way.

## Rate limits are the only cap on amplification

- 40 bytes of query draw 328 to 700 bytes back, to every host on the segment.
- A reply to a query is multicast at most once a second. The budget is the
  responder's, not each service's.
- Announcements and goodbyes do not consult the budget, so toggling the
  Sendspin switch sends multicast the limit does not govern. They do spend it:
  an announcement stamps `lastSent`, so it can suppress a reply in the next
  second.
- python-zeroconf sets the unicast bit on a browser's first query, which falls
  under the 20-a-second unicast limit. 2 browsers 36 ms apart were both
  answered. Unicast is not unlimited because a spoofed source would make the
  Dot a reflector.
- Known-answer suppression is per service **and per question**. Per packet, a
  host holding the ESPHome PTR would silence every other service it asked
  about, which is the shape python-zeroconf sends.
- Known answers start after *all* the questions. Home Assistant sends about 90
  questions in one packet.

## Things that would read as bugs

- Nothing probes for the name or detects a conflict; the read loop discards
  responses. Two Dots with the same `-name` both assert the same records, and
  Home Assistant sees one device flapping between two addresses. A spoofed
  goodbye retires the Dot from every cache on the segment.
- Nothing on the reply path is logged, because a peer causes every reply. A
  failed send counts toward a rebuild only when it went to the group: a querier
  can name an unroutable source, and counting that would let anyone force a
  rebuild every poll.
- 7 deliberate divergences from the RFC. None stops python-zeroconf resolving
  the device:
  - SRV and TXT go in the additional section of a browse reply;
  - no NSEC for the AAAA the device does not have;
  - the source port is never read, so a legacy querier gets an ordinary reply;
  - shared records skip the 20-120 ms delay, which spreads multiple responders;
  - `_services._dns-sd._udp.local.` goes unanswered, so a general browser never
    finds the Dot;
  - a truncated query is answered at once, not held for its known answers;
  - only a known PTR suppresses; a known SRV, TXT or A still draws a reply.
- A reply carries only what was asked, in the answer section: a browse gets the
  PTR, a resolve the SRV and TXT, a host question the A record. A strict peer
  may ignore a response whose answer section is empty or answers another name.
- One question asking for a multicast reply makes the whole reply multicast,
  and puts it on the once-a-second budget. No client here sends one.
