# overdub

Take over the **action button** on a rooted Echo Dot (2nd Generation) and
present the Dot to Home Assistant as an **ESPHome device**, while stock Alexa
keeps running.

Home Assistant adopts it with its own first-party ESPHome integration: no custom
component, no MQTT, and no Home Assistant credential on the Dot. The Dot reports
a press and a hold of the action button to Home Assistant, reports what it can
read about itself over the same connection, takes the button back from Home
Assistant and hands it over again, and chimes on the device itself for every
press it takes.

## Scope

This runs on a device you own and have already rooted. Nothing here is supported
by anyone, and a FireOS update can invalidate any of it. Rooting voided Amazon's
warranty, and the flashing that gets you there can brick the Dot; both are
behind you before anything here runs.

Taking the action button takes it from Alexa. Stopping a timer or an alarm with
it, press-to-talk, and holding it to enter setup mode all stop working while the
daemon holds it. The `Button capture` switch gives it back without stopping
anything else, so the Dot keeps reporting and keeps its entities while Alexa
has her button. Mute and the volume keys are untouched throughout.

## How this differs from EchoMuse, echolocal and EchoGo

The other projects on this hardware all replace Alexa. This one does not.

[**EchoMuse**](https://github.com/wilbowes/EchoMuse) gives the Dot "a second
life as a fully local, open-source voice assistant and media player for Home
Assistant", replacing the Alexa firmware with a Go server. It is also the
practical route to a rooted Dot, because its docs cover the amonet-biscuit
unlock.

[**echolocal**](https://github.com/ygelfand/echolocal) is the closest neighbour:
the same Dot, also Go, also speaking the ESPHome native API, with local wake word
detection, LED ring control, a media player and a Bluetooth proxy.

[**EchoGo**](https://github.com/Binozo/EchoGo) sits lowest: "A Go SDK for your
Echo Dot 2. Gen", giving programmatic control of the LEDs, microphone, speaker
and buttons. If you want to write the device's software yourself, that is the
toolkit.

**overdub keeps stock Alexa running and adds to her.** The action button is the
only thing taken, and if the daemon dies it goes straight back to her.

For a local voice satellite with Amazon out of the picture, use one of those. To
keep the Echo you have, with her voice, her music, her timers and the mute
button, and add a Home Assistant button and a handful of entities, use this
one.

## Requirements

* an Echo Dot (2nd Generation) you have rooted, with Magisk. Model **RS03QR**,
  codename biscuit, FireOS 5.5.5.4. The model number is printed on the
  underside, and everything here was measured on that one
* **Magisk 17.3**, or another that keeps `service.d` inside `magisk.img` at
  `/sbin/.core/img/.core/service.d`. That is the only path `install.sh` writes
  the boot script to, and it fails there rather than guessing. A Magisk that
  uses `/data/adb/service.d` needs that path changed first
* Go 1.25 or later, and `adb`, on a development machine
* An **Android NDK**, for the chime: it is played through OpenSL ES, so the
  daemon is cgo and `build.sh` will not run without one. `brew install --cask
  android-ndk` on macOS, or a release from
  [developer.android.com/ndk](https://developer.android.com/ndk) elsewhere, with
  `ANDROID_NDK_HOME` pointing at it
* Home Assistant on the same subnet as the Dot

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

`GOOS=android` is not optional either, and is the less obvious of the two: the
chime is cgo against OpenSL ES, and Go's linux runtime hangs before `main`
against Bionic rather than failing. `build.sh` sets it, finds the NDK compiler,
and makes the empty `libpthread` stub Bionic needs and cgo asks for. Set
`ANDROID_NDK_HOME` if the NDK is not where it looks; it says so if it cannot
find one.

## Install

```sh
deploy/install.sh kitchen                          # binary, boot script, key
```

More than one Dot on `adb` means telling it which. `install.sh` uses plain
`adb`, so `ANDROID_SERIAL` picks the target:

```sh
adb devices                                   # serials
ANDROID_SERIAL=<serial> deploy/install.sh kitchen
```

| File | Where |
|---|---|
| `overdub` | `/data/local/bin/` |
| `overdub.sh` | Magisk `service.d`, inside `magisk.img` on Magisk 17.3 |
| `.overdub-noise-key` | `/data/local/bin/`, mode 600, generated if absent |

**The name is required, and must be unique on your network.** Every entity id
Home Assistant creates is prefixed with it, so a duplicate collides there, and
adding a second Dot under a name already in use stops the flow with a conflict
menu rather than completing. Lowercase letters, digits, `-` and `_`, at most 63
characters, and not starting or ending with `-`. Those are ESPHome's own naming
rules, and `install.sh` rejects anything else.

Changing it later is allowed. Home Assistant identifies the device by its MAC
rather than its name, so a rename is accepted on the next connection and the
stored name is updated in place; the entity ids keep the prefix they were
created with. The display name is separate, and you set it in Home Assistant
afterwards.

Reboot to start the daemon. It is supervised, and failure is fail-open: if it
dies the grab is released and the action button goes back to Alexa.

## Usage

The boot script runs the daemon. To run it by hand, as root and by full path,
because `/data/local/bin` is on nobody's `PATH`:

```sh
adb shell 'su -c "/data/local/bin/overdub -name kitchen"'
```

| Flag | | |
|---|---|---|
| `-name` | **required** | unique device name Home Assistant identifies the Dot by |

Everything else is fixed in the binary, none of it having a second sensible
value here: `event1` and keycode 138, `mtk-kpd` for the clone, `wlan0` and
tcp/6053.

## Uninstall

```sh
deploy/uninstall.sh
```

The order matters if you interrupt it: the boot script goes first, so a reboot
part way through leaves a Dot with nothing running rather than a supervisor
respawning a half-deleted install. The daemon gets `SIGTERM` rather than being
killed outright, so it gives the button back and destroys its uinput clones on
the way out.

Everything goes: the boot script, the binary, the API key, and the log the boot
script writes. `/data/local/bin` goes with them if nothing else is left in it.

The tcp/6053 rule the daemon opened goes too, once the daemon is confirmed
gone, so no reboot is needed. An uninstall that reports trouble stops before
that step and leaves the rule in the chain.

Amazon's stack is untouched, because installing never touched it. Home Assistant
will show the device as unavailable; delete it there when you are done.

## Home Assistant

The Dot announces itself, so Home Assistant discovers it: it appears under
**Settings -> Devices & Services** as an ESPHome device named after `-name`,
and adding it asks only for the encryption key the installer printed.

If it does not appear, add it by hand at **Add integration -> ESPHome** with the
Dot's address and port `6053`; `adb shell ip -4 addr show wlan0` gives the
address. Discovery is multicast and does not cross subnets, so a Home Assistant
on a different network segment needs the address either way.

> **The key is the whole of the access control.** ESPHome has no peer
> allowlist, so anything that can route to the Dot may open a connection. What
> the key guards is what that connection reaches, not whether it is made: a peer
> without the key learns the device name and holds one of eight slots for ten
> seconds, and eight of them can keep Home Assistant off the Dot for as long as
> they care to. The firewall rule matches the interface rather than a source
> range, so that reach is wider than the local subnet, and a VPN client on
> another one is inside it. SECURITY.md has the measurement.

**A DHCP reservation is still worth setting.** Home Assistant stores the address
it found, and re-finds the device by name after a change, but only while
discovery reaches it.

The device is the server and Home Assistant dials in, the reverse of most
integrations. It needs inbound reach to tcp/6053, which the daemon opens on the
Dot's own firewall.

### What it exposes

| Entity | Kind | Notes |
|---|---|---|
| `event.<name>_action_button` | none | Home Assistant's standard button types: `press_end`, `multi_press_end`, `long_press_start`, `long_press_end`; an event, so it has no state between presses |
| `sensor.<name>_uptime` | diagnostic | seconds since boot |
| `sensor.<name>_wifi_signal` | diagnostic | dBm; a reading that is not a signal is reported as missing rather than as zero |
| `sensor.<name>_volume` | diagnostic | percent of the speaker's own scale, which is 30 steps here; a muted stream reads as zero |
| `sensor.<name>_cpu_temperature` | diagnostic | °C, from the SoC's own thermal zone |
| `sensor.<name>_memory_available` | diagnostic | MiB the kernel says an allocation could get, which is not the same as free |
| `sensor.<name>_jack_volume` | diagnostic | percent, for the 3.5mm output rather than the speaker; a muted stream reads as zero here too |
| `binary_sensor.<name>_audio_jack` | diagnostic | whether anything is in the 3.5mm socket |
| `binary_sensor.<name>_speaker_playing` | diagnostic | whether audio is coming out, by either wired route; sound shorter than about a second and a half is not reported, and bluetooth is not seen at all |
| `switch.<name>_button_capture` | config | whether the action button is the daemon's or Alexa's |

Uptime and signal are read once a minute, and again when Home Assistant
subscribes. Both volumes, the jack, the temperature and the memory are read
every two and a half seconds, whether the speaker is playing every half second, and
all of it only while something is subscribed.
Everything is sent only when it changes, so the uptime arrives every minute,
the others when they move, and a quiet short tick costs the reads and no
traffic at all.

That is why a volume you have just turned appears within a few seconds, whether
you turned it with the buttons, from an app, or by asking Alexa.

`volume` is the speaker's own level and `jack_volume` is the socket's. Android
keeps a level per route and switches between them when you plug something in,
so between those two, the one you are hearing is whichever `audio_jack` says.
Both are reported whatever is plugged in, because both are real levels the
device would return to. Mute is not per route, so muting reads as zero on both
at once.

A bluetooth speaker is a third route and is not reported at all. Pair one and
Android tracks its level separately again, so neither of these readings is what
you are hearing and `audio_jack` does not say so.

`button_capture` is the one entity Home Assistant writes to. Turn it off and the
action button is Alexa's again -- press-to-talk is measured, and timers and
setup mode follow the same key path -- while the daemon keeps running and keeps
reporting. Turn it back on and the button
is the daemon's, with no reboot and no reinstall. A press arriving mid-toggle
belongs to whichever side owned the button when it went down, so nothing is ever
half-delivered.

The daemon starts with the button captured, and the switch is not remembered
across a restart: a Dot that reboots comes back holding its button whatever the
switch said before. Home Assistant will restore it if you ask it to, with an
automation on `homeassistant_start` or on the device becoming available.

`audio_jack` is on whenever the socket is occupied and nothing more. The
detection is electrical and stops at the contacts: a bare cable with nothing on
the far end reads the same as headphones, and unplugging the far end of a
connected cable is invisible to it.

`action_button` is the button itself. It reports Home Assistant's standard
button gestures rather than names of its own, so an automation written for any
other button works here.

Quick presses are collected into a run, which fires `multi_press_end` once with
its count about a third of a second after you stop pressing. A single press
fires `press_end`, and waits out that same third of a second first: nothing
knows a press was single until it has.

Holding past six hundred milliseconds, Alexa's own threshold for a long press,
fires `long_press_start` **while you are still holding**, so an automation can
run for as long as the button is down. Letting go fires `long_press_end`. A hold
ends any run in front of it, and that run is reported first.

If the daemon is busy enough to notice the threshold late, it falls back to the
duration the release carries and sends both at once. The hold is still reported;
it is not reported early.

The chime does not wait. It sounds as the button goes down, once per press, so
four presses are four chimes and one `multi_press_end`.

An `EventResponse` carries a type and nothing else, so the numbers arrive beside
it as an `esphome.overdub_pressed` event on Home Assistant's bus. It always
carries `event_type` and `device`; `multi_press_count` comes with
`multi_press_end`, `held_ms` with `long_press_end`. Both are integers, so
`{{ trigger.event.data.multi_press_count == 7 }}` works without a cast. The
blueprint in `ha/` wires the gestures up.

`long_press_start` and `long_press_end` are not a guaranteed pair. Events carry
no state, so if Home Assistant misses the release -- a restart, a reconnect --
whatever the hold started keeps running. Give such an automation its own timeout.

It is an event rather than a sensor, so it has no state to read: an automation
triggers on it, and the dashboard shows no value between presses. A Home
Assistant that was not connected does not learn about a press afterwards.

Only a captured press does any of this. With `button_capture` off the button is
Alexa's, and a press is silent and unreported. The chime is how you know the
daemon still has the button.

`button_capture` is still the only entity Home Assistant writes to. Everything
else reports, the button included, and together they are the connection proved
end to end in both directions.

### Encryption

The API speaks ESPHome's `Noise_NNpsk0_25519_ChaChaPoly_SHA256`, and speaks
nothing else. There is no plaintext mode, because this build does not implement
one, and no peer allowlist, because ESPHome has no such concept: the device is
the server, and the client authenticates with a pre-shared key.

`deploy/install.sh` generates that key when the device has none, the way
ESPHome's own tooling does, and prints it once:

```
Generated an API encryption key. Paste it into Home Assistant's
ESPHome integration. The installer keeps no copy:

    kR2b...
```

Keep it. The installer does not take a key of your own, and re-running it leaves
an existing one alone, so reinstalling does not lock Home Assistant out of a Dot
it was already talking to.

To rotate: delete `/data/local/bin/.overdub-noise-key` on the Dot, install
again, and paste the new key into Home Assistant.

## Troubleshooting

```sh
adb shell 'su -c "cat /data/local/tmp/overdub.log"'      # truncated per boot
adb shell 'su -c "iptables -L INPUT -n -v | grep 6053"'  # packet counter
adb shell 'su -c "logcat -d -v brief -s tts-Server tts-Playback"'   # Alexa on playback
```

| Symptom | Look at |
|---|---|
| you need the Dot's address to add it | `adb shell ip -4 addr show wlan0` |
| Home Assistant times out adding the Dot | the tcp/6053 packet counter. Zero means the traffic never arrived |
| Home Assistant logs `Unexpected device found` | the stored address now answers with a different MAC, so it is a different device: set a DHCP reservation |
| Home Assistant says the key is invalid | the daemon log. `handshake failed` means the key it sent is not the one on the Dot |
| Home Assistant says the device requires encryption | it has no key stored for this Dot; give it the one the installer printed |
| nothing starts, and the log ends `no such file or directory (deploy/install.sh generates one)` | there is no key on the device: rerun `deploy/install.sh <name>` |
| nothing starts, and the log says the key `decodes to N bytes` | the key on the device is corrupt. A reinstall keeps an existing key, so delete it first: `adb shell 'su -c "rm -f /data/local/bin/.overdub-noise-key"'` |
| nothing starts, and the log says `NAME is unset` | the boot script was installed by hand; rerun `deploy/install.sh <name>` |
| mute stopped working | the clone's name. Android picks a keylayout by device name, so it must be `mtk-kpd` |
| every keycode looks wrong | the build. `GOARCH=arm` is required |
| the button does not chime | the daemon log. `presses will be silent` means the audio player did not start. It needs nothing of Alexa's stack, only the device's own `libOpenSLES.so` |
| the button does nothing, and the device is otherwise online | the `Button capture` switch, then the daemon log, which names the address that turned it off |
| the button rings Alexa rather than chiming | the same switch: it is off, which is what off means |

## How it works

`docs/` is the engineering record: what was measured on the hardware, and what
each decision is defending against.

* [Hardware](docs/hardware.md): the input nodes and keycodes, and how to test
  against a live Dot
* [Hard constraints](docs/constraints.md): what cannot change, and why
* [Architecture](docs/architecture.md): the subsystems, one at a time
* [Things that fail silently](docs/pitfalls.md): the failures that report
  success
* [Deployment](docs/deployment.md): installing and removing it

## Licence

BSD 2-Clause; [LICENSE.txt](LICENSE.txt) carries the terms.

The chime is original to this repository, so the licence covers it as it covers
the code. It is not a recording and not an asset: `internal/audio/chime.go`
renders it at startup from two sine tones, 880 Hz then 1320 Hz, faded out over
the last tenth of a second. It was an mp3 built by ffmpeg until the daemon
learned to make the sound itself, and those are that clip's own numbers.

This licence covers the code in this repository and nothing else. This
repository contains no Amazon code. The Amazon names it does carry identify
things already installed on the device, so that this software can interoperate
with them: functional names, not copied implementation.

overdub is not affiliated with, endorsed by, or supported by Amazon.
