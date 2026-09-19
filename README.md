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

**Alexa commands are optional, and what they risk is the account, not the Dot.**
They reach an undocumented endpoint with a credential that carries the whole
Amazon account, and that endpoint is the Alexa app's own rather than an
interface Amazon offers, so driving it may sit outside Amazon's terms. Read
[Alexa commands](#alexa-commands) before building MapDump. Without the jar the
daemon never offers them.

**The Dot also announces itself to Music Assistant, and plays.** It advertises
`_sendspin._tcp.local.` on tcp/8928, joins a group as a `player@v1`, keeps its
clock against the server's and plays the audio it is sent on the frame that
audio's own timestamp names. It reports `available: false` until that clock has
converged, which takes about a fifth of a second, and stays false on a Dot whose
speaker could not be opened at all. Anything that can reach the Dot on `wlan0`
can take that session, because the pairing flow is not implemented and the
fallback key is a published constant; what is behind it is playback and nothing
that reads off the device, but see [SECURITY.md](SECURITY.md) for what it does
hold. `switch.<name>_sendspin` turns the whole thing off -- the advert, the port
and any session -- and the setting survives a reboot.
`number.<name>_sendspin_output_delay` is the output delay, the one figure a music
server can also set: the last end to set it wins, and the number shows what is
applied whichever did.

What has not been settled is whether the Dot is in step with a *second* speaker
to better than a few milliseconds. One Dot can measure everything but that, so
grouping it with another Sendspin player and listening is the check that is left;
`docs/sendspin.md` says why and Music Assistant's own per-player delay is the dial
for whatever is left over.

Taking the action button takes it from Alexa. Stopping a timer or an alarm with
it, press-to-talk, and holding it to enter setup mode all stop working while the
daemon holds it. The `Action button mode` select gives it back without stopping
anything else, so the Dot keeps reporting and keeps its entities while Alexa
has her button. The microphone mute key is untouched throughout, and the volume
keys are too except while the Dot is muted, when they are held so that a press
lifts the mute.

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
* `adb`, on a development machine. A [release](#install-from-a-release) needs
  nothing else
* to build it yourself instead: Go 1.25 or later, and an **Android NDK** for the
  chime. It is played through OpenSL ES, so the daemon is cgo and `build.sh`
  will not run without one. `brew install --cask android-ndk` on macOS, or a
  release from [developer.android.com/ndk](https://developer.android.com/ndk)
  elsewhere, with `ANDROID_NDK_HOME` pointing at it
* Home Assistant on the same subnet as the Dot

The buttons on this hardware:

| Node | Name | Keycodes |
|---|---|---|
| `/dev/input/event1` | `mtk-kpd` | **138 action ("dot")**, 113 mute |
| `/dev/input/event2` | `keys` (gpio-keys) | 115 volume up, 114 volume down |

The daemon opens `event1` and takes keycode 138 from it, re-emitting the rest.
`event2` is opened only while the Dot is muted, to hold the volume keys. To
check them on another device, FireOS already ships the tools:

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

## Install from a release

Each [release](https://github.com/bboe/overdub/releases) carries one tarball
holding the scripts, the binary and `mapdump.jar`, already built. Unpack it and
install from it; the rest of this file applies unchanged from there:

```sh
tar xf overdub-v1.0.0.tar.gz
overdub-v1.0.0/deploy/install.sh kitchen
```

`SHA256SUMS` sits beside the tarball, and the build is attested, so GitHub can
be asked which workflow run and which commit produced the file you have:

```sh
shasum -a 256 -c SHA256SUMS      # sha256sum -c, where you have that instead
gh attestation verify overdub-v1.0.0.tar.gz --repo bboe/overdub
```

A released binary says what it is, which one built here does not:

```sh
adb shell 'su -c "/data/local/bin/overdub -version"'
```

The Dot's linker prints four `WARNING: linker:` lines of its own first, and adb
merges them into the same stream. The version is the last line:

```
overdub v1.0.0
```

Build it yourself instead for anything you want to change, or if you would
rather not run a binary somebody else compiled. The tarball carries no
`build.sh`, and `install.sh` builds when it finds one and installs what is in
`build/overdub` when it does not.

## Build

This section needs the repository. The release tarball carries neither
`build.sh` nor `deploy/mapdump/build.sh`, because it carries what they produce.

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

Alexa commands additionally need `mapdump.jar`, which is not in the repository
and is not built by `build.sh`. Skip this if you do not want them. `$SDK` is your
Android SDK root, and `deploy/mapdump/build.sh` names both jars, where to
download them, and the JDK it needs:

```sh
ANDROID_JAR=$SDK/platforms/android-22/android.jar \
R8_JAR=$SDK/build-tools/34.0.0/lib/d8.jar deploy/mapdump/build.sh
```

## Install

```sh
deploy/install.sh kitchen                          # binary, boot script, key
```

`install.sh` picks the jar up from `deploy/mapdump/mapdump.jar` if it is there
and says so either way; nothing else about the install changes.

Installing under a **different** name needs a reboot to finish. The boot script
is read once at boot, so the loop that respawns the daemon keeps the old name
until then; the install prints `REBOOT REQUIRED` when it sees that.

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
| `adb_keys` | `/data/local/bin/`, from `~/.android/adbkey.pub` or `$ADBKEY`; enables [`Secure`](#network-adb), and removed if there is no key to push |

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

`<name>` below is therefore the name **Home Assistant** knows the device by,
which is this one until somebody changes it there. Renaming the device in Home
Assistant offers to rename its entity ids to match, so a Dot installed as
`kitchen` and renamed afterwards answers to the new name in every id in the
table and to neither name in the daemon's own log, which goes on saying what
`-name` was given.

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

Everything goes: the boot script, the binary, the API key, the Sendspin identity,
`mapdump.jar` and the directory it sits in, the log the boot script writes, and
the two properties that remember whether Sendspin was switched off and the
output delay it was left at -- so installing
again starts with it on rather than inheriting a decision nothing on the device
explains.
`/data/local/bin` goes with them if nothing else is left in it. Removing the jar
revokes nothing: see [Alexa commands](#alexa-commands). The Sendspin identity
matters as much as the API key does -- the pairing token is derived from it, so a
copy left behind stays valid for a Dot that no longer runs this.

The tcp/6053 and tcp/8928 rules the daemon opened go too, once the daemon is
confirmed gone, so no reboot is needed. An uninstall that reports trouble stops
before that step and leaves them in the chain.

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
| `binary_sensor.<name>_alexa_registered` | diagnostic | whether the Dot holds an Amazon account, which is what setup gives it; a Dot that was never set up, or that deregistered itself, reads off while the button and the rest of this list go on working |
| `media_player.<name>_speaker` | none | the volume, the mute, the controls that change them, and playback: it sets the level of whichever route is live, and plays an mp3 you give it by handing the URL to Alexa's own synthesizer |
| `switch.<name>_microphone_muted` | none | whether the microphone is muted, and the control that changes it; muting from here presses the mute key, so it is the mute the button performs, ring included |
| `event.<name>_mute_button` | none | the microphone mute key, reported the same way the action button is |
| `select.<name>_mute_button_mode` | config | what the daemon does with the mute key; ships in `monitor` |
| `select.<name>_action_button_mode` | config | what the daemon does with the action button: intercept, monitor or pass through |
| `text.<name>_alexa_command` | config | a box that runs what you type on the Echo as though it had been spoken; listed only where `mapdump.jar` is installed and the Dot is registered |
| `select.<name>_network_adb` | config | adb over the network on tcp/5555: `Off`, `Insecure`, and `Secure` when a key was installed |
| `switch.<name>_sendspin` | config | whether the Dot offers Sendspin at all: off withdraws the mDNS advert, closes tcp/8928, deletes its firewall rule and ends any session in progress. Survives a reboot. Listed only where the Dot could read its Sendspin identity |
| `number.<name>_sendspin_output_delay` | config | milliseconds the Dot plays a server's audio ahead of the moment it was stamped for, 0 to 5,000. A music server can set the same figure and the last one set wins; whichever did, this is what is applied. Survives a reboot, works with the switch off, and is listed under the same condition |

Uptime, signal and the registration are read once a minute, and again when Home
Assistant subscribes. Both volumes, the jack, the temperature, the memory and the
microphone are read every two and a half seconds, whether the speaker is playing
every half second, and all of it only while something is subscribed -- except
uptime and signal, which are cheap enough to read either way. So the
registration has no state to show on the first connect after a restart, until
that subscriber's own reading arrives a moment later.

The microphone switch carries one caution: unmuting needs only the API key, not
a hand on the Dot, so anything holding that key can turn the microphone back on.
Everything is sent only when it changes, so the uptime arrives every minute,
the others when they move, and a quiet short tick costs the reads and no
traffic at all.

That is why a volume you have just turned appears within a few seconds, whether
you turned it with the buttons, from an app, or by asking Alexa.

Setting it from Home Assistant goes the other way down the same path: the daemon
asks Android for the level outright, so it lands exactly where you put the
slider and every other reader of the volume agrees with it afterwards. It moves
the route you are hearing, so with headphones in the socket the slider moves the
socket's level and leaves the speaker's alone.

It is silent, and it used to be heard: the daemon pressed the volume keys one
per step, and Android ticks on every adjustment. An automation setting the
volume at four in the morning no longer wakes the room.

Music Assistant can set it too, over Sendspin, and the same is true there. A
Dot that offers no volume is left out of the group volume a server works out,
so these are now part of it.

**Muting holds the volume keys.** Android does not lift its own mute when a
volume key is pressed -- it leaves the mute set and quietly resets the level to
one step -- so while the Dot is muted the daemon takes the keys, and the first
press lifts the mute and moves nothing. A second press then moves the volume as
usual. Only the keys on the Dot do this: a slider moved in Home Assistant while
muted sets the level and leaves the mute alone, so it takes effect when you
unmute. Two things follow from the grab: nothing else on the Dot sees a volume
press while it is muted, and Alexa's advanced factory reset, which is mute and
volume down held together, cannot complete until the mute is lifted.

`volume` is the speaker's own level and `jack_volume` is the socket's. Android
keeps a level per route and switches between them when you plug something in,
so between those two, the one you are hearing is whichever `audio_jack` says.
Both are reported whatever is plugged in, because both are real levels the
device would return to. Mute is not per route, so muting reads as zero on both
at once.

A bluetooth speaker is a third route and is not reported at all. Pair one and
Android tracks its level separately again, so neither of these readings is what
you are hearing and `audio_jack` does not say so.

### Playing something on it

`media_player.play_media` hands the URL to Alexa's synthesizer, which fetches and
plays it the way she plays her own speech -- mixed and ducked against whatever
else is going on, rather than fighting it. Two rules come from her rather than
from here:

* **The clip must be CBR mp3 at 48 kbps, 24 kHz, mono.** Anything variable is
  refused by her demuxer, and the refusal reads exactly like a file that is not
  there. Home Assistant's `tts.speak` takes `preferred_bitrate: 48` from 2026.9,
  which is the whole of the setup on newer versions.
* **The URL must be `http://`, with no comma or double quote in it.** The intent
  that carries it is a comma-separated array and hand-built JSON, so those two
  characters end it early. `https` is not refused by Alexa so much as unverified
  here.

The Dot fetches the clip itself, so whatever serves it has to be reachable *from
the Dot*, which is the opposite direction from the one Home Assistant uses to
reach the Dot. A `/local/` file served by Home Assistant is the ordinary case.

```yaml
action: tts.speak
target:
  entity_id: tts.home_assistant_cloud
data:
  media_player_entity_id: media_player.kitchen_speaker
  message: "The back door has been open for ten minutes"
  options:
    preferred_bitrate: 48
```

The entity reports `playing` while Alexa is actually playing rather than from
the moment you ask, because the daemon learns it from her playback log; expect
roughly two thirds of a second before it moves. If she never plays it at all,
the entity gives up and goes back to idle rather than sticking.

**When nothing plays, the daemon's log is the wrong place to look.** It records
what was asked for and nothing else, because everything after that is hers: she
fetches the clip, she decodes it, and she is where it fails. Her log is where
the reason is:

```sh
adb shell 'su -c "logcat -d -v brief -s tts-Server tts-Playback"'
```

Three failures look alike from Home Assistant and are easy to tell apart there:

* `cannot estimate length of the next mp3 frame` is the encoding -- a variable
  bitrate her demuxer will not take.
* `Playback ended: ... FAILED` with nothing before it is usually the fetch: a
  404, or a URL the Dot cannot route to.
* **No lines at all** means the request never reached her.

The routing one catches people out, because it fails in complete silence. The
Dot fetches the clip itself, and a Dot on an IoT network often cannot open a
connection to the machine serving it even though that machine reaches the Dot
perfectly well. Ask the Dot rather than assuming:

```sh
adb shell 'su -c "curl -sS -m 5 -o /dev/null -w %{http_code} http://<host>:8123/"'
```

`200` means the route is open. A curl error and `000` is the answer: open that
one route, and everything else already works.

The mode selects are the only entities Home Assistant writes to. There is one
per button -- the action button and the microphone mute -- and each has three
settings:

| mode | Alexa | Home Assistant |
|---|---|---|
| `intercept` | nothing | events |
| `monitor` | answers the press as usual | events |
| `pass through` | answers the press as usual | nothing |

The action button ships in `intercept`, which is what the daemon is for. **Mute
ships in `monitor`**: taking it by default would leave a Dot that cannot be
muted by the button that says mute on it, and monitor is additive -- Alexa still
mutes, and your automation still fires. `pass through` gives the
button back -- press-to-talk is measured, and timers and setup mode follow the
same key path -- while the daemon keeps running and keeps reporting everything
else. `monitor` is both at once: press-to-talk still works and your automation
fires too. Only an intercepted press chimes, because in monitor Alexa answers it
herself and two acknowledgements for one press is worse than none.

Changing it takes no reboot and no reinstall. A press arriving mid-change
belongs to whichever mode was set when the key went down, so nothing is ever
half-delivered.

The daemon starts in `intercept`, and the mode is not remembered across a
restart: a Dot that reboots comes back holding its button whatever was set
before. Home Assistant will restore it if you ask it to, with an
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
carries `event_type`, `device` and `button` -- every button fires the same bus
event, so `button` is what tells them apart and an automation wants it in its
trigger. `multi_press_count` comes with `multi_press_end`, `held_ms` with
`long_press_end`. Both are integers, so
`{{ trigger.event.data.multi_press_count == 7 }}` works without a cast. The
blueprint in `ha/` wires the gestures up.

**Every button fires the same bus event**, so an automation that filters only on
`device_id` runs for all of them: with mute in `monitor`, a press of the mute key
would run an action meant for the action button. Filter on `button` as well. The
blueprint in `ha/` does, but **Home Assistant does not update a blueprint you
have already imported** -- re-import it after upgrading, or the mute key will run
your single-press action.

`long_press_start` and `long_press_end` are not a guaranteed pair. Events carry
no state, so if Home Assistant misses the release -- a restart, a reconnect --
whatever the hold started keeps running. Give such an automation its own timeout.

It is an event rather than a sensor, so it has no state to read: an automation
triggers on it, and the dashboard shows no value between presses. A Home
Assistant that was not connected does not learn about a press afterwards.

Only a press the daemon reports does any of this. In `pass through` the button
is Alexa's and a press is unreported. The chime tells intercept from the other
two rather than telling you the daemon is alive: `monitor` reports the press
without chiming, so silence there is the mode working as asked.

The mode selects are still the only entities Home Assistant writes to. Everything
else reports, the button included, and together they are the connection proved
end to end in both directions.

### Alexa commands

With `mapdump.jar` installed, the Dot gains a text box on its device page and a
matching action. Either runs text on the Echo as though somebody had said it:

```yaml
action: esphome.kitchen_send_command
data:
  text: play dance party music on Amazon Music
```

Two things have to be true: the jar has to be installed, and the Dot has to be
registered to an Amazon account. If either is missing, neither the box nor the
action is advertised at all, rather than being offered and failing on every
call, and the daemon log says which one it was. The one exception is a Dot that
cannot be asked -- if the registration reading itself fails, the box is offered
rather than hidden, because the jar is the switch somebody chose and a reading
that did not happen is not an answer. The
box shows the last command it was given, so an automation that sent one can be
seen to have sent it.

**Registration is the credential**, which is why it gates the feature.
`binary_sensor.<name>_alexa_registered` reports it, and a Dot that was factory
reset, or rooted and restored, usually is not registered.

Registering one is Alexa's own process, and the daemon's only part in it is to
get out of the way:

1. Set `Action button mode` to `pass through`, which gives Alexa her button
   back without stopping anything else here.
2. Hold the action button until the ring turns orange. That is Alexa's own long
   press, and it is what the daemon was intercepting.
3. Add the device in the Alexa app. The Dot brings up its own Wi-Fi network to
   finish, so it leaves your LAN and Home Assistant shows it as unavailable
   until setup ends. `adb` over USB is unaffected; `adb` over the network goes
   with the LAN.
4. Set the button mode back to `intercept`.

The daemon needs nothing else: it asks again every five minutes, so the command
box appears on its own once the Dot is registered, with no restart and no
reinstall.

> **This is an unofficial endpoint, reached with an account-level credential.**
> `/api/behaviors/preview` is undocumented and its request shape has drifted
> before.
>
> What MapDump reads is the OAuth refresh token this Echo was registered with,
> and the daemon trades it for cookies scoped to `.amazon.com` rather than to an
> Alexa subdomain. Measured, what comes back is `at-main`, `sess-at-main`,
> `session-id`, `session-token`, `ubid-main` and `x-main` -- the set a browser
> signed in to amazon.com carries. So while the daemon runs it holds a signed-in
> session on the retail account, order history and addresses included: the
> breadth [alexa_media_player](https://github.com/alandtse/alexa_media_player)
> gets by asking you to log in, arrived at from the other end. Not AWS, which is
> a separate sign-in. The token and the cookies are kept in memory and never
> written to disk, and the only lock on the port that reaches them is the API
> key.
>
> The failure mode to watch for is not an error from the daemon but the Dot
> quietly deregistering. To revoke what it holds, deregister the Dot in the
> Alexa app or under Manage Your Content and Devices; uninstalling removes the
> jar but revokes nothing already extracted.

### Network ADB

`Network ADB` turns adb on over the network, on tcp/5555, so the Dot can be
worked on without a cable. It has three positions, and a reboot always returns
it to the first:

| Position | What it does |
|---|---|
| `Off` | adbd stops listening on the network, and tcp/5555 is closed again |
| `Insecure` | adb is open to the local network, and **anyone on it may connect** |
| `Secure` | adb is open, but only a client holding the installed key may connect |

Connect with `adb connect <address>:5555`. `ANDROID_SERIAL` then picks that
target for `install.sh` as readily as a cable does.

**`Off` and `Insecure` are the network only.** Neither touches adb over USB, so
`Off` is not a way to lock the Dot down against someone holding it: it closes
tcp/5555 and nothing else.

**`Secure` reaches the cable as well.** `ro.adb.secure` is a setting on `adbd`
rather than on one transport, so while it is on, every adb connection is
challenged. A machine that does not hold the installed key gets `unauthorized`
over USB too, and this Dot has no screen to show the prompt a phone would.

Changing position restarts `adbd`, which drops every live adb session -- the one
you may be watching from included. Do not change it from an install that is
running over adb.

`Secure` is offered only if `deploy/install.sh` found a public key to install --
`~/.android/adbkey.pub`, or whatever `ADBKEY` names. Without one it would refuse
every machine including yours, so it is not listed at all. That key becomes the
*only* one adbd will accept: a Dot that had authorised other machines over USB
stops accepting them.

> **Secure is authentication, not a sandbox.** `ro.secure=1` on this build, so
> adbd runs as `shell` and root still comes from `su`. A client holding the key
> reaches root exactly as it did before. It decides who may connect, not what
> they may do, and it is not a reason to expose tcp/5555 beyond your own network.

If a key ever locks you out, set the control back to `Off` or `Insecure` from
Home Assistant. That path is the ESPHome API on tcp/6053 and owes nothing to
adb. Failing that, power-cycle the Dot.

Uninstalling does not turn it off. The position lives in the property store and
the firewall chain rather than on disk, and `uninstall.sh` may itself be running
over the connection it would cut, so it says so and leaves it to a reboot.

Installing with no key while the Dot is in `Secure` leaves the control reporting
a position it no longer offers, and Home Assistant marks that state invalid.
Both halves are true: the Dot really is in `Secure`, and `Secure` really cannot
be chosen with no key to install. Nothing is broken by it, and a reboot clears
it, since `ro.adb.secure` does not survive one.

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
| the button does not chime | the mode first: only `intercept` chimes. Then the daemon log, where `presses will be silent` means the audio player did not start. It needs nothing of Alexa's stack, only the device's own `libOpenSLES.so` |
| the button does nothing, and the device is otherwise online | the `Action button mode` select, then the daemon log, which names the address that changed it |
| the button rings Alexa rather than chiming | the mode. `pass through` is Alexa's alone; `monitor` is hers *and* reported, and only `intercept` chimes |

## How it works

`docs/` is the engineering record: what was measured on the hardware, and what
each decision is defending against.

* [Hardware](docs/hardware.md): the input nodes and keycodes, and how to test
  against a live Dot
* [Hard constraints](docs/constraints.md): what cannot change, and why
* [The button](docs/button.md): the grab, the clone, the modes, and what a
  press reports
* [The Home Assistant API](docs/api.md): the entities, the polls, and the
  encryption
* [Finding the Dot](docs/mdns.md): the mDNS responder, and what it advertises
  for
* [Network adb, and the microphone](docs/device.md): the two things Home
  Assistant can switch on the Dot itself
* [Audio](docs/audio.md): what it takes to make a sound here, and the chime
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
