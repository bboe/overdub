# Hardware

## How the device is wired

| Node | Name | Keycodes |
|---|---|---|
| `/dev/input/event1` | `mtk-kpd` | **138 = action ("dot")**, 113 = mute |
| `/dev/input/event2` | `keys` (gpio-keys) | 115 volume up, 114 volume down |

138 and 113 are the codes this project acts on; `event2` is listed for
orientation, and nothing here opens it. The clone advertises every keycode
`event1` declares, because the input core drops events for a keycode it has not
claimed. Keycodes and nothing else: it declares `EV_KEY` alone, and events of
any other type are dropped rather than re-emitted.

`event1` is read by `system_server`'s `EventHub`, so the action button travels
the ordinary input pipeline, and `/system/usr/keylayout/mtk-kpd.kl` maps 138 to
`BUTTON_MODE` and 113 to `MUTE`.

Because it arrives as an ordinary `KeyEvent`, an AccessibilityService with
`FLAG_REQUEST_FILTER_KEY_EVENTS` could consume `KEYCODE_BUTTON_MODE` with **no
root at all**. The evdev route needs no APK and no accessibility grant.

## Testing on hardware

A live Echo in someone's home: audio makes noise, and a press may drive real
automations. Prefer targeted tests.

```sh
# Simulate a button press
adb shell 'su -c "sendevent /dev/input/event1 1 138 1; sendevent /dev/input/event1 0 0 0;
                  sendevent /dev/input/event1 1 138 0; sendevent /dev/input/event1 0 0 0"'

# Alexa's own verdict on a playback attempt
adb shell 'su -c "logcat -d -v brief -s tts-Server tts-Playback"'

adb shell 'su -c "cat /data/local/tmp/overdub.log"'    # daemon log, truncated per boot
```
