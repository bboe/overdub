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

**Nothing locks the API port yet.** There is no peer allowlist, because ESPHome
has no such concept, so tcp/6053 is open to the subnet exactly like a keyless
ESPHome node, and every entity behind it is reachable by anything that can route
to the Dot on `wlan0`.

**`-name` is an identity, not a label, and it is required.** README.md carries
the rule a user needs, which is not to change it after adding the device. The
rest of this is why there is no way around that.

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
