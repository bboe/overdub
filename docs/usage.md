# Using overdub

README.md roots a Dot, installs a release and adds it to Home Assistant. This
page covers the rest: what the installer prints, building from source,
uninstalling, every entity, and troubleshooting. Why each part works the way it
does is on the other pages in this directory.

## What the installer prints

Each step prints one line, and ✅ means it was read back off the Dot:

```
Installing overdub as kitchen on <serial>.

✅ build          prebuilt build/overdub, md5 63f1e442a0522439eea42570d30db241
✅ root           adb runs as root
✅ API key        kept the one already on the device
✅ binary         md5 63f1e442a0522439eea42570d30db241
✅ key directory  /data/local/bin is drwx------
✅ boot script    /data/local/bin/overdub.sh
✅ init           /system/etc/init/overdub.rc
✅ daemon         restarted as -name kitchen, pid 364 -> 2606
✅ pairing        read the Sendspin key

   To pair Music Assistant with this device, paste this token into
   its Sendspin player settings. Anyone holding it can pair a
   server with this device:

       SP:0...

Installed. Home Assistant must be able to reach this device on tcp/6053;
README.md says how.
```

- ❗ marks something to act on, such as a reboot. ❌ marks a failure, and the
  script stops there.
- Where the terminal cannot show them, the marks print as `OK`, `!!` and `XX`.

## Verifying a release

`SHA256SUMS` sits beside the tarball, and the build is attested:

```sh
shasum -a 256 -c SHA256SUMS      # or sha256sum -c
gh attestation verify overdub-v1.0.0-fireos6.tar.gz --repo bboe/overdub
```

A released binary reports its version. A local build prints
`overdub (unversioned build)`:

```sh
adb shell '/data/local/bin/overdub -version'
```

The Dot's linker prints 4 `WARNING: linker:` lines first, merged into the same
stream. The version is the last line:

```
overdub v1.0.0
```

- The tarball carries no `build.sh`. `install.py` builds when it finds
  `build.sh`, and otherwise installs `build/overdub` as it stands.
- `build.sh` runs on macOS and Linux only. On Windows, `install.py` refuses a
  source tree; install from a release.
- Build it yourself to change anything, or to run only a binary you compiled.

## Building from source

This needs the repository, not the tarball.

```sh
./build.sh
```

- `build.sh` sets `GOOS=android GOARCH=arm GOARM=7`, finds the NDK compiler,
  and makes the empty `libpthread` and `librt` stubs cgo needs. Neither target
  setting is optional; [constraints.md](constraints.md) says why.
- If it cannot find the NDK, it says so. Set `ANDROID_NDK_HOME`.

## Installing from source

```sh
deploy/install.py kitchen                          # binary, boot script, key
```

- Running it again is safe. It pushes only what differs from the Dot, and
  restarts the daemon only when what runs may differ from what is installed.
  With nothing to change it ends `Already installed; nothing changed.`
  If it also warned, it ends `Nothing changed; see the warning above.`
- With more than one Dot on `adb`, set `ANDROID_SERIAL`:

```sh
adb devices                                   # serials
ANDROID_SERIAL=<serial> deploy/install.py kitchen
```

What goes where:

- `overdub`: `/data/local/bin/`
- `overdub.sh` (the boot script): `/data/local/bin/`
- `overdub.rc`: `/system/etc/init/`, the init service that runs the boot
  script at boot
- `.overdub-noise-key`: `/data/local/bin/`, mode 600, generated if absent

The name:

- **Required, and unique on your network.** Home Assistant prefixes every
  entity id with it. A second Dot under a name in use stops at a conflict menu.
- Lowercase letters, digits, `-` and `_`; at most 63 characters; not starting
  or ending with `-`. These are ESPHome's rules, and `install.py` enforces them.
- Home Assistant identifies the device by its MAC, so a rename is accepted on
  the next connection. Entity ids keep the prefix they were created with.
- A rename restarts the init service, so the new name runs at once.
  `install.py` prints `REBOOT REQUIRED` only if init has not read
  `overdub.rc` yet.
- `<name>` below is the name **Home Assistant** knows the device by. If you
  rename the device there, the entity ids can follow, while the daemon log
  keeps the `-name` it was given.

The first install ends asking for a reboot: init reads `overdub.rc` only at
boot. After that, `install.py` starts or restarts the daemon itself. It is
supervised. If it dies, the grab is released and the action button goes back
to Alexa.

A daemon start usually plays Alexa's microphone-on sound once, or
microphone-off when the microphones are muted. The daemon restarts Alexa's key
reader, and the reader announces the microphone state as it starts.
docs/button.md says why.

## Running it by hand

The boot script runs the daemon. To run it by hand, as root, by full path:

```sh
adb shell '/data/local/bin/overdub -name kitchen'
```

| Flag | |
|---|---|
| `-name` | **required**: the unique device name Home Assistant knows |
| `-version` | print the version and exit |

Everything else is fixed in the binary or read from the Dot: keycodes 138 and
113, found on whichever input node declares them, the clone named after that
node, `wlan0`, tcp/6053 and tcp/8928.

## Alexa's session

Alexa's session runs through the daemon on its way to Amazon. Nothing turns this
on or off: it happens on every Dot that installs the binary.

Everything in the session is passed on unchanged, so Alexa answers as she did,
and the daemon reads nothing out of what goes by.

Three things change on the device, and `uninstall.py` undoes all three:

| what | where |
|---|---|
| where Alexa sends her session | `com.amazon.puffin AVSEndpoint`, over LIPC |
| two files the daemon added | under `/system/etc/security` |
| what it made for itself on first run | `/data/local/bin/.overdub-avs-identity` |

Nothing has to be put on the Dot for this. The log says it is up:

```
voice: Alexa's session goes through 127.0.0.1:8443 to https://avs-alexa-14-na.amazon.com
```

**If the daemon is not running, Alexa cannot answer.** The endpoint still points
at it. The boot script restarts a daemon that died 5 seconds later, and an
uninstall points Alexa back at Amazon whatever state the daemon is in.

## Uninstalling

```sh
deploy/uninstall.py
```

- It removes the boot script first. A reboot part way through leaves nothing
  running, not a supervisor respawning a half-deleted install.
- The daemon gets `SIGTERM`, so it releases the button and destroys its uinput
  clones.
- It removes the boot script, the binary, the API key, the Sendspin identity,
  its pairing records and the last server to play, the log, and every
  `persist.overdub.*` property. A new install starts with Sendspin on
  and no output delay, and voice in `pass through`.
- `/data/local/bin` goes too if it is then empty.
- The tcp/6053 and tcp/8928 rules, and the loopback rules for tcp/8443, go
  once the daemon is confirmed gone, so no reboot is needed. An uninstall that
  reports trouble stops before this step.
- Uninstalling revokes nothing; see [Alexa commands](#alexa-commands).
- The Sendspin identity matters as much as the API key: the pairing token
  derives from it, so a copy left behind stays valid. A server that paired
  keeps its record; remove the player there too.
- Alexa is pointed back at Amazon, and the two files the daemon added under
  `/system` are removed. Both are read back; see
  [Alexa's session](#alexas-session).
- Nothing else of Amazon's is touched. Delete the device in Home Assistant
  when done.
- A later install generates a new API key. The adb keys set from Home
  Assistant go too; the key of the computer that rooted the Dot, which
  firebreak wrote to `/adb_keys`, still opens adb.
- Running it again is safe. With nothing left it ends `Already uninstalled;
  nothing to remove.`

## Home Assistant

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
| sensor | `volume` | diagnostic | %, 0 while muted |
| sensor | `cpu_temperature` | diagnostic | °C, the SoC's thermal zone |
| sensor | `memory_available` | diagnostic | MiB an allocation could get |
| binary_sensor | `audio_jack` | diagnostic | on: a plug in the socket |
| sensor | `output_device` | diagnostic | `speaker`, `jack`, `bluetooth` (5) |
| sensor | `bluetooth_device` | diagnostic | the connected speaker (5) |
| sensor | `playing` | diagnostic | what is playing, or empty (6) |
| sensor | `alexa_reply` | -- | what Alexa last said, in `monitor` (13) |
| binary_sensor | `alexa_registered` | diagnostic | on: has an account (7) |
| select | `action_button_mode` | config | starts `intercept` (8) |
| select | `mute_button_mode` | config | starts `monitor` (8) |
| text | `alexa_command` | config | run as though spoken (9) |
| switch | `sendspin` | config | on: Sendspin is up (10) |
| number | `sendspin_output_delay` | config | 0 to 5,000 ms (11) |
| select | `voice_mode` | config | starts `pass through` (13) |
| update | `firmware` | config | the latest release (12) |
| button | `restart` | config | restarts the Dot (14) |
| assist_satellite | `assist_satellite` | -- | answers in `intercept` (15) |

1. See [The action button](#the-action-button).
2. Volume and mute, and playback of any URL Home Assistant can transcode.
3. Turning it on mutes the microphones as the button does, ring included.
   Turning it off is refused: only the button turns them back on.
4. A reading that is not a signal is missing, not zero.
5. Bluetooth that carries no audio, such as the Alexa app over BLE, does not
   count. `bluetooth_device` is the speaker's name, or its address when the Dot
   has no name for it, and empty when no speaker is connected.
6. The foreground one of `alert`, `communications` and `dialog` (her speech,
   not her listening), then `clip` (from Home Assistant), `content` (her
   music) and `sendspin`. Earcons and chimes are not counted. It changes as
   the sound does, over the speaker, the jack or Bluetooth. Sound from
   anything else, such as a phone playing to the Dot over Bluetooth, is not
   seen. It replaces the `speaker_playing` binary sensor: move an automation
   on that to this one, and delete the old entity.
7. Off on a Dot never set up, or deregistered; everything else still works.
8. `intercept`, `monitor` or `pass through`. The mute button has no
   `intercept`.
9. Listed only while the Dot is registered.
10. Off withdraws the mDNS advert, closes tcp/8928, deletes its firewall rule
    and ends any session. Survives a reboot.
11. How far ahead of a chunk's timestamp the Dot plays. A music server can set
    it too; the last write wins, and the number shows what is applied.
    Survives a reboot and works with the switch off.
12. Listed only for a release build. See [Updating](#updating).
13. `intercept`, `monitor` or `pass through`. In `intercept`, Home
    Assistant's Assist answers everything said to the Dot, and Amazon hears
    none of it. So nothing Alexa does by voice works in it, online or
    offline: her timers, alarms, reminders, music and smart-home control,
    including what her on-device engine runs without the internet. Timers do
    not work through Home Assistant yet either. If Home Assistant is not
    connected, or its pipeline fails, the turn ends with nothing said: Alexa
    never answers in `intercept`. In `monitor`,
    `alexa_reply` takes each part of Alexa's answer as she says it, so its
    history reads as the whole answer. A part over 255 bytes arrives in
    pieces. The other modes empty it. Amazon does not send what you asked, so
    only the answer is there. The mode survives a reboot.
14. It restarts the whole Dot, Alexa and adb included, not only overdub.
    overdub gives Alexa back her endpoint and lets go of the buttons, then
    asks init to reboot. The Dot is back on the network in about a minute. If
    the Dot is still up 30 seconds later, overdub logs it and restarts
    overdub. It is the way back when adb stops answering; docs/device.md has
    why.
15. Home Assistant adds it, and its own selects with it: `assistant`,
    `assistant_2`, `finished_speaking_detection`, `wake_word` and
    `wake_word_2`. The wake word selects read unavailable: Alexa hears the
    wake word, and the Dot offers Home Assistant none to choose. `assistant`
    picks the pipeline `intercept` uses; `assistant_2` is unused, and can be
    disabled.

The Sendspin entities are listed only when the Dot could read or create its
Sendspin identity.

### How often it reads

| every | reads | when |
|---|---|---|
| 60 seconds | uptime, signal | always |
| 60 seconds | registration, Bluetooth device | subscribed |
| 2.5 seconds | volume, jack, route, temperature, memory, mic | subscribed |
| 500 ms | whether the speaker is playing | subscribed |

- The first subscriber wakes both polls. The registration has no state on the
  first connect after a restart until that reading arrives.
- A speaker connecting or disconnecting reads the Bluetooth device again within
  a few seconds.
- A value is sent only when it changes.

### Volume

- A volume change appears within a few seconds, whether it came from the keys,
  an app or Alexa.
- Setting it from Home Assistant sets Alexa's own level, so the Alexa app and
  "Alexa, volume up" agree with it. Each set plays Alexa's volume sound and
  lights the ring, as a key press does.
- Music Assistant can set it over Sendspin too.
- Sendspin ducks to 20% while Alexa listens and answers, and comes back when
  she is done. docs/audio.md has how.
- The Dot keeps a level per output, and `volume` is the live one's. Plugging
  or unplugging the jack switches to that output's level. Muted, `volume`
  reads 0.
- Plugging in a cable disconnects a paired speaker, and it does not reconnect
  when you pull the cable. Connecting a speaker with a cable already in works:
  the Dot plays to the speaker.

#### Muting holds the volume keys

- A volume key does not lift the mute: it moves the level in silence. So while
  the Dot is muted, the daemon grabs the volume keys.
- The first key press lifts the mute and moves nothing. The next moves the
  volume.
- A Home Assistant slider moved while muted sets the level and leaves the mute
  on.
- While muted, nothing else on the Dot sees a volume key. Alexa's advanced
  factory reset (mute and volume down held) cannot complete.

### Playing something on it

`media_player.play_media` and `tts.speak` play through the daemon. Home
Assistant transcodes the media to FLAC and the Dot fetches it from Home
Assistant, so **the Dot must be able to reach Home Assistant's port**, 8123 by
default. A Dot on an isolated network often cannot, even though Home
Assistant reaches the Dot. Ask the Dot:

```sh
adb shell 'toybox nc -w 3 <home assistant address> 8123 </dev/null && echo open'
```

```yaml
action: tts.speak
target:
  entity_id: tts.home_assistant_cloud
data:
  media_player_entity_id: media_player.kitchen_speaker
  message: "The back door has been open for 10 minutes"
```

- An announcement, which is what `tts.speak` sends, plays over a Sendspin
  stream and lowers it to 20% meanwhile.
- Anything else stops a Sendspin stream, as Alexa's own music does.
- `media_player.media_stop` ends a clip. A new one replaces the old.
- When nothing plays, the daemon log says why: `playback: clip: HTTP 404`,
  `playback: clip: fetch: dial tcp ...: connect: connection refused` for a Dot
  that cannot reach Home Assistant, or `the reply is not FLAC`.

While Alexa listens or speaks, a Sendspin stream drops to 20%, and to 10%
while an alarm or timer rings. When she plays music herself, the Dot leaves
Sendspin play: Music Assistant stops it, or plays on without it in a group.
It does not resume when she stops; press play again.

### The button modes

Each button has a mode select:

| mode | Alexa | Home Assistant |
|---|---|---|
| `intercept` | nothing | events |
| `monitor` | answers the press as usual | events |
| `pass through` | answers the press as usual | nothing |

- The action button starts in `intercept`. Mute starts in `monitor`.
- The mute button has no `intercept`. The microphones go off at the press,
  before the daemon sees it.
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

A registered Dot gets a text box and a matching action. Either runs text on
the Echo as though spoken:

```yaml
action: esphome.kitchen_send_command
data:
  text: play dance party music on Amazon Music
```

- Both appear only when the Dot is registered to an Amazon account. Otherwise
  neither is listed, and the daemon log says why. There is no other switch:
  a registered Dot always offers them.
- If the registration cannot be read, the box is offered.
- The box shows the last command it was given.
- The daemon checks again every 5 minutes, so the box appears without a restart
  once the Dot is registered.
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
> The daemon reads the OAuth refresh token this Echo was registered with out
> of its own store, and trades it for cookies scoped to `.amazon.com`: `at-main`,
> `sess-at-main`, `session-id`, `session-token`, `ubid-main` and `x-main`. That
> is a signed-in retail session, order history and addresses included, the same
> breadth [alexa_media_player](https://github.com/alandtse/alexa_media_player)
> gets from a login. It is not AWS. The token and cookies stay in memory, and
> the API key is the only lock on the port that reaches them.
>
> Watch for the Dot quietly deregistering, not for an error. To revoke,
> deregister the Dot in the Alexa app or under Manage Your Content and Devices.
> Uninstalling revokes nothing already extracted.

### Network adb

adb over tcp/5555 is always on, and always asks for a key: firebreak's unlock
sets it up that way at every boot, and overdub keeps the port open in the
firewall. There is nothing to switch.

- Connect with `adb connect <address>:5555`. `ANDROID_SERIAL` then picks it for
  `install.py`.
- The computer that rooted the Dot always gets in: firebreak wrote its public
  key to `/adb_keys` on the system partition, which nothing here changes, so
  no call can lock you out, and none can revoke that key.
- To let another machine in, give the Dot its public key from Home Assistant:
  **Developer Tools → Actions → `ESPHome: <name>_set_adb_keys`**, and paste
  the contents of that machine's `~/.android/adbkey.pub` into `keys`, one key
  per line.
- The call replaces the whole list. Send every key you want kept; send `none`
  to remove them all. An empty `keys` is refused. No restart is needed.
- The `adb keys` diagnostic sensor shows who can connect: the rooting
  computer's key, marked 🔒, then the keys set here, by the names at
  the end of each line, separated by commas. After a refused call it starts with the reason and the line it
  stopped at.

#### A Dot rooted before firebreak v1.2.0

If `install.py` warns `ro.adb.secure is not 1`, firebreak did not lock adb on
this Dot. **Any host on its network gets a root shell,** and keys set from Home
Assistant do nothing. Lock it like this, from the computer that rooted it,
whose `~/.android/adbkey.pub` becomes the one key. docs/device.md says why
each step is shaped this way.

```sh
adb push ~/.android/adbkey.pub /data/local/tmp/adbkey.pub
adb shell
```

Then, on the Dot:

```sh
k=/data/local/tmp/adbkey.pub; t=/data/local/tmp/default.prop.new
lock() {  # $1: the slot's root, empty for the running one
  sed 's|^ro.adb.secure=.*|ro.adb.secure=1|' $1/default.prop > $t
  grep -qx ro.adb.secure=1 $t || echo ro.adb.secure=1 >> $t
  cat $t > $1/default.prop
  cat $k > $1/system/adb_keys.new
  chmod 0644 $1/system/adb_keys.new
  chcon u:object_r:adb_keys_file:s0 $1/system/adb_keys.new
  mv $1/system/adb_keys.new $1/adb_keys
  sync
  grep -x ro.adb.secure=1 $1/default.prop
  cmp $k $1/adb_keys && ls -lZ $1/adb_keys
}
mount -o rw,remount / && lock ""; mount -o ro,remount /
o=b; [ "$(getprop ro.boot.slot_suffix)" = _b ] && o=a
m=/data/local/tmp/other; mkdir -p $m
mount -t ext4 /dev/block/by-name/system_$o $m && lock $m; umount $m; rmdir $m
rm -f $k $t
```

Each slot must print `ro.adb.secure=1` and
`-rw-r--r-- ... u:object_r:adb_keys_file:s0 ... adb_keys`. **Do not reboot
otherwise:** with the property set and no key adbd can read, adb is shut, and
the way back is firebreak's gesture into TWRP. After `adb reboot`, this
computer gets back in and any other gets `unauthorized`.

> **A key is a root shell.** adbd runs as root on this build. Anyone with the
> Dot's Home Assistant API key can install an adb key. Do not expose tcp/5555
> beyond your own network.

### Pairing with Music Assistant

- **Music Assistant 2.10.9 or later pairs the Dot by itself** through Home
  Assistant 2026.11 or later, once both have the Dot. Home Assistant reads the
  token over the API and hands it to Music Assistant, which pairs the Dot
  whenever it is connected unpaired and Home Assistant has announced it.
- This needs Music Assistant's **Home Assistant plugin** (Settings →
  Plugins), connected as a Home Assistant administrator: Home Assistant serves
  the token only to an admin. Without the plugin nothing is logged; the player
  just waits in Music Assistant as needing setup. The daemon logs `paired with`. Paste the
  token by hand only where that does not happen.
- Every install prints the pairing token once the daemon has made its
  Sendspin key. A first install may print a warning instead; install again
  once the daemon runs.
- Paste the token into Music Assistant where it offers to pair the player.
  The daemon logs `paired with`, and its next connection logs `complete on the
  lt psk`.
- **Treat the token as a password.** It never changes, and it pairs any server
  that holds it. The installer keeps no copy, and Home Assistant stores none.
  To rotate it, delete
  `/data/local/bin/.overdub-sendspin-key` and restart the daemon. Every server
  then sees a new player, and the old pairing records go unused.
- **The Dot plays only for a paired server.** It offers no unpaired access,
  so Music Assistant's guest access does nothing for it. An unpaired Music
  Assistant stays connected with no role and plays nothing.
- Unpairing in Music Assistant while the Dot is connected and paired removes
  the record on the Dot as well. Otherwise the Dot keeps a record no server
  uses, until 9 newer pairings evict it.
- After an unpair, Music Assistant does not reconnect by itself. When the
  daemon next starts, Music Assistant connects again, unpaired, and plays
  nothing until you pair the Dot again.

### Updating

- The `firmware` update entity shows the newest overdub release on GitHub that
  this Dot can run. A newer release it cannot run is shown only when nothing
  it can run is newer, with a line at the top of its release notes saying why
  and what to do instead. **Install** refuses it, and that line then starts
  with **Install failed**.
- **Install** downloads the release, checks it, and replaces the binary. The
  daemon then restarts into the new release, which takes about 7 seconds.
- The Dot looks for a release a minute after it starts, then every 6 hours.
  Home Assistant's **Check for updates** looks again, at most once a minute.
- A Dot on v0.3.0 is offered a Fire OS 6 release, and its notes start with
  **Fire OS 5 Dots**. **Install** fails, normally with "this release changes
  the boot script, which an update does not replace; run its install.py
  instead". Run `install.py` with the Dot on USB: it moves the Dot to Fire OS
  6 with firebreak, keeps its setup and keys, and then installs. Earlier
  releases have no updater.
- An update installs only a release signed with the overdub release key. A
  release that changes the boot script cannot install this way: the entity's
  release notes then say to run `install.py` from that release.
- A release is on trial until Home Assistant has been connected to it for a
  minute. **Install** works again after that.
- If the new release exits 3 times during its trial, the Dot goes back to the
  release it replaced. The log says `the update to <version> exited 3 times`,
  and the entity offers the release again, with a line at the top of its
  release notes saying it failed.
- The first release that has this entity needs `install.py` and then one
  reboot. Until that reboot, **Install** is refused, and the release notes say
  why.
- A build from source has no version, so it lists no update entity.

### Encryption

- The API speaks only ESPHome's `Noise_NNpsk0_25519_ChaChaPoly_SHA256`. There
  is no plaintext mode.
- `deploy/install.py` generates the key when the Dot has none, and prints it
  once:

```
✅ API key        generated, and verified on the device

   Paste this key into Home Assistant's ESPHome integration.
   The installer keeps no copy:

       kR2b...
```

- Keep it. A reinstall keeps an existing key, so Home Assistant stays paired.
  The installer does not accept a key of your own.
- To rotate: delete `/data/local/bin/.overdub-noise-key` on the Dot, install
  again, and paste the new key into Home Assistant.

## Troubleshooting

```sh
adb shell 'cat /data/local/tmp/overdub.log'      # emptied when the service starts
adb shell 'iptables -L INPUT -n -v | grep 6053'  # packet counter
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
  `no such file or directory (deploy/install.py generates one)`**: there is no
  key. Rerun `deploy/install.py <name>`.
- **Nothing starts; the log says the key `decodes to N bytes`**: the key is
  corrupt. A reinstall keeps it, so delete it first:
  `adb shell 'rm -f /data/local/bin/.overdub-noise-key'`
- **Nothing starts; the log says `NAME is unset`**: the boot script was
  installed by hand. Rerun `deploy/install.py <name>`.
- **The mute button offers no intercept**: on every model. The press mutes the
  microphones in hardware, before the daemon sees it.
- **Every keycode looks wrong**: the build. `GOARCH=arm` is required.
- **The button does not chime**: the mode first; only `intercept` chimes. Then
  the daemon log: `presses will be silent` means the audio player did not
  start. It needs only the device's own `libmixerAPI.so`, from Fire OS 6.
- **The button does nothing**: the `Action button mode` select, then the daemon
  log, which names the address that changed it.
- **The button rings Alexa instead of chiming**: the mode is `pass through` or
  `monitor`.
