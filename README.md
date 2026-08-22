# overdub

Take over the **action button** on a rooted Echo Dot (2nd Generation), while
stock Alexa keeps running. A press plays a chime through Alexa's own player, so
she ducks and mixes it the way she does her own speech.

## Scope

This runs on a device you own and have already rooted. Nothing here is supported
by anyone, and a FireOS update can invalidate any of it. Rooting voided Amazon's
warranty, and the flashing that gets you there can brick the Dot; both are
behind you before anything here runs.

Taking the action button takes it from Alexa. Stopping a timer or an alarm with
it, press-to-talk, and holding it to enter setup mode all stop working while the
daemon runs. Mute and the volume keys are untouched.

## How this differs from EchoMuse, echolocal and EchoGo

The other projects on this hardware all replace Alexa. This one does not.

[**EchoMuse**](https://github.com/wilbowes/EchoMuse) gives the Dot "a second
life as a fully local, open-source voice assistant and media player for Home
Assistant", replacing the Alexa firmware with a Go server. It is also the
practical route to a rooted Dot, because its docs cover the amonet-biscuit
unlock.

[**echolocal**](https://github.com/ygelfand/echolocal) is the closest neighbour:
the same Dot, also Go, describing itself as "a pure-Go replacement for Amazon's
services", with local wake word detection, LED ring control, a media player and
a Bluetooth proxy.

[**EchoGo**](https://github.com/Binozo/EchoGo) sits lowest: "A Go SDK for your
Echo Dot 2. Gen", giving programmatic control of the LEDs, microphone, speaker
and buttons. If you want to write the device's software yourself, that is the
toolkit.

**overdub keeps stock Alexa running and adds to her.** The action button is the
only thing taken, and if the daemon dies it goes straight back to her.

For a local voice satellite with Amazon out of the picture, use one of those. To
keep the Echo you have -- her voice, her music, her timers, the mute button --
and gain a button of your own that can also talk back, use this one.

## Requirements

* an Echo Dot (2nd Generation) you have rooted, with Magisk. Model **RS03QR**,
  codename biscuit, FireOS 5.5.5.4. The model number is printed on the
  underside, and everything here was measured on that one
* **Magisk 17.3**, or another that keeps `service.d` inside `magisk.img` at
  `/sbin/.core/img/.core/service.d`. That is the only path `install.sh` writes
  the boot script to, and it fails there rather than guessing. A Magisk that
  uses `/data/adb/service.d` needs that path changed first
* Go 1.25 or later, and `adb`, on a development machine

The buttons on this hardware:

| Node | Name | Keycodes |
|---|---|---|
| `/dev/input/event1` | `mtk-kpd` | **138 action ("dot")**, 113 mute |
| `/dev/input/event2` | `keys` (gpio-keys) | 115 volume up, 114 volume down |

The daemon opens `event1` and takes keycode 138 from it, re-emitting the rest.
`event2` is listed for orientation only: nothing here opens it. To check them on
another device, FireOS already ships the tools:

```sh
adb shell su -c 'cat /proc/bus/input/devices'   # names, handlers, key bitmaps
adb shell su -c getevent                        # events, without grabbing
```

## Coming from EchoMuse

EchoMuse's debloat step suppresses the Alexa stack this runs beside, so a Dot
rooted through it needs that undone first. Nothing was uninstalled, so nothing
needs reinstalling:

```sh
deploy/restore-amazon.sh   # then reboot
```

Everything comes back except `com.amazon.device.software.ota`, left hidden on
purpose: an OTA rewrites `boot.img`, removing Magisk and taking root and overdub
with it. EchoMuse's own payload is moved to `/data/local/echomuse-disabled/`
rather than deleted, except the `service.d` debloat hook, which is removed: it
is what suppresses the stack again on every boot.

Skip this if the Dot never had EchoMuse. It restores every package that is
currently hidden or disabled, not just EchoMuse's, so running it on a Dot where
you have suppressed things yourself will undo that too.

## Build

```sh
./build.sh
```

`GOARCH=arm` is not optional, and `build.sh` pins it. A build for any other word
size fails to compile rather than producing a daemon that misreads every input
event: `internal/evdev` asserts the 32-bit `timeval` this device has.

## Install

```sh
deploy/install.sh
```

More than one Dot on `adb` means telling it which. `install.sh` uses plain
`adb`, so `ANDROID_SERIAL` picks the target:

```sh
adb devices                                   # serials
ANDROID_SERIAL=<serial> deploy/install.sh
```

| File | Where |
|---|---|
| `overdub` | `/data/local/bin/` |
| `overdub.sh` | Magisk `service.d`, inside `magisk.img` on Magisk 17.3 |

Reboot to start the daemon. It is supervised, and failure is fail-open: if it
dies the grab is released and the action button goes back to Alexa.

## Usage

The boot script runs the daemon. To run it by hand -- as root, and by full path,
because `/data/local/bin` is on nobody's `PATH`:

```sh
adb shell 'su -c "/data/local/bin/overdub"'
```

There are no flags. Everything is fixed in the binary, because none of it has a
second sensible value on this hardware: `event1` and keycode 138 for the action
button, and `mtk-kpd` for the passthrough clone, which Android needs in order to
apply the same keylayout so mute keeps working.

## Uninstall

```sh
deploy/uninstall.sh
```

The order matters if you interrupt it: the boot script goes first, so a reboot
part way through leaves a Dot with nothing running rather than a supervisor
respawning a half-deleted install. The daemon gets `SIGTERM`, so it releases the
grab and destroys its uinput clone itself. The kernel would do both anyway when
the descriptors close.

Everything goes: the boot script, the binary, and the log the boot script
writes. `/data/local/bin` goes with them if nothing else is left in it.

Amazon's stack is untouched, because installing never touched it.

## Troubleshooting

```sh
adb shell 'su -c "cat /data/local/tmp/overdub.log"'      # truncated per boot
adb shell 'su -c "logcat -d -v brief -s tts-Server tts-Playback"'   # playback
```

| Symptom | Look at |
|---|---|
| mute stopped working | the clone's name. Android picks a keylayout by device name, so it must be `mtk-kpd` |
| every keycode looks wrong | the build. `GOARCH=arm` is required |
| nothing is spoken | the daemon log. `Error: Not found; no service started.` means the Alexa stack is still suppressed: run `deploy/restore-amazon.sh` and reboot |

## How it works

`docs/` is the engineering record: what was measured on the hardware, and what
each decision is defending against.

* [Hardware](docs/hardware.md) -- the input nodes and keycodes, and how to test
  against a live Dot
* [Hard constraints](docs/constraints.md) -- what cannot change, and why
* [Architecture](docs/architecture.md) -- the subsystems, one at a time
* [Things that fail silently](docs/pitfalls.md) -- the failures that report
  success
* [Deployment](docs/deployment.md) -- installing and removing it

## Licence

BSD 2-Clause; [LICENSE.txt](LICENSE.txt) carries the terms.

`internal/alexa/chime.mp3` is original to this repository: two sine tones,
concatenated and faded, so the licence covers it as it covers the code. It is
synthesised rather than sampled, and this is what made it, byte for byte:

```sh
ffmpeg -y -loglevel error \
  -f lavfi -i "sine=frequency=880:duration=0.18,volume=0.5" \
  -f lavfi -i "sine=frequency=1320:duration=0.22,volume=0.5" \
  -filter_complex "[0][1]concat=n=2:v=0:a=1,afade=t=out:st=0.30:d=0.10,aresample=24000" \
  -c:a libmp3lame -b:a 48k -ar 24000 -ac 1 -write_xing 0 -id3v2_version 0 chime.mp3
```

This licence covers the code in this repository and nothing else. This
repository contains no Amazon code. The Amazon names it does carry identify
things already installed on the device, so that this software can interoperate
with them: functional names, not copied implementation.

overdub is not affiliated with, endorsed by, or supported by Amazon.
