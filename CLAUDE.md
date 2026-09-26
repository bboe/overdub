# CLAUDE.md

A single Go binary that takes over the action button on a rooted **Echo Dot (2nd
Generation)** (model RS03QR, codename biscuit, FireOS 5.5.5.4) and presents the
Dot to Home Assistant as an ESPHome device. It runs on the Dot itself, under
Magisk, alongside stock Alexa.

`README.md` says how to use it. This file and `docs/` say why, and most of it
was measured on hardware rather than reasoned out.

## Commands

```sh
./build.sh                  # the only supported build; needs an NDK
deploy/install.sh <name>    # build, push, install over adb
deploy/uninstall.sh         # remove it again, and give the button back
gofmt -l .                  # expected to be silent
GOOS=linux GOARCH=arm GOARM=7 go vet ./...    # the target, not the runner

# the suite, as 32-bit ARM: under qemu-user-static, or under Docker
GOOS=linux GOARCH=arm GOARM=7 go test -exec qemu-arm-static ./...
docker run --rm --platform linux/arm/v7 -v "$PWD":/src \
  -v "$HOME/go/pkg/mod":/go/pkg/mod -w /src golang:1.26 go test -count=2 ./...

# -race has no arm build, so these run natively
go test -race ./internal/alexa/ ./internal/audio/ ./internal/esphome/ \
  ./internal/device/ ./internal/mdns/ ./internal/sendspin/ \
  ./internal/untrustedlog/

git ls-files -z '*.sh' | xargs -0 shellcheck -S style   # as CI runs it
deploy/check-identifiers.sh   # Amazon identifiers still look like ones

# against the reference server; needs uv, and CI runs it in a job of its own
SENDSPIN_INTEROP=1 go test -count=1 -run Interop ./internal/sendspin/
```

- Tests cover what is hand-rolled and checkable in isolation: the wire formats,
  the event layout, the parsers, and the shape of the chime.
- They run as 32-bit ARM, because the tree compiles for nothing else.
- Anything touching the hardware or Amazon's stack is verified on the device.

## Layout

- `main.go` and `serve.go` hold the flag, the constants and the wiring.
- Each package under `internal/` opens with a doc block saying what it owns:
  `go doc ./internal/<name>` is the map.

## Comments

- The code carries no explanatory comments. The why goes in `docs/`, on the
  page that already covers the area; how to use it goes in `README.md`.
- Do not add prose comments back, even when a change makes one tempting.
- Four things stay, because the code cannot recover them:
  - one doc block per package: `// Package button ...`,
    `// Command overdub ...`
  - Go and cgo directives: `//go:build`, and `chime_android.go`'s `#cgo` and
    `#include` preamble. They are compiler input, and deleting one is silent.
  - a label on a bare number: a protobuf field name
    (`msg.str(13, s.name) // friendly_name`), an ioctl derivation
    (`// _IOW('E', 0x90, int)`), the `uinput_user_dev` layout, a byte in a
    test fixture (`// code = 138`)
  - each shell script's opening usage paragraph
- Everything else goes: rationale, measurements, and notes saying a line is
  deliberate.

## The rest

`docs/` carries the measurements and the traps, mostly silent failures. The two
pages imported below apply to everything. Read the page for a subsystem
**before** you touch it:

- `docs/button.md`: what a key press does -- the grab, the clone, the modes,
  the gestures
- `docs/api.md`: the volume keys and the grab the mute holds them with; any
  entity; the polls, the deadlines, the key
- `docs/mdns.md`: the mDNS responder, or what a service advertises
- `docs/device.md`: network adb, the microphone mute, a persisted setting
- `docs/audio.md`: making a sound, or playing one
- `docs/deployment.md`: `install.sh`, `uninstall.sh`, the boot script
- `docs/rooting.md`: `dot_root.py`, from stock Fire OS 6 to rooted Fire OS 5
- `docs/command.md`: the Alexa command path, the credential, MapDump
- `docs/hardware.md`: anything run against the real device
- `docs/sendspin.md`: the Sendspin client, its WebSocket, its handshake

@docs/constraints.md
@docs/pitfalls.md
