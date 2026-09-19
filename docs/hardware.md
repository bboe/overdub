# Hardware

## How the device is wired

| Node | Name | Keycodes |
|---|---|---|
| `/dev/input/event1` | `mtk-kpd` | **138 = action ("dot")**, 113 = mute |
| `/dev/input/event2` | `keys` (gpio-keys) | 115 volume up, 114 volume down |

138 and 113 are the codes this project acts on; `event2` is listed for
orientation, and nothing here opens it. The clone advertises every keycode
`event1` declares, because the input core drops events for a keycode it has not
claimed. It declares `EV_KEY` alone, so events of any other type are dropped
rather than re-emitted.

`event1` is read by `system_server`'s `EventHub`, so the action button travels
the ordinary input pipeline, and `/system/usr/keylayout/mtk-kpd.kl` maps 138 to
`BUTTON_MODE` and 113 to `MUTE`. Because it arrives as an ordinary `KeyEvent`,
an AccessibilityService with `FLAG_REQUEST_FILTER_KEY_EVENTS` could consume
`KEYCODE_BUTTON_MODE` with **no root at all**. The evdev route needs no APK and
no accessibility grant.

## Testing on hardware

A live Echo in someone's home: audio makes noise, and a press may drive real
automations. Prefer targeted tests.

**The API, without Home Assistant**, using the library Home Assistant itself
uses. The forward rides the stock chain's own loopback accept, so this needs no
LAN route and no firewall rule. Without `noise_psk` the client gets
`RequiresEncryptionAPIError`: the daemon speaks the encrypted transport and
nothing else.

```sh
adb forward tcp:16053 tcp:6053
pip install aioesphomeapi
adb shell 'su -c "cat /data/local/bin/.overdub-noise-key"'   # the key it needs
# APIClient("127.0.0.1", 16053, None, noise_psk=<that key>)
```

**A button press**, without touching the device:

```sh
adb shell 'su -c "sendevent /dev/input/event1 1 138 1; sendevent /dev/input/event1 0 0 0;
                  sendevent /dev/input/event1 1 138 0; sendevent /dev/input/event1 0 0 0"'
```

**The volume**, as `internal/device` reads it. The whole dump is large, so cut
it to the one stream that matters.

```sh
adb shell 'su -c "dumpsys audio"' | sed -n '/^- STREAM_MUSIC:/,/^- STREAM_ALARM:/p'
adb shell 'su -c "input keyevent 25"'    # volume down one step; 24 is up
```

`Mute count` moves only when something calls `setStreamMute`, which is
transaction 8 on `IAudioService`. Nothing on the device does it on its own:
"Alexa, mute" sets the speaker's level to 0 and leaves every stream's count at
0, `input keyevent 164` does nothing at all, and stepping below zero clamps
rather than muting -- all consistent with API 22, where `ADJUST_TOGGLE_MUTE`
does not yet exist. The dump's `mute affected streams = 0x2e` includes
`STREAM_MUSIC`, which is why the call takes.

**A volume key pressed while the stream is muted does not lift the mute, and
destroys the level.** Measured: muted at step 5, one press left `Mute count` at
1 and the stored level at **1**, because the muted index reads 0 and the press
moves up from there. Unmuting then restores 1. This is why the daemon grabs
`/dev/input/event2` for as long as it holds a mute; docs/sendspin.md carries
the rest.

A volume key pressed while Alexa-muted releases the mute and restores a level,
so a probe that presses one is not a read-only observation of a muted Dot. That
is Alexa's level-0 mute and not the stream mute above, which behaves the other
way.

**What a held key is worth.** Alexa's key handling is in
`/system/priv-app/SpeechInteractionManager/SpeechInteractionManager.apk`, and two
of its assets say what a hold does.

`assets/keyConfig/key_config.json`:

```json
keys:    uber=110, mute=91, volumeUp=24, volumeDown=25
keySets: "uber" = [uber],  "factoryReset" = [mute, volumeDown]
```

`assets/factoryResetConfig/factory_reset_key_press_value.json` binds resets to
those sets by `KeyState` ordinal, where the enum runs `UNKNOWN, PRE_DOWN, DOWN,
UP, SHORT, LONG, VERY_LONG, SUPER_LONG, EXTREME_LONG, REPEAT`:

| entry | key set | state | gesture |
|---|---|---|---|
| `reset` | `uber` | 8 | action button alone, `STATE_EXTREME_LONG` |
| `advancedReset` | `factoryReset` | 7 | **mute and volume down together**, `STATE_SUPER_LONG` |

Thresholds, from the `timeInMs` each `KeyListener` line carries -- read them
from there rather than from wall-clock around the test:

| state | after the key-down |
|---|---|
| `STATE_VERY_LONG` | 5.0s |
| `STATE_SUPER_LONG` | 8.0s |
| `STATE_EXTREME_LONG` | 20.0s |

- A stranded action button wipes the Dot 20s later.
- A stranded mute is half the advanced-reset combo from 8s, and this daemon does
  not grab `event2`, so the volume half comes from the user's own hand.
- Mute alone is inert here and safe to hold: `MuteButtonHandler.onButtonPress`
  has no reset path, and its one long-hold branch is behind
  `hasSystemFeature("com.amazon.edge.enable_toggle_offline")`, which this Dot
  does not have.

A key left down lasts as long as the daemon. `dumpsys input` reports `KeyDowns`
per device; killing the daemon takes the input device away, and the clone the
supervisor builds arrives holding nothing. `sendevent ... 1 113 0` clears it
without waiting. `pkill -f` does not work on this toolbox and reports nothing
when it fails, so take the pid from `ps` and check it changed.

**Whether the microphone is muted.** The state is AudioFlinger's `mMicMute` and
no dumpsys prints it, so the read is a binder call: `GET_MIC_MUTE` is the
eighteenth transaction on `IAudioFlinger` and the reply is one int32.

```sh
adb shell 'su -c "service call media.audio_flinger 18"'   # Parcel(00000000) live, 00000001 muted
adb shell 'su -c "service call audio 13 i32 3"'           # music level on the live route
adb shell 'su -c "service call audio 15 i32 3"'           # its maximum, 30 on biscuit
adb shell 'su -c "service call audio 4 i32 3 i32 9 i32 0 s16 overdub"'   # set it to 9
adb shell 'su -c "service call media.audio_flinger 11"'   # master mute, 00000001 is silence
adb shell 'su -c "service call media.audio_flinger 9 i32 0"'   # lift it
```

- About 12ms a call, measured at 500 calls in 6 seconds.
- The mute fires on the key-**down**: a lone `1` mutes, a lone `0` does nothing,
  and a second `1` while the key is held does nothing either.
- `dumpsys audio`'s `Mute count` is not this: it is the per-stream *output*
  mute, 0 whatever the microphone is doing.

**A silent Dot whose every reading looks right is the master mute.** No dumpsys
on this build prints it -- not `audio`, not `media.audio_flinger` -- so audio
reaches the HAL, DL1 prepares and starts, I2S enables, the external amp switches
on and pops audibly, and nothing is heard. Counting back from `GET_MIC_MUTE` at
18 gives `SET_MASTER_MUTE` 9, `MASTER_VOLUME` 10, `MASTER_MUTE` 11, and that is
the first thing to read when the Dot goes quiet.

**Do not go looking for a transaction by calling one.** `setStreamVolume` at 4
was found by calling its neighbours blind, and that cost a music volume zeroed to
0, four streams left muted by `setStreamSolo`, and the master mute above -- none
of which announced itself. Probe the getters, which name themselves by answering
a level or a maximum you already know, and derive the setters from the ordering.

**A mute taken over `service call` is permanent.** AudioService releases a stream
mute or solo when the client that took it dies, through a death recipient on the
binder the caller passed. `service call` passes none, so there is nothing to fire
when it exits: `setStreamSolo(3, true)` left `RING`, `ALARM`, `NOTIFICATION` and
`TTS` muted across a daemon restart, and only `setStreamSolo(3, false)` cleared
them.

Injecting the key is a real mute, so it silences the microphone until it is
pressed again:

```sh
adb shell 'su -c "sendevent /dev/input/event1 1 113 1; sendevent /dev/input/event1 0 0 0;
                  sendevent /dev/input/event1 1 113 0; sendevent /dev/input/event1 0 0 0"'
```

**What the device says about itself:**

```sh
adb shell 'su -c "cat /data/local/tmp/overdub.log"'      # daemon log, truncated per boot
adb shell 'su -c "logcat -d -v brief -s tts-Server tts-Playback"'   # Alexa on a playback
adb shell 'su -c "iptables -L INPUT -n -v | grep 6053"'  # the rule, and its packet count
```
