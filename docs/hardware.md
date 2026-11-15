# Hardware

## How the device is wired

biscuit, the Echo Dot (2nd Generation), `ro.product.model` `AEOBC`:

| Node | Name | Keycodes |
|---|---|---|
| `/dev/input/event1` | `mtk-kpd` | **138 = action ("dot")**, 113 = mute |
| `/dev/input/event2` | `keys` (gpio-keys) | 115 volume up, 114 volume down |

radar, the Echo (2nd Generation), `ro.product.model` `AEORD`, has biscuit's
layout and its `pcm23p`: main-bedroom is one.

donut, the Echo Dot (3rd Generation), `ro.product.model` `AEODN`:

| Node | Name | Keycodes |
|---|---|---|
| `/dev/input/event1` | `gpio-privacy` | 113 = mute |
| `/dev/input/event3` | `gpio-keys` | **138 = action**, 115 volume up, 114 volume down |

- The numbers differ and so does the grouping, so no node is named in the
  binary. The daemon finds each by the keycode it declares, reading
  `EVIOCGBIT`, and skips a node whose `/sys/class/input` link runs through
  `/devices/virtual/`: a clone, the daemon's or a leftover, and `hwmdata`.
- The daemon acts on 138 and 113. It grabs the node carrying 138, and clones it
  under the node's own name, read with `EVIOCGNAME`.
- While the Dot is muted the volume keys are held, so a press lifts the mute
  rather than moving a level nobody can hear. The node is the one declaring
  both 114 and 115. On biscuit that is `event2`, grabbed only while muted.
  On donut it is the action node, which is grabbed already, and a second grab
  of a node fails; so the read loop swallows them while muted.
- Both keys, and the node they come from, decide it, not what the action node
  declares. biscuit's `mtk-kpd` declares 114 as well as 113 and 138, and no
  key arrives on it but those two.
- The node is looked for at the first mute, and again at each mute until it
  is found, without a wait: the lookup runs under the mute's lock.
- A swallowed key-down has its key-up swallowed too, even after the mute
  lifts, so the clone never sees a stray up.
- Every auto-repeat is swallowed while the keys are held, a key already down
  when the mute began included: a repeat moves the level. That key's release
  passes, because the clone saw its down. A grab of its own node gives the
  same result on biscuit.
- donut's 113 is on a node of its own, `gpio-privacy`, the hardware privacy
  switch. The daemon reads it without a grab, so the mute button there offers
  `monitor` and `pass through` only: nothing can keep a key from a node no one
  holds.
- A read that fails there stops that watch and is logged; the daemon runs on.
  Exiting would make a lasting error a restart loop, and each restart drops
  the action button's grab.
- The board is `ro.product.device` up to its `_`: `biscuit_puffin` is
  `biscuit`. It names the model Home Assistant shows, and `board=` in the mDNS
  record: `Echo (2nd Generation)` for radar. An unknown board is shown as `Echo (<board>)`.
- The clone advertises every keycode the action node declares, because the input core
  drops events for a keycode the device has not claimed. It declares `EV_KEY`
  alone, so other event types are dropped rather than re-emitted.

## The privacy driver

- biscuit's and radar's is `/sys/devices/soc/10010000.keypad/amz_privacy/`.
  `enable` is writable by `ace_button_mgr`, and `acebuttond` writes it at its
  start to restore the state it saved. `state` and `privacy_state` read 1 on
  main-bedroom, whose microphone was off.
- **Never read `power_button_state` there.** It is world-readable, and a read
  panics the kernel: biscuit has no power-button GPIO, and the handler
  dereferences NULL. A `for f in */; do cat` over the directory rebooted
  overdub-test.

```
Unable to handle kernel NULL pointer dereference at virtual address 0000004c
PC is at gpiod_get_raw_value+0x10/0x44
LR is at get_power_button_state+0x20/0x3c
Kernel panic - not syncing: Fatal exception
```

## Testing on hardware

A live Echo in someone's home: audio makes noise, and a press may drive real
automations. Prefer targeted tests.

The API without Home Assistant, through the library Home Assistant uses. The
forward rides the stock chain's loopback accept, so it needs no LAN route and
no firewall rule. Without `noise_psk` the client gets
`RequiresEncryptionAPIError`.

```sh
adb forward tcp:16053 tcp:6053
pip install aioesphomeapi
adb shell cat /data/local/bin/.overdub-noise-key   # the key it needs
# APIClient("127.0.0.1", 16053, None, noise_psk=<that key>)
```

A button press on biscuit or radar, whose action key is on `event1`; donut's
is on `event3`. Then the volume and mute as `internal/device` reads them:

```sh
adb shell 'sendevent /dev/input/event1 1 138 1;
  sendevent /dev/input/event1 0 0 0;
  sendevent /dev/input/event1 1 138 0;
  sendevent /dev/input/event1 0 0 0'
adb shell lipc-get-prop -i -q com.doppler.audiod MainVolume
adb shell lipc-get-prop -i -q com.doppler.audiod Mute
```

- `input keyevent` is there but cannot run: it is a script that starts
  `app_process`, which Fire OS 6 lacks.

## What a held key is worth

Measured on Fire OS 5, where Alexa's key handling was an APK. Fire OS 6's
`acebuttond` holds it now, and its holds are not measured; treat these as the
hazard until they are.

Alexa's key handling is in
`/system/priv-app/SpeechInteractionManager/SpeechInteractionManager.apk`. Two
assets say what a hold does.

`assets/keyConfig/key_config.json`:

```json
keys:    uber=110, mute=91, volumeUp=24, volumeDown=25
keySets: "uber" = [uber],  "factoryReset" = [mute, volumeDown]
```

`assets/factoryResetConfig/factory_reset_key_press_value.json` binds resets to
those sets by `KeyState` ordinal, where the enum runs `UNKNOWN, PRE_DOWN, DOWN,
UP, SHORT, LONG, VERY_LONG, SUPER_LONG, EXTREME_LONG, REPEAT`:

| state (ordinal) | hold | what fires |
|---|---|---|
| `VERY_LONG` (6) | 5 seconds | no reset |
| `SUPER_LONG` (7) | 8 seconds | `advancedReset`: **mute + volume down** |
| `EXTREME_LONG` (8) | 20 seconds | `reset`: action alone |

- The holds come from the `timeInMs` each `KeyListener` log line carries. Read
  them there, not from wall-clock time around a test.
- A stranded action button wipes the Dot 20 seconds later.
- A stranded mute is half the advanced-reset combo from 8 seconds. The volume
  half needs a hand while the Dot is unmuted; while muted the daemon holds the
  volume keys, so the combo cannot complete. "How the device is wired" has the
  node.
- Mute alone is safe to hold. `MuteButtonHandler.onButtonPress` has no reset
  path, and its one long-hold branch needs
  `hasSystemFeature("com.amazon.edge.enable_toggle_offline")`, which this Dot
  lacks.
- A key left down lasts as long as the daemon. Killing the daemon removes the
  input device, and the clone the supervisor builds starts with no key down.
  `sendevent ... 1 113 0` clears it at once.
- `pkill -f` works on Fire OS 6's toybox, and matches the `adb shell` command
  line that runs it: a pattern spelled out in that command kills its own
  shell. Fire OS 5's toolbox `pkill -f` did nothing and said nothing. Either
  way, take the pid from `ps` and check that it changed.

## What the device says about itself

```sh
# daemon log, truncated at boot and every 20 restarts
adb shell cat /data/local/tmp/overdub.log
# Alexa's audio focus, live: logd prunes her lines before a dump reads them
adb shell 'logcat -v brief -s PuffinApp | grep FocusManager'
# the rule, and its packet count
adb shell 'iptables -L INPUT -n -v | grep 6053'
# names, handlers, key bitmaps
adb shell cat /proc/bus/input/devices
# events, without grabbing
adb shell getevent
```
