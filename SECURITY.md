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
| tcp/6053, the ESPHome API | the Noise pre-shared key, and nothing besides. ESPHome has no peer allowlist, so the key is the whole of it, and the daemon refuses to start without one. What the key does not do is decide who gets a connection: a peer is counted against the eight from the moment it opens a socket, so anyone who can route to the Dot can hold every slot and keep Home Assistant off it, without the key and without ever sending a byte. What such a peer can spend is bounded rather than refused: one handshake wait per slot, which neither a replayed handshake nor a stalled one extends, frames bounded before they are allocated, and log lines rate-limited |

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
