# CLAUDE.md

A single Go binary that takes over the action button on a rooted **Echo Dot (2nd
Generation)** (model RS03QR, codename biscuit, FireOS 5.5.5.4) and presents the
Dot to Home Assistant as an ESPHome device. It runs on the Dot itself, under
Magisk, alongside stock Alexa.

`README.md` says how to use it. This file and `docs/` say why, and most of it
was measured on hardware rather than reasoned out.

## Commands

```sh
./build.sh                       # the only supported build; needs an NDK
deploy/install.sh <name>         # build, push, install over adb
deploy/uninstall.sh              # remove it again, and give the button back
gofmt -l .                       # expected to be silent
GOOS=linux GOARCH=arm GOARM=7 go vet ./...        # the target, not the runner
GOOS=linux GOARCH=arm GOARM=7 go test -exec qemu-arm-static ./...   # needs qemu-user-static
go test -race ./internal/esphome/ ./internal/device/ ./internal/mdns/ \
        ./internal/sendspin/ ./internal/untrustedlog/   # -race has no arm build
shellcheck -S style build.sh deploy/*.sh          # CI runs it too
SENDSPIN_INTEROP=1 go test -run Interop ./internal/sendspin/  # vs the reference
        server; needs uv, and CI runs it in a job of its own
```

Tests cover what is hand-rolled and checkable in isolation: the wire formats,
the event layout, the parsers, and the shape of the chime.
They run as 32-bit ARM under qemu, because the tree compiles for nothing else.
Anything touching the hardware or Amazon's stack is verified on the device
instead.

## Layout

```
main.go, serve.go  the flag, the constants, and the wiring
internal/alexa     her synthesizer: the intent that hands it a clip, and the
                   log tail that says whether the clip played; and her cloud:
                   the credential MapDump recovers, and the text command it buys
internal/audio     the chime: the tones it is made of, and the OpenSL ES
                   player that sounds them
internal/button    the exclusive grab, the clone, the read loop, whether the
                   action key is ours or Alexa's, and the volume keys pressed
                   on request
internal/device    the Dot itself: its network, the firewall rule, network
                   adb, and the microphone mute
internal/esphome   the ESPHome API, its protobuf, the Noise transport, and
                   the advert Home Assistant finds the Dot by
internal/evdev     evdev and uinput primitives
internal/mdns      the mDNS responder, answering for every service the Dot
                   offers; it knows nothing about any of them
internal/sendspin  the Sendspin client: the WebSocket a server arrives over,
                   the identity and pairing token, the Noise KKpsk2 transport,
                   what it declares and what a server may activate
internal/untrustedlog
                   what a peer may spend making this daemon write to /data
```

## Comments

The code carries no explanatory comments. Why it is the way it is belongs in
this file and in `docs/`; how to use it belongs in `README.md`. Do not add prose
comments back, and do not add one a change makes tempting -- write the paragraph
into the `docs/` page that already covers the area.

Four things may stay, because the code cannot recover them on its own:

- one doc block per package: `// Package button ...`, `// Command overdub ...`
- Go and cgo directives: `//go:build`, and `chime_android.go`'s `#cgo` and
  `#include` preamble. These are compiler input rather than commentary, and
  deleting one is silent -- a stripped `//go:embed` once left the chime a nil
  slice with `go build`, `go vet` and `go test -c` all clean, and only running
  the test said so.
- a label on a bare number: a protobuf field name
  (`msg.str(13, s.name) // friendly_name`), an ioctl derivation
  (`// _IOW('E', 0x90, int)`), the `uinput_user_dev` layout, a byte in a test
  fixture (`// code = 138`)
- each shell script's opening usage paragraph

Everything else goes: rationale, measurements, and notes saying a line is
deliberate rather than an oversight. All of those read better as prose in
`docs/`, where one page holds the whole argument rather than a sentence of it
per call site.

## The rest

`docs/` carries the measurements and the traps. Two pages apply whatever you are
doing and are imported below. The others are reference for one subsystem each:
read the page **before** you touch the thing, because what they carry is mostly
silent failures, and a trap you meet afterwards has already cost you the
evening.

| before you | read |
|---|---|
| change what a key press does -- the grab, the clone, the modes, the gestures | `docs/button.md` |
| add or change an entity, or touch the polls, the deadlines, or the key | `docs/api.md` |
| touch the mDNS responder, or what a service advertises | `docs/mdns.md` |
| turn on network adb, or touch the microphone mute | `docs/device.md` |
| make a sound, or play one | `docs/audio.md` |
| change `install.sh`, `uninstall.sh`, or the boot script | `docs/deployment.md` |
| touch the Alexa command path, the credential, or MapDump | `docs/command.md` |
| run anything against the real device | `docs/hardware.md` |
| touch the Sendspin client, its WebSocket, or its handshake | `docs/sendspin.md` |

@docs/constraints.md
@docs/pitfalls.md
