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
| tcp/6053, the ESPHome API | the Noise pre-shared key, and nothing besides. ESPHome has no peer allowlist, so the key is the whole of it, and the daemon refuses to start without one. What the key does not do is decide who gets a connection: a peer is counted against the eight from the moment it opens a socket, so anyone who can route to the Dot can hold every slot and keep Home Assistant off it, without the key and without ever sending a byte. What such a peer can spend is bounded rather than refused: one handshake wait per slot, which neither a replayed handshake nor a stalled one extends, frames bounded before they are allocated, and log lines rate-limited. Behind the key there is now one write as well as the readings: `select.<name>_action_button_mode` chooses what the daemon does with the action button, so a peer that holds the key can stop presses reaching Home Assistant by choosing `pass through`, or hand the button to Alexa while still being told about it by choosing `monitor`. It survives no restart and is logged with the address that asked, though that line shares the peer-log budget, so a peer that has already spent the run's ceiling moves the button unrecorded. What it costs beyond the button is fan-out rather than reach: a changed state wakes the sensor poll, whose uptime reading differs on every read, so cycling the modes republishes that sensor once a second instead of once a minute to every subscriber, and to Home Assistant's recorder. `wakeGap` bounds the rate; nothing bounds how long a peer keeps it up |
| udp/5353, the mDNS responder | nothing, by design: discovery answers anyone who asks, and the stock firewall already accepts the port. What it can spend is bounded instead. A reply is at most one multicast a second, so the flooding a forty-byte query could otherwise buy is capped, and the answer it draws is 328 to 700 bytes; a query carrying its own answer is not replied to at all; and no reply is ever logged, so it is not a way to write to `/data`. The records name the device, its address and the API port, which anything on the segment could learn by connecting anyway |

The rule the daemon adds matches the interface rather than a source range:
`-i wlan0 -p tcp --dport 6053 -j ACCEPT`, source `0.0.0.0/0`. So the port is
reachable from anything that can route to the Dot on `wlan0`, which is wider
than the local subnet: measured, a client arriving over a VPN from another
subnet reaches it. Narrowing that is the operator's to do in their own network,
because ESPHome has no peer allowlist for the daemon to offer.

## The install path

`deploy/install.sh` reads every step back off the device rather than trusting an
exit status, because nothing in that path reports its own failure: `cp` onto a
running binary fails silently, and `adb shell` exits 0 whatever happened
remotely. A way to make an install report success while placing something else
is a finding.
