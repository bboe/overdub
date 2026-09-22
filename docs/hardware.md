# Hardware

## How the device is wired

| Node | Name | Keycodes |
|---|---|---|
| `/dev/input/event1` | `mtk-kpd` | **138 = action ("dot")**, 113 = mute |
| `/dev/input/event2` | `keys` (gpio-keys) | 115 volume up, 114 volume down |

- The daemon acts on 138 and 113. It grabs `event2` only while the Dot is
  muted, so a volume press then lifts the mute rather than moving a level
  nobody can hear.
- The clone advertises every keycode `event1` declares, because the input core
  drops events for a keycode the device has not claimed. It declares `EV_KEY`
  alone, so other event types are dropped rather than re-emitted.
- `system_server`'s `EventHub` reads `event1`, and
  `/system/usr/keylayout/mtk-kpd.kl` maps 138 to `BUTTON_MODE` and 113 to
  `MUTE`. An AccessibilityService with `FLAG_REQUEST_FILTER_KEY_EVENTS` could
  consume `KEYCODE_BUTTON_MODE` with no root. The evdev route needs no APK and
  no accessibility grant.

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
adb shell 'su -c "cat /data/local/bin/.overdub-noise-key"'   # the key it needs
# APIClient("127.0.0.1", 16053, None, noise_psk=<that key>)
```

A button press, and the volume as `internal/device` reads it:

```sh
adb shell 'su -c "sendevent /dev/input/event1 1 138 1;
  sendevent /dev/input/event1 0 0 0;
  sendevent /dev/input/event1 1 138 0;
  sendevent /dev/input/event1 0 0 0"'
adb shell 'su -c "dumpsys audio"' |
  sed -n '/^- STREAM_MUSIC:/,/^- STREAM_ALARM:/p'
adb shell 'su -c "input keyevent 25"'    # volume down one step; 24 is up
```

## The stream mute

- `Mute count` moves only when something calls `setStreamMute`. Nothing on the
  device does that unprompted: "Alexa, mute" sets the speaker level to 0 and
  leaves every count at 0, `input keyevent 164` does nothing, and stepping below
  zero clamps. This fits API 22, which has no `ADJUST_TOGGLE_MUTE`.
- `mute affected streams` includes `STREAM_MUSIC`, so the call takes.
- The mute belongs to the stream, not a route: one `Mute count` per stream, one
  index per device. Muting leaves every index in place (`speaker: 7,
  headset: 13, headphone: 21` under `Mute count: 1`), and the mute holds across
  the jack. Routes keep separate levels, so unmuting returns to the live one.
- A volume key pressed while the stream is muted does not lift the mute and
  destroys the level. Muted at step 5, one press leaves `Mute count` at 1 and
  the stored level at **1**: the muted index reads 0 and the press moves up from
  there. This is why the daemon grabs `event2` while it holds a mute;
  docs/api.md has the rest.
- A volume key pressed while *Alexa*-muted lifts that mute and restores a level,
  so a probe that presses one changes state. That is Alexa's level-0 mute, not
  the stream mute.

## What a held key is worth

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
  half needs a hand while the Dot is unmuted; while muted the daemon holds
  `event2`, so the combo cannot complete.
- Mute alone is safe to hold. `MuteButtonHandler.onButtonPress` has no reset
  path, and its one long-hold branch needs
  `hasSystemFeature("com.amazon.edge.enable_toggle_offline")`, which this Dot
  lacks.
- A key left down lasts as long as the daemon. `dumpsys input` reports
  `KeyDowns` per device. Killing the daemon removes the input device, and the
  clone the supervisor builds starts with no key down. `sendevent ... 1 113 0`
  clears it at once.
- `pkill -f` does not work on this toolbox and reports nothing when it fails.
  Take the pid from `ps` and check that it changed.

## Binder calls worth knowing

The microphone mute is AudioFlinger's `mMicMute`, and no dumpsys prints it. The
read is `GET_MIC_MUTE`, transaction 18 on `IAudioFlinger`; the reply is one
int32.

```sh
# 00000000 live, 00000001 muted
adb shell 'su -c "service call media.audio_flinger 18"'
# music level on the live route
adb shell 'su -c "service call audio 13 i32 3"'
# its maximum, 30 on biscuit
adb shell 'su -c "service call audio 15 i32 3"'
# set it to 9
adb shell 'su -c "service call audio 4 i32 3 i32 9 i32 0 s16 overdub"'
# master mute, 00000001 is silence
adb shell 'su -c "service call media.audio_flinger 11"'
# lift it
adb shell 'su -c "service call media.audio_flinger 9 i32 0"'
```

- A call costs about 12 ms: 500 calls take 6 seconds.
- The microphone mute fires on the key-**down**. A lone `1` mutes, a lone `0`
  does nothing, and a second `1` while held does nothing.
- `dumpsys audio`'s `Mute count` is not this. It is the per-stream output mute,
  0 whatever the microphone does.
- A silent Dot whose every reading looks right is the master mute. No dumpsys
  on this build prints it: audio reaches the HAL, DL1 prepares and starts, I2S
  enables, the amp pops, and nothing is heard. Counting from `GET_MIC_MUTE` at
  18 gives `SET_MASTER_MUTE` 9, `MASTER_VOLUME` 10 and `MASTER_MUTE` 11. Read it
  first when the Dot goes quiet.
- **Do not find a transaction by calling it.** A blind setter call can zero the
  music volume, mute streams or set the master mute, and none of it announces
  itself. Probe the getters, which answer a level or maximum you already know,
  and derive the setters from the order.
- A mute taken over `service call` is permanent. AudioService releases a stream
  mute or solo when the caller's binder dies, and `service call` passes none.
  `setStreamSolo(3, true)` mutes `RING`, `ALARM`, `NOTIFICATION` and `TTS`
  across a daemon restart; only `setStreamSolo(3, false)` clears them.
- Injecting keycode 113 is a real microphone mute, so it stays until pressed
  again.

## What the device says about itself

```sh
# daemon log, truncated at boot and every 20 restarts
adb shell 'su -c "cat /data/local/tmp/overdub.log"'
# Alexa on a playback
adb shell 'su -c "logcat -d -v brief -s tts-Server tts-Playback"'
# the rule, and its packet count
adb shell 'su -c "iptables -L INPUT -n -v | grep 6053"'
# names, handlers, key bitmaps
adb shell su -c 'cat /proc/bus/input/devices'
# events, without grabbing
adb shell su -c getevent
```
