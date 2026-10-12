# overdub

Take over the **action button** on a rooted Echo Dot (2nd Generation) and
present the Dot to Home Assistant as an **ESPHome device**, while stock Alexa
keeps running.

- Home Assistant adopts it with its own ESPHome integration: no custom
  component, no MQTT, no Home Assistant credential on the Dot.
- The Dot reports presses and holds of its buttons, reports what it can read
  about itself, and chimes on every press it takes.
- It also joins Music Assistant as a Sendspin player, through its own speaker
  or a paired Bluetooth speaker.

## Scope

- This runs on a Dot you own and have already rooted. Nobody supports it, and a
  FireOS update can break any of it.
- **Alexa commands are optional, and they risk the account, not the Dot.** They
  reach an undocumented Alexa app endpoint with a credential that carries the
  whole Amazon account, which may sit outside Amazon's terms. Read
  [Alexa commands](docs/usage.md#alexa-commands) before you build MapDump. Without the jar,
  the daemon does not offer them.
- **Sendspin has no pairing.** The pairing flow is not implemented and the
  fallback key is a published constant, so anything that reaches the Dot on
  `wlan0` can take the session. The session gives playback only; see
  [SECURITY.md](SECURITY.md) for what it holds.
- **Sendspin over Bluetooth needs a strong Wi-Fi link.** The Dot's Wi-Fi and
  Bluetooth share one radio, and a weak link starves the stream (see
  [docs/pitfalls.md](docs/pitfalls.md)). A stream settles in sync about 2
  seconds after it starts. A speaker connecting or disconnecting mid-stream
  costs up to about 1 second of audio.
- Taking the action button takes it from Alexa. While the daemon holds it, the
  button does not stop timers or alarms, talk, or enter setup mode. The
  `Action button mode` select gives it back without stopping anything else.
- Alexa still gets the mute key by default. The volume keys are untouched,
  except while the Dot is muted (see
  [Muting](docs/usage.md#muting-holds-the-volume-keys)).

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
  `/sbin/.core/img/.core/service.d`. `install.py` writes the boot script only
  there, and fails if it cannot. A Magisk that uses `/data/adb/service.d`
  needs the path changed first.
- `adb`, from Android
  [platform-tools](https://developer.android.com/tools/releases/platform-tools),
  and Python 3.9 or later on a macOS, Linux or Windows machine. A
  [release](#install-from-a-release) needs nothing else to install on a rooted
  Dot. Rooting one needs firebreak; see [Rooting a Dot](#rooting-a-dot).
- To build it yourself: Go 1.25 or later, and an **Android NDK**
  (`brew install --cask android-ndk` on macOS, or
  [developer.android.com/ndk](https://developer.android.com/ndk)).
  Set `ANDROID_NDK_HOME` to it.
- Home Assistant on the same subnet as the Dot.

## Rooting a Dot

overdub needs [firebreak](https://github.com/bboe/firebreak)'s default
target, `amonet-biscuit-v1.1.0-bboe`: rooted Fire OS 5.5.5.4 with Magisk 17.3.
[firebreak's README](https://github.com/bboe/firebreak#run-it) has the install
commands for macOS, Linux and Windows. With the Dot on USB:

```sh
uvx firebreak@latest       # rooted Fire OS 5.5.5.4
deploy/install.py kitchen
```

Both directions, filmed from start to finish, the Dot beside the terminal. The
films show the earlier scripts, `dot_root.py` and `dot_restore_stock.py`, which
firebreak replaces:

<a href="https://youtu.be/yB-SI6i5EZc"><img src="https://img.youtube.com/vi/yB-SI6i5EZc/maxresdefault.jpg" alt="Video: rooting a stock Echo Dot to Fire OS 5" width="49%"></a>
<a href="https://youtu.be/hwVIYQENaBY"><img src="https://img.youtube.com/vi/hwVIYQENaBY/maxresdefault.jpg" alt="Video: returning a rooted Echo Dot to stock Fire OS 6" width="49%"></a>

- The rooted Dot finishes in setup mode, with an orange ring and no Wi-Fi.
  Home Assistant reaches it only over Wi-Fi, so add it in the Alexa app. That
  is safe once rooted: firebreak hides the updater and blocks the update hosts.
- `amonet-biscuit-v1.1.0` can also host overdub. `amonet-biscuit-v2.0.0` and
  `stock` leave Fire OS 6, which overdub does not run on.
- `deploy/dot_firmware.py` was the rooting script. It now prints the firebreak
  command for its arguments, and runs it.

### macOS

Moved to [macOS](https://github.com/bboe/firebreak#macos) in firebreak's
README.

### Linux (Debian, Ubuntu)

Moved to [Linux](https://github.com/bboe/firebreak#linux) in firebreak's
README.

### Windows

Moved to [Windows](https://github.com/bboe/firebreak#windows) in firebreak's
README.

## Install from a release

Each [release](https://github.com/bboe/overdub/releases) carries one tarball
with the scripts, the binary and `mapdump.jar`, already built:

```sh
tar xf overdub-v1.0.0.tar.gz
overdub-v1.0.0/deploy/install.py kitchen
```

On Windows, in PowerShell:

```powershell
tar xf overdub-v1.0.0.tar.gz
py -3 overdub-v1.0.0\deploy\install.py kitchen
```

The installer prints one line a step and the encryption key for Home Assistant.
[docs/usage.md](docs/usage.md) shows its output, how to verify a release, and
how to build and install from source.

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

[docs/usage.md](docs/usage.md) lists every entity and covers the button modes,
playing audio, Alexa commands, network adb, uninstalling and troubleshooting.

## Documentation

`docs/` records what was measured on the hardware and what each decision
defends against.

- [Using overdub](docs/usage.md): installing, the entities, uninstalling,
  troubleshooting
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
- [Rooting](https://github.com/bboe/firebreak/blob/main/docs/rooting.md):
  firebreak's record of each step, in its own repository

## Licence

- BSD 2-Clause; [LICENSE.txt](LICENSE.txt) carries the terms.
- The chime is original to this repository and covered by the same licence.
  `internal/audio/chime.go` renders it at startup from 2 sine tones, 880 Hz
  then 1320 Hz.
- This repository contains no Amazon code. The Amazon names it carries identify
  things already on the device, so this software can interoperate with them.
- overdub is not affiliated with, endorsed by, or supported by Amazon.
