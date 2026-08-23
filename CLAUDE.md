# CLAUDE.md

A single Go binary that takes over the action button on a rooted **Echo Dot (2nd
Generation)** (model RS03QR, codename biscuit, FireOS 5.5.5.4) and presents the
Dot to Home Assistant as an ESPHome device. It runs on the Dot itself, under
Magisk, alongside stock Alexa.

`README.md` says how to use it. This file and `docs/` say why, and most of it
was measured on hardware rather than reasoned out.

## Commands

```sh
./build.sh                       # the only supported build
deploy/install.sh <name>         # build, push, install over adb
deploy/uninstall.sh              # remove it again, and give the button back
gofmt -l .                       # expected to be silent
GOOS=linux GOARCH=arm GOARM=7 go vet ./...        # the target, not the runner
GOOS=linux GOARCH=arm GOARM=7 go test -exec qemu-arm-static ./...   # needs qemu-user-static
go test -race ./internal/esphome/ ./internal/device/   # -race has no arm build
shellcheck -S style build.sh deploy/*.sh          # CI runs it too
```

Tests cover what is hand-rolled and checkable in isolation: the wire formats,
the event layout, the parsers, and the one encoding Alexa's demuxer accepts.
They run as 32-bit ARM under qemu, because the tree compiles for nothing else.
Anything touching the hardware or Amazon's stack is verified on the device
instead.

## Layout

```
main.go, serve.go  the flag, the constants, and the wiring
internal/alexa     speech through Alexa: the intent, the clip, and the server
                   that serves it to her
internal/button    the exclusive grab, the clone, and the read loop
internal/device    the Dot itself: its network, and the firewall rule
internal/esphome   the ESPHome API, its protobuf, the Noise transport, and
                   the mDNS responder Home Assistant finds the Dot by
internal/evdev     evdev and uinput primitives
```

## The rest

`docs/` carries the measurements and the traps. It is imported below, so all of
it is in context here without being opened:

@docs/hardware.md
@docs/constraints.md
@docs/architecture.md
@docs/pitfalls.md
@docs/deployment.md
