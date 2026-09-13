# Security

## Reporting

Report anything exploitable privately, through the **Report a vulnerability**
button on this repository's Security tab, rather than as a public issue. That
opens an advisory only you and I can see.

This is a spare-time project on one device, so expect an acknowledgement rather
than a schedule.

## Supported

The tip of `main`. There are no releases, and nothing is backported.

## What is not a finding

This runs as root on a device you rooted yourself, and it installs over `adb`.
Root on the Dot is where this starts rather than something it defends: a shell
there can already do everything the daemon does. What is worth reporting is
anything that hands that reach to somebody who does not have it.

## What guards what

| Surface | What guards it |
|---|---|
| tcp/6053, the ESPHome API | the Noise pre-shared key, and nothing besides. ESPHome has no peer allowlist, so the key is the whole of it, and the daemon refuses to start without one. What the key does not do is decide who gets a connection: a peer is counted against the eight from the moment it opens a socket, so anyone who can route to the Dot can hold every slot and keep Home Assistant off it, without the key and without ever sending a byte. What such a peer can spend is bounded rather than refused: one handshake wait per slot, which neither a replayed handshake nor a stalled one extends, frames bounded before they are allocated, and log lines rate-limited. Behind the key there are writes as well as the readings, and `select.<name>_action_button_mode` is the one that costs most: it chooses what the daemon does with the action button, so a peer that holds the key can stop presses reaching Home Assistant by choosing `pass through`, or hand the button to Alexa while still being told about it by choosing `monitor`. That one survives no restart. `switch.<name>_sendspin` is the exception to that rule and the only write here that outlives a reboot, because it is kept in a property rather than in memory -- so a peer holding the key can leave Sendspin off, or back on, for every boot that follows. Each write is logged with the address that asked, though those lines share the peer-log budget, so a peer that has already spent the run's ceiling moves the button unrecorded. What it costs beyond the button is fan-out rather than reach: a changed state wakes the sensor poll, whose uptime reading differs on every read, so cycling the modes republishes that sensor once a second instead of once a minute to every subscriber, and to Home Assistant's recorder. `wakeGap` bounds the rate; nothing bounds how long a peer keeps it up |
| udp/5353, the mDNS responder | nothing, by design: discovery answers anyone who asks, and the stock firewall already accepts the port. What it can spend is bounded instead. A reply is at most one multicast a second, so the flooding a forty-byte query could otherwise buy is capped, and the answer it draws was measured at 328 to 700 bytes when the Dot advertised one service, which a second service raises by that service's own records; a query carrying its own answer draws no reply for the service that answer names, and none at all when it names them all; and no reply is ever logged, so it is not a way to write to `/data`. The records name the device, its address, the API port, and -- since the Dot speaks Sendspin -- the Sendspin port, the `path` a server needs to reach it, and a token that changes when the Dot reboots. All of that except the token is learnable by connecting anyway; the token is published here and nowhere else, and what it reveals is that this Dot has rebooted since a server last saw it, which is the whole point of sending it |
| tcp/8928, the Sendspin client | **no secret, and unlike udp/5353 that is not the intent.** The Sentinel PSK is a published constant, so any host that can route to the Dot on `wlan0` completes the Noise handshake with it, and the Dot offers `unpaired_access`, so such a peer may reach playback. What that buys is the single exclusive slot: while it holds it, Music Assistant is refused `concurrent_attempt`, and the Dot stays with whichever server arrived first because no ranking or displacement is implemented. What it cannot do is spend the Dot without bound: eight connections is the ceiling, a peer gets 30 seconds to handshake and 30 more to declare itself, giving every role up puts it back under that second window, a message is bounded in frames as well as bytes before anything is allocated, and every peer-triggered line is charged to a log budget of this surface's own -- so the two surfaces sum rather than share -- with every peer-supplied string cut and quoted, except the activated roles, which are values of ours that a peer can only choose from. The pairing flow is what replaces the Sentinel with a per-server key, and it is not implemented yet, so nothing here is guarded by a credential the operator controls. What the operator does control is whether the surface exists: `switch.<name>_sendspin` withdraws the mDNS advert with a goodbye, closes the listener, deletes this rule and stops re-asserting it, and ends the session already running, so an operator who does not use Music Assistant can have the port gone rather than merely unused, and the setting survives a reboot in `persist.overdub.sendspin`. That switch is behind the ESPHome key and nothing more, so a peer holding that key can turn the surface back on -- it bounds who reaches Sendspin by the API's credential rather than adding one of its own. The rule the daemon adds is opened only after the socket binds |
| the Amazon refresh token, where `mapdump.jar` is installed | nothing beyond the API key. MapDump reads it out of MAP's own store as MAP's uid, the daemon holds it in memory for the life of the process and never writes it to disk or logs it, and the cookies it trades it for are struck out of every error it reports. What that does not bound is who can spend it: anyone holding the API key can run any Alexa command, and the cookies the token buys are scoped to the whole retail account rather than to Alexa. Removing the jar stops new extractions and revokes nothing already taken; deregistering the Dot is what revokes it |

The rules the daemon adds match the interface rather than a source range:
`-i wlan0 -p tcp --dport 6053 -j ACCEPT` and the same for `8928`, source
`0.0.0.0/0`. So the ports are reachable from anything that can route to the Dot
on `wlan0`, which is wider
than the local subnet: measured, a client arriving over a VPN from another
subnet reaches it. Narrowing that is the operator's to do in their own network,
because ESPHome has no peer allowlist for the daemon to offer.

## The install path

`deploy/install.sh` reads every step back off the device rather than trusting an
exit status, because nothing in that path reports its own failure: `cp` onto a
running binary fails silently, and `adb shell` exits 0 whatever happened
remotely. A way to make an install report success while placing something else
is a finding.
