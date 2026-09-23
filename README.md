# overdub

Take over the **action button** on a rooted Echo Dot (2nd Generation) and
present the Dot to Home Assistant as an **ESPHome device**, while stock Alexa
keeps running.

- Home Assistant adopts it with its own ESPHome integration: no custom
  component, no MQTT, no Home Assistant credential on the Dot.
- The Dot reports presses and holds of its buttons, reports what it can read
  about itself, and chimes on every press it takes.
- It also joins Music Assistant as a Sendspin player.

## Scope

- This runs on a Dot you own and have already rooted. Nobody supports it, and a
  FireOS update can break any of it.
- **Alexa commands are optional, and they risk the account, not the Dot.** They
  reach an undocumented Alexa app endpoint with a credential that carries the
  whole Amazon account, which may sit outside Amazon's terms. Read
  [Alexa commands](#alexa-commands) before you build MapDump. Without the jar,
  the daemon does not offer them.
- **Sendspin has no pairing.** The pairing flow is not implemented and the
  fallback key is a published constant, so anything that reaches the Dot on
  `wlan0` can take the session. The session gives playback only; see
  [SECURITY.md](SECURITY.md) for what it holds.
- Taking the action button takes it from Alexa. While the daemon holds it, the
  button does not stop timers or alarms, talk, or enter setup mode. The
  `Action button mode` select gives it back without stopping anything else.
- Alexa still gets the mute key by default. The volume keys are untouched,
  except while the Dot is muted (see
  [Muting](#muting-holds-the-volume-keys)).

## How this differs from EchoMuse, echolocal and EchoGo

The other projects on this hardware replace Alexa. This one adds to her. It
takes only the action button, and the button goes back to her if the daemon
dies.

- [**EchoMuse**](https://github.com/wilbowes/EchoMuse): replaces the Alexa
  firmware with a local voice assistant and media player for Home Assistant.
  Its docs cover the amonet-biscuit unlock, the practical route to root.
- [**echolocal**](https://github.com/ygelfand/echolocal): the same Dot, also
  Go and the ESPHome API, with local wake word, LED ring, media player and a
  Bluetooth proxy.
- [**EchoGo**](https://github.com/Binozo/EchoGo): a Go SDK for the LEDs,
  microphone, speaker and buttons, for writing the device software yourself.

Use one of those for a local voice satellite without Amazon. Use this one to
keep Alexa and add a Home Assistant button and some entities.

## Requirements

- An Echo Dot (2nd Generation), model **RS03QR** (printed on the underside),
  codename biscuit, FireOS 5.5.5.4, rooted, with Magisk. Everything here was
  measured on that model.
- **Magisk 17.3**, or another that keeps `service.d` at
  `/sbin/.core/img/.core/service.d`. `install.sh` writes the boot script only
  there, and fails if it cannot. A Magisk that uses `/data/adb/service.d`
  needs the path changed first.
- `adb` on a development machine. A [release](#install-from-a-release) needs
  nothing else.
- To build it yourself: Go 1.25 or later, and an **Android NDK**
  (`brew install --cask android-ndk` on macOS, or
  [developer.android.com/ndk](https://developer.android.com/ndk)).
  Set `ANDROID_NDK_HOME` to it.
- Home Assistant on the same subnet as the Dot.

The buttons:

| Node | Name | Keycodes |
|---|---|---|
| `/dev/input/event1` | `mtk-kpd` | **138 action ("dot")**, 113 mute |
| `/dev/input/event2` | `keys` (gpio-keys) | 115 volume up, 114 volume down |

- The daemon grabs `event1` and re-emits its keys through a clone named
  `mtk-kpd`, except those a button's mode keeps from Alexa.
- It opens `event2` only while the Dot is muted.
- To inspect the nodes on the device:

```sh
adb shell su -c 'cat /proc/bus/input/devices'   # names, handlers, key bitmaps
adb shell su -c getevent                        # events, without grabbing
```

## Coming from EchoMuse

EchoMuse's debloat step suppresses the Alexa stack this runs beside. Undo it
first; nothing was uninstalled, so nothing needs reinstalling:

```sh
deploy/restore-amazon.sh   # then reboot
```

- It restores every hidden or disabled package, not only EchoMuse's. Things
  you suppressed yourself come back too.
- `com.amazon.device.software.ota` stays hidden. An OTA rewrites `boot.img`
  and removes Magisk, root and overdub.
- EchoMuse's payload moves to `/data/local/echomuse-disabled/`. Its
  `service.d` debloat hook is deleted.
- Skip this if the Dot never had EchoMuse.
- docs/deployment.md says how the script decides what to restore.

## Install from a release

Each [release](https://github.com/bboe/overdub/releases) carries one tarball
with the scripts, the binary and `mapdump.jar`, already built:

```sh
tar xf overdub-v1.0.0.tar.gz
overdub-v1.0.0/deploy/install.sh kitchen
```

`SHA256SUMS` sits beside the tarball, and the build is attested:

```sh
shasum -a 256 -c SHA256SUMS      # or sha256sum -c
gh attestation verify overdub-v1.0.0.tar.gz --repo bboe/overdub
```

A released binary reports its version. A local build prints
`overdub (unversioned build)`:

```sh
adb shell 'su -c "/data/local/bin/overdub -version"'
```

The Dot's linker prints 4 `WARNING: linker:` lines first, merged into the same
stream. The version is the last line:

```
overdub v1.0.0
```

- The tarball carries no `build.sh`. `install.sh` builds when it finds
  `build.sh`, and otherwise installs `build/overdub` as it stands.
- Build it yourself to change anything, or to run only a binary you compiled.

## Build

This needs the repository, not the tarball.

```sh
./build.sh
```

- `build.sh` sets `GOOS=android GOARCH=arm GOARM=7`, finds the NDK compiler,
  and makes the empty `libpthread` and `librt` stubs cgo needs. Neither target
  setting is optional; [docs/constraints.md](docs/constraints.md) says why.
- If it cannot find the NDK, it says so. Set `ANDROID_NDK_HOME`.

Alexa commands also need `mapdump.jar`, which `build.sh` does not build. Skip
this if you do not want them. `$SDK` is your Android SDK root;
`deploy/mapdump/build.sh` names both jars, where to get them, and the JDK it
needs:

```sh
ANDROID_JAR=$SDK/platforms/android-22/android.jar \
R8_JAR=$SDK/build-tools/34.0.0/lib/d8.jar deploy/mapdump/build.sh
```

## Install

```sh
deploy/install.sh kitchen                          # binary, boot script, key
```

- `install.sh` installs `deploy/mapdump/mapdump.jar` if it is there, and says
  which.
- With more than one Dot on `adb`, set `ANDROID_SERIAL`:

```sh
adb devices                                   # serials
ANDROID_SERIAL=<serial> deploy/install.sh kitchen
```

What goes where:

- `overdub`: `/data/local/bin/`
- `overdub.sh` (the boot script): Magisk `service.d`, inside `magisk.img`
- `.overdub-noise-key`: `/data/local/bin/`, mode 600, generated if absent
- `mapdump.jar`: `/data/local/map/`, if it was built
- `adb_keys`: `/data/local/bin/`, from `~/.android/adbkey.pub` or `$ADBKEY`.
  It enables [`Secure`](#network-adb). Removed if there is no key to push.

The name:

- **Required, and unique on your network.** Home Assistant prefixes every
  entity id with it. A second Dot under a name in use stops at a conflict menu.
- Lowercase letters, digits, `-` and `_`; at most 63 characters; not starting
  or ending with `-`. These are ESPHome's rules, and `install.sh` enforces them.
- Home Assistant identifies the device by its MAC, so a rename is accepted on
  the next connection. Entity ids keep the prefix they were created with.
- A rename needs a reboot. The supervisor reads the boot script once at boot,
  so it respawns the old name until then. `install.sh` prints
  `REBOOT REQUIRED` when it sees this.
- `<name>` below is the name **Home Assistant** knows the device by. If you
  rename the device there, the entity ids can follow, while the daemon log
  keeps the `-name` it was given.

Reboot to start the daemon. It is supervised. If it dies, the grab is released
and the action button goes back to Alexa.

## Usage

The boot script runs the daemon. To run it by hand, as root, by full path:

```sh
adb shell 'su -c "/data/local/bin/overdub -name kitchen"'
```

| Flag | |
|---|---|
| `-name` | **required**: the unique device name Home Assistant knows |
| `-version` | print the version and exit |

Everything else is fixed in the binary: `event1`, keycodes 138 and 113,
`mtk-kpd` for the clone, `wlan0`, tcp/6053 and tcp/8928.

## Uninstall

```sh
deploy/uninstall.sh
```

- It removes the boot script first. A reboot part way through leaves nothing
  running, not a supervisor respawning a half-deleted install.
- The daemon gets `SIGTERM`, so it releases the button and destroys its uinput
  clones.
- It removes the boot script, the binary, the API key, the Sendspin identity,
  `/data/local/map`, the log, and every `persist.overdub.*` property. A new
  install starts with Sendspin on and no output delay.
- `/data/local/bin` goes too if it is then empty.
- The tcp/6053 and tcp/8928 rules go once the daemon is confirmed gone, so no
  reboot is needed. An uninstall that reports trouble stops before this step.
- Removing the jar revokes nothing; see [Alexa commands](#alexa-commands).
- The Sendspin identity matters as much as the API key: the pairing token
  derives from it, so a copy left behind stays valid.
- Amazon's stack is untouched. Delete the device in Home Assistant when done.

## Home Assistant

- The Dot announces itself over mDNS. It appears under
  **Settings -> Devices & Services**, named after `-name`. Adding it asks only
  for the encryption key the installer printed.
- If it does not appear, use **Add integration -> ESPHome** with the Dot's
  address and port `6053`. `adb shell ip -4 addr show wlan0` gives the address.
  Discovery does not cross subnets.
- Set a DHCP reservation. Home Assistant re-finds a moved device by name only
  while discovery reaches it.
- The Dot is the server; Home Assistant dials in to tcp/6053. The daemon opens
  that port on the Dot's firewall.

> **The key is the whole of the access control.** ESPHome has no peer
> allowlist, so anything that can route to the Dot may connect. A peer without
> the key learns the device name and holds 1 of 8 slots for 10 seconds; 8 of
> them keep Home Assistant off the Dot for as long as they like. The firewall
> rule matches the interface, not a source range, so a VPN client on another
> subnet is inside it. SECURITY.md has the measurement.

### What it exposes

Each entity id is `<domain>.<name>_<suffix>`.

| domain | suffix | category | value |
|---|---|---|---|
| event | `action_button` | control | a gesture (1) |
| event | `mute_button` | control | a gesture (1) |
| media_player | `speaker` | control | volume, mute, playback (2) |
| switch | `microphone_muted` | control | on: muted (3) |
| sensor | `uptime` | diagnostic | seconds since boot |
| sensor | `wifi_signal` | diagnostic | dBm (4) |
| sensor | `volume` | diagnostic | % of the speaker's 30 steps |
| sensor | `jack_volume` | diagnostic | % for the 3.5 mm output |
| sensor | `bluetooth_volume` | diagnostic | % for a paired speaker |
| sensor | `cpu_temperature` | diagnostic | °C, the SoC's thermal zone |
| sensor | `memory_available` | diagnostic | MiB an allocation could get |
| binary_sensor | `audio_jack` | diagnostic | on: a plug in the socket |
| sensor | `output_device` | diagnostic | `speaker`, `jack`, `bluetooth` (5) |
| sensor | `bluetooth_device` | diagnostic | the connected speaker (5) |
| binary_sensor | `speaker_playing` | diagnostic | on: wired audio out (6) |
| binary_sensor | `alexa_registered` | diagnostic | on: has an account (7) |
| select | `action_button_mode` | config | starts `intercept` (8) |
| select | `mute_button_mode` | config | starts `monitor` (8) |
| select | `network_adb` | config | `Off`, `Insecure`, `Secure` (9) |
| text | `alexa_command` | config | run as though spoken (10) |
| switch | `sendspin` | config | on: Sendspin is up (11) |
| number | `sendspin_output_delay` | config | 0 to 5,000 ms (12) |

1. See [The action button](#the-action-button).
2. Volume and mute of the live route, and playback of an mp3 URL through
   Alexa's synthesizer.
3. Setting it presses the mute key, ring included.
4. A reading that is not a signal is missing, not zero.
5. Bluetooth that carries no audio, such as the Alexa app over BLE, does not
   count. `bluetooth_device` is the speaker's name, or its address when the Dot
   has no name for it, and empty when no speaker is connected.
6. Sound shorter than about 1.5 seconds is not reported. Bluetooth is not seen.
7. Off on a Dot never set up, or deregistered; everything else still works.
8. `intercept`, `monitor` or `pass through`.
9. `Secure` only when a key was installed.
10. Listed only with `mapdump.jar` installed and the Dot registered.
11. Off withdraws the mDNS advert, closes tcp/8928, deletes its firewall rule
    and ends any session. Survives a reboot.
12. How far ahead of a chunk's timestamp the Dot plays. A music server can set
    it too; the last write wins, and the number shows what is applied.
    Survives a reboot and works with the switch off.

The Sendspin entities are listed only when the Dot could read or create its
Sendspin identity.

### How often it reads

| every | reads | when |
|---|---|---|
| 60 seconds | uptime, signal | always |
| 60 seconds | registration, Bluetooth device | subscribed |
| 2.5 seconds | 3 volumes, jack, route, temperature, memory, mic | subscribed |
| 500 ms | whether the speaker is playing | subscribed |

- The first subscriber wakes both polls. The registration has no state on the
  first connect after a restart until that reading arrives.
- A speaker connecting or disconnecting reads the Bluetooth device again within
  a few seconds.
- A value is sent only when it changes.

### Volume

- A volume change appears within a few seconds, whether it came from the keys,
  an app or Alexa.
- Setting it from Home Assistant sets the level of the route in
  `output_device` exactly. It is silent: no key press, so no tick.
- Music Assistant can set it over Sendspin too.
- Android keeps a level per route: `volume` for the speaker, `jack_volume` for
  the socket, `bluetooth_volume` for a paired speaker. All 3 are reported
  whatever is connected.
- Mute is not per route. Muted, all 3 read 0.
- `bluetooth_volume` is the Dot's attenuation, in series with the speaker's own
  volume. Turning the speaker itself down is invisible here.
- Plugging in a cable disconnects a paired speaker, and it does not reconnect
  when you pull the cable. Connecting a speaker with a cable already in works:
  the Dot plays to the speaker.

#### Muting holds the volume keys

- Android does not lift its mute on a volume key; it resets the level to 1 step
  instead. So while the Dot is muted, the daemon grabs the volume keys.
- The first key press lifts the mute and moves nothing. The next moves the
  volume.
- A Home Assistant slider moved while muted sets the level and leaves the mute
  on.
- While muted, nothing else on the Dot sees a volume key. Alexa's advanced
  factory reset (mute and volume down held) cannot complete.

### Playing something on it

`media_player.play_media` hands the URL to Alexa's synthesizer. She fetches it
and plays it like her own speech, mixed and ducked. Her rules:

- **CBR mp3, 48 kbps, 24 kHz, mono.** She refuses variable bitrate, and the
  refusal looks like a missing file. Home Assistant's `tts.speak` takes
  `preferred_bitrate: 48` from 2026.9.
- **`http://` only, with no comma or double quote in the URL.** Those two
  characters end the intent early. `https` is unverified.
- **The Dot fetches the clip itself**, so the server must be reachable *from
  the Dot*. A `/local/` file served by Home Assistant is the ordinary case.

```yaml
action: tts.speak
target:
  entity_id: tts.home_assistant_cloud
data:
  media_player_entity_id: media_player.kitchen_speaker
  message: "The back door has been open for 10 minutes"
  options:
    preferred_bitrate: 48
```

- The entity reads `playing` only while Alexa plays, from her playback log.
  Expect about 0.7 seconds before it moves.
- If she never starts, it goes back to idle after 30 seconds.

**When nothing plays, read her log, not the daemon's.** The daemon logs only
the request:

```sh
adb shell 'su -c "logcat -d -v brief -s tts-Server tts-Playback"'
```

- `cannot estimate length of the next mp3 frame`: variable bitrate.
- `Playback ended: ... FAILED` with nothing before it: usually the fetch, a 404
  or a URL the Dot cannot route to.
- **No lines at all**: the request never reached her.

A Dot on an IoT network often cannot reach the server even though the server
reaches the Dot. Ask the Dot:

```sh
url=http://<host>:8123/
adb shell "su -c 'curl -sS -m 5 -o /dev/null -w %{http_code} $url'"
```

`200` means the route is open. A curl error and `000` means it is not.

### The button modes

Each button has a mode select with 3 settings:

| mode | Alexa | Home Assistant |
|---|---|---|
| `intercept` | nothing | events |
| `monitor` | answers the press as usual | events |
| `pass through` | answers the press as usual | nothing |

- The action button starts in `intercept`. Mute starts in `monitor`, so the
  mute button still mutes.
- `pass through` gives Alexa the button back; the daemon keeps reporting
  everything else.
- Only an intercepted action button press chimes. In `monitor`, Alexa answers
  the press herself, so silence there is correct.
- A change needs no reboot. A press belongs to the mode set when its key went
  down.
- Modes are not remembered: after a restart each button is back in its
  starting mode. Restore it with an automation on `homeassistant_start` or on
  the device becoming available.

### The action button

It reports Home Assistant's standard button event types, so an automation for
any other button works here.

- Quick presses form a run. `multi_press_end` fires with the count 350 ms after
  the last release.
- A single press fires `press_end`, after the same 350 ms wait.
- A hold past 600 ms (Alexa's long-press threshold) fires `long_press_start`
  **while you still hold**. Release fires `long_press_end`. A hold ends any run
  before it, and that run is reported first.
- If the daemon notices the threshold late, it sends both hold events at
  release. It never reports a hold early.
- The chime sounds on key-down, once per press: 4 presses are 4 chimes and one
  `multi_press_end`.
- `long_press_start` and `long_press_end` are not a guaranteed pair. If Home
  Assistant misses the release, whatever the hold started keeps running. Give
  such an automation its own timeout.
- It is an event, so it has no state, and a press made while Home Assistant was
  disconnected is lost.

Each gesture also fires `esphome.overdub_pressed` on Home Assistant's bus:

- Always: `event_type`, `device` and `button`.
- `multi_press_count` with `multi_press_end`; `held_ms` with `long_press_end`.
  Both are integers: `{{ trigger.event.data.multi_press_count == 7 }}` works.
- **Every button fires this event.** Filter on `button` as well as
  `device_id`, or a mute press in `monitor` runs your action button's action.
- The blueprint in `ha/` filters on `button`. **Home Assistant does not update
  an imported blueprint**: re-import it after upgrading.

### Alexa commands

With `mapdump.jar` installed, the Dot gets a text box and a matching action.
Either runs text on the Echo as though spoken:

```yaml
action: esphome.kitchen_send_command
data:
  text: play dance party music on Amazon Music
```

- Both appear only when the jar is installed and the Dot is registered to an
  Amazon account. Otherwise neither is listed, and the daemon log says why.
- If the registration cannot be read, the box is offered.
- The box shows the last command it was given.
- The daemon checks again every 5 minutes, so the box appears without a restart
  once both are true.
- **Registration is the credential.** `binary_sensor.<name>_alexa_registered`
  reports it. A factory-reset or restored Dot is usually not registered.

To register a Dot:

1. Set `Action button mode` to `pass through`.
2. Hold the action button until the ring turns orange.
3. Add the device in the Alexa app. The Dot brings up its own Wi-Fi network
   for setup, so it leaves your LAN and Home Assistant shows it unavailable
   until setup ends. `adb` over USB still works; network `adb` does not.
4. Set the button mode back to `intercept`.

> **This is an unofficial endpoint, reached with an account-level credential.**
> `/api/behaviors/preview` is undocumented, and its request shape can change.
>
> MapDump reads the OAuth refresh token this Echo was registered with. The
> daemon trades it for cookies scoped to `.amazon.com`: `at-main`,
> `sess-at-main`, `session-id`, `session-token`, `ubid-main` and `x-main`. That
> is a signed-in retail session, order history and addresses included, the same
> breadth [alexa_media_player](https://github.com/alandtse/alexa_media_player)
> gets from a login. It is not AWS. The token and cookies stay in memory, and
> the API key is the only lock on the port that reaches them.
>
> Watch for the Dot quietly deregistering, not for an error. To revoke,
> deregister the Dot in the Alexa app or under Manage Your Content and Devices.
> Uninstalling revokes nothing already extracted.

### Network ADB

`Network ADB` turns on adb over tcp/5555. A reboot returns it to `Off`.

| Position | What it does |
|---|---|
| `Off` | adbd stops listening on the network; tcp/5555 closes |
| `Insecure` | open to the network: **anyone on it may connect** |
| `Secure` | open only to a client holding the installed key |

- Connect with `adb connect <address>:5555`. `ANDROID_SERIAL` then picks it for
  `install.sh`.
- **`Off` and `Insecure` affect the network only.** Neither touches USB.
- **`Secure` covers USB too.** `ro.adb.secure` applies to every transport, so
  a machine without the installed key gets `unauthorized` over USB, and the Dot
  has no screen to approve it.
- `Secure` is offered only if `install.sh` found a public key
  (`~/.android/adbkey.pub` or `$ADBKEY`). That key becomes the *only* one adbd
  accepts.
- A position change restarts adbd, which drops every adb session. Do not change
  it from an install that runs over adb.
- If a key locks you out, set `Off` or `Insecure` from Home Assistant; that path
  does not use adb. Failing that, power-cycle the Dot.
- Uninstalling does not turn it off. A reboot does.
- Installing with no key while in `Secure` leaves the select showing a position
  it no longer offers, which Home Assistant marks invalid. A reboot clears it.

> **Secure is authentication, not a sandbox.** `ro.secure=1` on this build, so
> adbd runs as `shell` and root comes from `su`. A client with the key still
> reaches root. Do not expose tcp/5555 beyond your own network.

### Encryption

- The API speaks only ESPHome's `Noise_NNpsk0_25519_ChaChaPoly_SHA256`. There
  is no plaintext mode.
- `deploy/install.sh` generates the key when the Dot has none, and prints it
  once:

```
Generated an API encryption key. Paste it into Home Assistant's
ESPHome integration. The installer keeps no copy:

    kR2b...
```

- Keep it. A reinstall keeps an existing key, so Home Assistant stays paired.
  The installer does not accept a key of your own.
- To rotate: delete `/data/local/bin/.overdub-noise-key` on the Dot, install
  again, and paste the new key into Home Assistant.

## Troubleshooting

```sh
adb shell 'su -c "cat /data/local/tmp/overdub.log"'      # truncated per boot
adb shell 'su -c "iptables -L INPUT -n -v | grep 6053"'  # packet counter
adb shell 'su -c "logcat -d -v brief -s tts-Server tts-Playback"'   # playback
```

- **Home Assistant times out adding the Dot**: check the tcp/6053 packet
  counter. 0 means the traffic never arrived.
- **`Unexpected device found`**: the stored address now answers with a
  different MAC. Set a DHCP reservation.
- **The key is invalid**: the daemon log. `handshake failed` means Home
  Assistant's key is not the one on the Dot.
- **The device requires encryption**: Home Assistant has no key for this Dot.
  Give it the one the installer printed.
- **Nothing starts; the log ends
  `no such file or directory (deploy/install.sh generates one)`**: there is no
  key. Rerun `deploy/install.sh <name>`.
- **Nothing starts; the log says the key `decodes to N bytes`**: the key is
  corrupt. A reinstall keeps it, so delete it first:
  `adb shell 'su -c "rm -f /data/local/bin/.overdub-noise-key"'`
- **Nothing starts; the log says `NAME is unset`**: the boot script was
  installed by hand. Rerun `deploy/install.sh <name>`.
- **Mute stopped working**: the clone's name. Android picks a keylayout by
  device name, so it must be `mtk-kpd`.
- **Every keycode looks wrong**: the build. `GOARCH=arm` is required.
- **The button does not chime**: the mode first; only `intercept` chimes. Then
  the daemon log: `presses will be silent` means the audio player did not
  start. It needs only the device's own `libOpenSLES.so`.
- **The button does nothing**: the `Action button mode` select, then the daemon
  log, which names the address that changed it.
- **The button rings Alexa instead of chiming**: the mode is `pass through` or
  `monitor`.

## How it works

`docs/` records what was measured on the hardware and what each decision
defends against.

- [Hardware](docs/hardware.md): the input nodes, and testing against a live Dot
- [Hard constraints](docs/constraints.md): what cannot change, and why
- [The button](docs/button.md): the grab, the clone, the modes, the gestures
- [The Home Assistant API](docs/api.md): the entities, the polls, the
  encryption
- [Finding the Dot](docs/mdns.md): the mDNS responder and its adverts
- [Network adb, and the microphone](docs/device.md): what Home Assistant can
  switch on the Dot itself
- [Audio](docs/audio.md): making a sound here, and the chime
- [Alexa commands](docs/command.md): the credential, MapDump, the text command
- [Music Assistant](docs/sendspin.md): the Sendspin client, its handshake, its
  clock
- [Things that fail silently](docs/pitfalls.md): failures that report success
- [Deployment](docs/deployment.md): installing and removing it

## Licence

- BSD 2-Clause; [LICENSE.txt](LICENSE.txt) carries the terms.
- The chime is original to this repository and covered by the same licence.
  `internal/audio/chime.go` renders it at startup from 2 sine tones, 880 Hz
  then 1320 Hz.
- This repository contains no Amazon code. The Amazon names it carries identify
  things already on the device, so this software can interoperate with them.
- overdub is not affiliated with, endorsed by, or supported by Amazon.
