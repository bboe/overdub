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

`Mute count` is 0 on this Dot and nothing found so far moves it. "Alexa, mute"
sets the speaker's level to 0 and leaves every stream's count at 0, which is why
the ordinary level path reports a muted Echo correctly and the parser's muted
branch has never been seen to run. `input keyevent 164` does nothing at all,
and stepping below zero clamps rather than muting -- both consistent with API
22, where `ADJUST_TOGGLE_MUTE` does not yet exist. The dump's `mute affected
streams = 0x2e` does include `STREAM_MUSIC`, so the state is real and an app
calling `setStreamMute` would produce it; nothing on this device does.

A volume key pressed while Alexa-muted releases the mute and restores a level,
so a probe that presses one is not a read-only observation of a muted Dot.

**What the device says about itself:**

```sh
adb shell 'su -c "cat /data/local/tmp/overdub.log"'      # daemon log, truncated per boot
adb shell 'su -c "logcat -d -v brief -s tts-Server tts-Playback"'   # Alexa on a playback
adb shell 'su -c "iptables -L INPUT -n -v | grep 6053"'  # the rule, and its packet count
```
