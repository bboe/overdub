# Things that fail silently

**A file mode is invisible to everything else here.** gofmt, vet, the tests and
shellcheck all read the shell scripts rather than execute them, and the build
runs `./build.sh` and never `install.sh`. So a script that lost its executable
bit passes the whole suite and fails for the first person to run the command
README.md gives them.

CI asserts that every tracked `.sh` is `100755`, as a rule rather than a list,
so a script added later is covered without being remembered. Measured the hard
way: `install.sh` was once committed `100644`, and the whole suite passed.

**`install.sh` can lie, so it verifies itself.** `cp` onto the running binary
fails with ETXTBSY, silently: toolbox `cp` prints "Text file busy" and exits 0,
and `adb shell` exits 0 whatever happened remotely, so `set -e` catches nothing.
An install can report success, change nothing, and take a reboot to notice. The
binary is replaced by rename and its md5 checked against the build.

**By hash rather than size, because size does not separate two builds.** A
binary replaced during an install here measured byte-for-byte the same size as
the one replacing it, so a silently failed `cp` would have passed a size check.
`build.sh` pins `-buildvcs=false` to make the comparison mean something: Go
otherwise stamps the commit, its timestamp and a dirty flag into the binary, all
three fixed-length, so builds of different source differ in content while
matching exactly in size.

Without the stamp the same tree always gives the same bytes, measured across
repeat builds, a different directory, a checkout with no `.git`, and a dirty
tree.

That last one is the one to say out loud, so `install.sh` does. Reproducibility
here means the binary cannot tell you what it was built from, and `git checkout`
carries modified files across a branch change: the checkout reads as a revert,
the build is not one, and every check downstream then passes on the wrong
binary, twice in a row, printing "binary verified" each time. Measured the hard
way.

Every step is read back, because not one of them reports its own failure: the
binary is hashed, and the boot script compared against what was pushed. The boot
script matters most: it is the only reason anything runs at all, and the path
it goes to is the one this device's Magisk 17.3 uses. A Magisk that keeps
`service.d` somewhere else has no such directory, so the `cp` fails, the `rm`
beside it runs regardless, and the Dot does not come back from its next reboot
with no log to read, because that script is what creates it. Which versions
share the 17.3 layout is not something measured here.

The restart check goes through a function for the same class of reason: a
pipeline reports only its last command, so with `adb` inline a pulled cable read
as an empty pid, which is what this script says when nothing was supervising the
daemon at all.

**The Dot's own firewall.** FireOS runs iptables with `INPUT policy DROP` and a
port allowlist, and `tcp/6053` is not on it, so Home Assistant times out adding
the device **with nothing in the daemon log**: the SYN never reaches userspace.
The daemon opens it itself, in `internal/device/firewall.go`. `iptables -L
INPUT -n -v | grep 6053` shows a packet counter, which separates "the device
dropped it" from "the network did".

`AllowTCP` checks and then appends, which is two calls and not one. One port
needed no lock. With the adb select there are two, reached from the sensor poll
and from the adb worker, and two arriving together both find their rule absent
and both append it. The `-D` that closes a port removes one copy, so the chain
keeps an ACCEPT nothing will ever delete for a port the select truthfully
reports as closed. The chain is not ours alone either, so nothing tidies it up
later. One mutex over both mutations is the whole fix.

`-w` on this iptables takes no seconds argument: it waits for the xtables lock
for as long as it takes, and netd holds that lock constantly. Ten seconds, so a
held lock is reported rather than waited on. The one-shot call at startup says
so; the thirty-second re-assert says so once and then goes quiet, because netd
rebuilding the chain is the ordinary case and a line every thirty seconds for
the rest of the boot would bury everything else.

**Anything done once at daemon startup races the network.** On a cold boot, which
a warm restart hides entirely:

| Uptime | What is true |
|---|---|
| 0-15s | `wlan0` does not exist at all |
| ~26s | `sys.boot_completed`, boot script runs, daemon starts |
| later | netd rebuilds the iptables INPUT chain, discarding our rule |

Both are live hazards: a daemon that reads the MAC once starts before there is
an interface to read it from, and a rule added at boot is wiped afterwards.
Measured on a cold boot: the daemon logged `waiting for wlan0 to appear` and
waited 15 seconds, then added its rule, which was gone by 49 seconds and back by
64. Setup that depends on the network must wait or re-assert, never run once.

**A log line is an unauthenticated write to `/data`.** Every line the API logs
is there because a peer did something, and `%q` renders a frame of `\xff` as
four times its size on one line: measured, one `HelloRequest` wrote 131,207
bytes. The log is truncated at boot and every twentieth restart, and a peer
writing to it never makes the daemon exit, so neither truncation arrives. Peer
strings are cut to 64 bytes before they are quoted, and API lines are limited
twice over: 20 a minute, and 5,000 for the run. The rate alone is not enough:
21 lines a minute at the measured 311-byte worst case is 9 MB a day, and
nothing truncates it. After the ceiling nothing a peer does is logged again
until the daemon restarts, the count of what was dropped included, because a
line a minute saying so grows the same file. Connection churn is the same
hazard at a smaller size, a connect and a disconnect line apiece, so those are
limited too rather than only the lines carrying a peer's own bytes.

**An empty pre-shared key is not a weak key, it is no key.** `flynn/noise` reads
an empty `PresharedKey` as *no psk modifier at all*, so `NNpsk0` quietly becomes
plain `NN` and every peer on the subnet completes the handshake. It fails open,
and it fails silently: nothing errors. Measured by asking it for a first
handshake message each way: 48 bytes with a 32-byte key, and **32 bytes with a
nil one**, which is the psk contribution simply missing. So `noiseAccept` checks
the length before it uses the key, and `TestAServerWithNoKeyRefusesEveryone`
fails if that check is removed. `DecodeNoisePSK` means the one caller cannot
reach it today, but a key that is configured and not enforced only looks like
protection.

**udp/5353 needs no rule, and the port is already taken.** Unlike tcp/6053, the
stock INPUT chain accepts `udp dpt:5353` outright: measured on a Dot, that rule
is there with 15,293 packets against it, because Alexa runs its own mDNS. The
same fact is why the socket sets `SO_REUSEADDR` and `SO_REUSEPORT` before
binding: two sockets are already on `0.0.0.0:5353`, and without both options the
bind fails rather than sharing. So the responder needs nothing from the firewall
and everything from the socket options, which is the reverse of the API.

**`adb` merges the device's stderr into its stdout**, so every read-back in the
deploy scripts is one line of noise away from a wrong answer. A linker warning
from `su` prepended to a key file makes it decode to nothing; taken as the answer
to `[ -x ... ]` it is not `yes`; counted as "anything still on the device" it is
six things left behind. So no read compares the whole stream against a literal.
Each one matches the shape it expects, and the ones whose answer is a decision
rather than a value end the remote command with a word of their own and demand
it, because otherwise silence and "no" are the same reading, and a dropped cable
becomes a confident wrong diagnosis.

**`adb push` does not carry the local mode, and `/data/local/tmp` is `0771`.**
A key written locally as `0600` lands on the device as `0666`, and the `o+x` on
that directory lets any uid reach it by name, so staging the key there hands it
to every app on the device for as long as it sits there. Nothing reports this:
the install succeeds, the key is correct, and the mode it finally lands with is
right. Measured both ways on a Dot: from `/data/local/tmp` an app uid reads the
staged key, and from a `0700` directory of our own the same read is denied. So
`install.sh` makes that directory first and removes it after. The binary and the
boot script go through the shared one still, because neither is a secret: the
binary is read back by hash, and the boot script compared against what was
pushed.

**Which socket options qemu-user implements depends on the qemu build**, so a
socket test can pass in one emulator and fail in another for reasons that have
nothing to do with the code. Under `qemu-arm-static` on x86-64, which is
what CI runs, *setting* `IP_MULTICAST_IF` fails with `protocol not available`,
so the responder cannot open a socket at all. Under Docker's `linux/arm/v7` and
`linux/arm64` it is set without complaint. `IP_ADD_MEMBERSHIP` on the line
before succeeds everywhere, so the socket opens far enough to look right either
way.

Reading an option back is a separate question from setting it, and the answer
is not qemu's. Linux refuses `getsockopt(IP_MULTICAST_IF)` with the same
`ENOPROTOOPT`, measured on native arm64 as well as under emulation, so the one
socket option whose loss would send replies out of whichever interface the
route table prefers is the one no test can assert. The other three are read
back and checked.

The tests that need a real multicast socket therefore skip on `ENOPROTOOPT`
rather than fail on it, and the assertions that read an option back skip
individually, so an option one emulator will not report does not take the other
three with it. The daemon never meets any of this: the Dot runs ARM without an
emulator. What made it worth writing down is how it presented. The socket test
had never once passed in CI, and the single green run that seemed to prove
otherwise predated the file.

**Two frames are indexed right after they are measured**, and a peer with no key
reaches the first. An empty second handshake frame would be read at `[0]` for
its preamble, and a decrypted message shorter than four bytes sliced for its
inner header. Neither is a crash the daemon survives usefully: the supervisor
brings it back five seconds later with the button ungrabbed each time, so one
peer repeating one empty frame is a reboot loop. Each guard has a test that
panics without it, rather than one that reads the code back.

**There is still no peer allowlist**, because ESPHome has no such concept. The
key is the whole of the access control, and it guards what a peer can reach
rather than whether it gets in: anything that can route to the Dot on `wlan0`
may open a connection and hold one of the eight slots until the handshake wait
expires, and eight of those keep Home Assistant off the device for as long as
somebody cares to. What is behind the slot needs the key.

**`-name` is required and has to be unique.** README.md carries the rule a user
needs; the rest of this is why there is no way around it.

The daemon checks it as well as `install.sh`, because the binary can be run by
hand, and the rules are ESPHome's own: the name is the device's identity in Home
Assistant and the prefix of every entity id it creates.

It has **no default**. A default is the same name on every Dot, and Home
Assistant prefixes every entity id with it, so two Dots would collide there.
Adding the second stops in a conflict menu asking whether to migrate or
overwrite the first, which is a question nobody can answer from what it shows.

A MAC-derived default like `echodot-00532a` is no better: it invents a
placeholder identity ESPHome deliberately does not have, since ESPHome fails the
compile without a `name:`. `friendly_name` is sent equal to `name` rather than
dropped, because Home Assistant otherwise logs an INFO line on every connect, and
because it collapses the discovery card to one string.

Deriving `name` from a display name fails the other way. They have opposite
lifecycles: the identity is pinned at first connect, the label is cosmetic and
freely edited, so deriving one from the other turns a cosmetic edit into a
breaking change. Slugifying is lossy besides: "Alexa's Dot" and "Alexas Dot"
both land on `alexas-dot`.
