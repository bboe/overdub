# Things that fail silently

Every entry here reports success and does the wrong thing. Read the section
covering whatever you are about to touch.

## Install and build

- Nothing executes the shell scripts: gofmt, vet, the tests and shellcheck
  only read them. CI asserts every tracked `.sh` is `100755`.
- `install.sh` asks two questions separately: does `build.sh` **exist**, and is
  it executable. Merged, a source tree with stripped mode bits
  (`core.fileMode=false`, an unpacked archive) takes the tarball branch,
  installs a stale `build/overdub`, and prints `binary verified`.
- `cp` onto the running binary fails with ETXTBSY, but toolbox `cp` exits 0,
  `adb shell` exits 0 whatever happened remotely, and `set -e` catches nothing.
  The binary goes in by rename, and its md5 is read back.
- Compare by hash, never by size. Go's VCS stamp (commit, timestamp, dirty flag)
  is fixed-length, so different source gives the same size. `build.sh` pins
  `-buildvcs=false`.
- One tree gives one set of bytes, from any directory, with or without `.git`,
  dirty or clean. `OVERDUB_VERSION` is empty unless the release job sets it.
- A release binary and a local build of the same tag differ: the runner's NDK
  is not yours. The published hash identifies the release.
- The `-L` that `build.sh` hands cgo for its `libpthread` stubs must stay
  relative. An absolute path enters cgo's action hash, so two directories give
  two binaries. The `cd` at the top makes the relative path resolve.
- `zip` records each entry's mtime in its headers. `deploy/mapdump/build.sh`
  pins it to zip's 1980 floor and passes `-X`.
- `TZ=UTC` must cover **both** the `touch` and the `zip`. A zip entry carries a
  DOS stamp in local time, so a mismatch writes the offset into the headers.
- A reproducible binary cannot say what it was built from. `git checkout`
  carries modified files across a branch change, so the build is not the
  branch, and every check downstream passes on the wrong binary.
- The boot script is the only reason anything runs. `install.sh` copies it to
  Magisk 17.3's `/sbin/.core/img/.core/service.d/`. Where that directory does
  not exist, the `cp` fails, the `rm` beside it still runs, and the Dot does
  not start the daemon after its next reboot, with no log. The script's md5 is
  read back.
- The restart check calls `adb` through a function. A pipeline reports only its
  last command, so inline, a pulled cable reads as an empty pid, which is also
  what "nothing supervised the daemon" prints.

## Paths and uids

- `/system/bin/pm` has no shebang, so `execve` answers ENOEXEC. A shell runs it
  anyway, so it works by hand over `adb shell`. `Installed()` would read the
  failure as a missing package, so `pm` runs as `/system/bin/sh
  /system/bin/pm`. `am` has a shebang and runs directly.
- `/data/local/bin` must be `0700`, and `mkdir -p` keeps whatever mode an
  earlier install left. `install.sh` chmods it and reads the mode back.
- MAP's uid, 32051, cannot traverse `0700`. `app_process` does not report a
  permission error: it prints `ClassNotFoundException: MapDump` on
  `DexPathList[[]]` and then `Aborted`, which reads like a bad jar. So the jar
  lives in `/data/local/map`, owned by 32051, mode `0755`, and `install.sh`
  checks that uid 32051 can read it.
- Uid 32051 cannot write to `/data/local/tmp` (`root:shell`). The failure is
  `EACCES` from inside dalvik, well after start. Nothing the jar does at
  runtime touches that directory.
- `adb push` does not carry the local mode, and `/data/local/tmp` is `0771`, so
  a `0600` key lands `0666` and any uid can reach it by name. `install.sh`
  stages secrets through a `0700` directory of its own, then removes it. The
  binary and the boot script are not secret and use the shared directory.
- There is no `/etc/resolv.conf`, so every Go lookup fails against `::1` with
  "connection refused". `internal/alexa` reads `net.dns1` and `net.dns2` and
  builds its own resolver.

## The firewall

- FireOS runs iptables with `INPUT policy DROP` and a port allowlist without
  `tcp/6053`. Home Assistant then times out adding the device, with **nothing in
  the daemon log**: the SYN never reaches userspace.
  `internal/device/firewall.go` opens the port.
- `iptables -L INPUT -n -v | grep 6053` shows a packet counter, which tells
  "the device dropped it" from "the network did".
- `AllowTCP` checks and then appends. The API's re-assert, network adb and the
  Sendspin switch all call it, so two concurrent calls could both append, and
  one `-D` would leave an ACCEPT behind. `chainMu` covers both mutations.
- `DenyTCP` deletes until iptables reports no match, at most 16 times. An
  iptables that answered 0 for a no-op delete would otherwise spin holding
  `chainMu`, and `tcp/6053` would not be re-asserted.
- `tcp/8928`'s rule goes in **after** the listener binds. A rule for a port
  nothing listens on is re-asserted every 30 seconds for the rest of the boot.
  The API needs no such order: a failed bind there exits the daemon.
- `-w` here takes no seconds argument and waits on the xtables lock
  indefinitely, and netd holds that lock constantly. So every call has a
  10-second deadline, and a failed re-assert is logged once until it succeeds.

## Startup races the network

A warm restart hides all of this.

| Uptime | What is true |
|---|---|
| 0-15 seconds | `wlan0` does not exist |
| ~26 seconds | `sys.boot_completed`, boot script runs, daemon starts |
| later | netd rebuilds the INPUT chain, discarding our rule |

- Setup that depends on the network must wait or re-assert, never run once. On
  a cold boot the daemon waited 15 seconds for `wlan0`, and its rule was gone
  by 49 seconds and back by 64.
- The 30-second re-assert leaves a gap. Across one reboot the rule went in at
  25 seconds, was gone by 30, and was back at 57, and the mDNS announcement
  went out at 36. So the rule is asserted once more immediately before
  announcing; docs/sendspin.md has the order.
- A repeated announcement with the same data is a refresh, not news, so the
  responder cannot fix that for a peer that already holds the records. The
  client's retry must. Home Assistant reconnects on a timer; Music Assistant
  gives up permanently after about 8.5 minutes.

## What a peer can spend

- A peer causes every line the API logs. `%q` renders a frame of `\xff` at 4
  times its size on 1 line: one `HelloRequest` wrote 131,207 bytes.
- The boot script truncates the log only at boot and every 20th restart, and a
  peer never makes the daemon exit.
- `internal/untrustedlog` holds the limits: peer strings cut to 64 bytes before
  quoting, every line cut at 512 bytes, 20 peer lines a minute, and 5,000 for
  the run. `Cut` is the call site's job; peer bytes inside an error formatted
  with `%v` are bounded only by the 512-byte line cut.
- Counters are per `Log`, so each subsystem has its own budget and the disk
  sees the sum.
- The rate alone is not enough: 21 lines a minute at the measured 311-byte
  worst case is 9 MB a day. After the 5,000th line one line says so, and then
  nothing a peer does is logged until restart, not even a dropped count.
- An empty pre-shared key is no key. `flynn/noise` reads an empty
  `PresharedKey` as no psk modifier, so `NNpsk0` becomes `NN` and every peer
  completes the handshake. The first handshake message is 48 bytes with a
  32-byte key and 32 bytes with none. `noiseAccept` checks the key length;
  `TestAServerWithNoKeyRefusesEveryone` fails without that check.
- There is no peer allowlist: ESPHome has none. Any host that can route to the
  Dot can hold one of the 8 slots until the handshake wait expires, and 8 such
  hosts keep Home Assistant out. Past the slot, everything needs the key.
- Two frames are indexed right after they are measured, and a peer with no key
  reaches the first: an empty second handshake frame read at `[0]`, and a
  decrypted message under 4 bytes sliced for its header. A panic there restarts
  the daemon 5 seconds later with the button ungrabbed, so a repeated empty
  frame is a restart loop. Each guard has a test that panics without it.

## mDNS and adb

- udp/5353 needs no rule: the stock INPUT chain accepts it, because Alexa runs
  her own mDNS. Her sockets are why the responder sets `SO_REUSEADDR` and
  `SO_REUSEPORT` before binding; without both the bind fails.
- `adb` merges the device's stderr into its stdout, so any read-back can carry
  a line of noise. A linker warning from `su` prepended to a key file makes it
  decode to nothing. No deploy script compares the whole stream to a literal:
  each matches the shape it expects, and a read that decides something ends
  with a word of its own, so silence and "no" differ.

## Tests and emulation

- qemu-user's socket options depend on the build. Under `qemu-arm-static` on
  x86-64 (CI), *setting* `IP_MULTICAST_IF` fails with `protocol not available`.
  Under Docker's `linux/arm/v7` and `linux/arm64` it succeeds.
  `IP_ADD_MEMBERSHIP`, one line earlier, succeeds everywhere.
- Linux refuses `getsockopt(IP_MULTICAST_IF)` with `ENOPROTOOPT`, on native
  arm64 too. So no test can assert the one option whose loss sends replies out
  of the wrong interface. `SO_REUSEADDR`, `SO_REUSEPORT` and group membership
  are read back and checked.
- Tests that need a real multicast socket skip on `ENOPROTOOPT`, and each
  read-back assertion skips on its own. The Dot runs ARM natively and never
  meets this.

## evdev and syscalls

- `os.File.Fd()` calls `pfd.SetBlocking()`. After that, `Close()` cannot
  interrupt a `Read` blocked on the file: the reader holds a goroutine, a
  thread and the fd, and later acts on an event meant for the next owner.
  Through `SyscallConn().Control`, the blocked read returns
  `file already closed`.
- So every ioctl on an evdev descriptor in `internal/evdev` goes through
  `Control`. This is a package rule, because one call through `Fd()`
  reintroduces the hang. `/dev/uinput` uses `Fd()` safely: nothing reads it.
- A pointer converted to `uintptr` for a syscall must be converted in that
  syscall's own argument list, in the same *frame*. Elsewhere the compiler
  stops tracking it, and a stack move leaves the integer naming freed memory.
  `ioctlPtr` takes an `unsafe.Pointer` and converts it in the call. `go vet`'s
  `unsafeptr` check flags only the reverse conversion.

## The device name

- `-name` is required, has no default, and must be unique. The daemon checks it
  as well as `install.sh`, because the binary can be run by hand.
- A fixed default would give every Dot the same name. Home Assistant prefixes
  every entity id with it, so adding a second Dot stops in a conflict menu.
- A MAC-derived default invents an identity ESPHome deliberately lacks: ESPHome
  fails the compile without a `name:`.
- `friendly_name` is sent equal to `name`. Without it Home Assistant logs an
  INFO line on every connect, and the discovery card shows one string.
- `name` is not derived from a display name. The identity is pinned at first
  connect and the label is cosmetic, so a derived name turns a cosmetic edit
  into a breaking change. Slugifying is also lossy: "Alexa's Dot" and "Alexas
  Dot" both become `alexas-dot`.
