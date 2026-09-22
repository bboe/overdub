# The button

## Interception

`internal/button` over `internal/evdev`.

- `event1` carries the action button *and* mute, so an exclusive `EVIOCGRAB`
  takes both. A `uinput` clone named `mtk-kpd` re-emits every key the daemon
  does not consume, and Android applies the same keylayout to it. `EventHub`
  finds the clone by inotify.
- Read the key bitmap with `EVIOCGBIT`, never from sysfs. The sysfs word size
  differs from `/proc/bus/input/devices` here, and a wrong guess silently breaks
  mute.
- The clone copies the node's `struct input_id` from `EVIOCGID`. Android picks a
  keylayout by `Vendor_XXXX_Product_XXXX_Version_XXXX.kl`, then
  `Vendor_XXXX_Product_XXXX.kl`, then the device name, then `Generic.kl`.
  Biscuit has neither vendor file, so the name `mtk-kpd` selects `mtk-kpd.kl`.
  The copied ids keep the clone matching on a Dot that ships a vendor file.
- The bus decides `IsExternal`. With no `device.internal` in the `.idc` (true
  for both nodes here), `EventHub::isExternalDeviceLocked` calls a device
  external when its bus is `BUS_USB` or `BUS_BLUETOOTH`. Biscuit's keypad is
  `BUS_HOST`.
- The clone's `InputDeviceIdentifier` is byte-identical to the keypad's
  (`bus=0x0019, vendor=0x2454, product=0x6500, version=0x0010`). Android still
  gives them different descriptors, so per-device settings do not collide.
- No test proves the id reaching `uinput` is the one read from the node. That
  needs `/dev/uinput` and root. The daemon logs the id it cloned; compare it
  with `dumpsys input`.
- The button is taken before the network is waited for, and the API waits for
  `wlan0` in its own goroutine. Mute passes through the read loop, so the read
  loop cannot wait either.
- The wait for `wlan0` never gives up. Nothing restarts a daemon that has not
  exited, so an expired wait would lose the API for the rest of the boot.

### What is fatal, and why

- A failed grab. The real node still delivers to `EventHub`, so the clone would
  land every key twice and mute would toggle on and straight off. Exiting gives
  the button back to Alexa.
- A failed re-emission. A write fails for the clone, not for one key, so the
  daemon would hold the grab with mute going nowhere.
- A key left down on the clone is a reset gesture held. Alexa resets to factory
  on the action button alone at 20 seconds, and does an advanced reset on mute
  and volume down at 8 seconds (docs/hardware.md). Exiting destroys the clone,
  which releases the key.
- `Close` destroys the clone before it releases the grab. The read loop can
  still run at close, so the reverse order leaves `event1` ungrabbed beside a
  live clone, and a key pressed then lands twice. Losing that key is cheaper.

## The modes

Two selects, `action_button_mode` and `mute_button_mode`. They are selects
rather than a reading of whether the grab took, because a person who looks at
that reading almost always wants to change it.

- Each keycode has its own mode and latch, because one key can be held while
  the other is pressed.
- The action button ships in **intercept**, mute in **monitor**. Intercepting
  mute by default would leave the mute button unable to mute. The zero value is
  intercept, because a key nobody configured is one this daemon keeps.
- The three modes are two independent choices: whether Android sees the key,
  and whether Home Assistant hears of it. Measured with 138 injected into
  `event1`, counted against `uber` in logcat:

| mode | daemon reports | Alexa sees the key |
|---|---|---|
| `intercept` | yes | no |
| `monitor` | yes | yes |
| `pass through` | no | yes |

- Intercept is first in the list, so an unconfigured Dot keeps its button.
- A mode the listing did not offer is refused and logged. Home Assistant sends
  only listed options, so anything else comes from a peer that invented it.
- A command for the mode the button already has is turned away.

### What the modes do not touch

- The grab. Monitor and pass through re-emit 138 through the clone like every
  other key. Releasing the grab beside a live clone lands every key twice.
- Android's input layer treats both devices the same: same keylayout, same
  `Sources: 0x00000501`, same `KeyboardType`, same `IsExternal`. `mtk-kpd.kl`
  maps `key 138 BUTTON_MODE`.
- Alexa's app layer does not care which device sent the key. Measured in pass
  through with 138 injected into `event1` (`EVIOCGRAB` gates reads, not writes):

```
HeadlessKeyPolicyManager: KEYCODE_BUTTON_MODE, scanCode=138, deviceId=24, source=0x501
KeyEventObserver:         Received uber, keyCode=110, state=0
KeyListener:              STATE_DOWN -> STATE_UP -> STATE_SHORT
SPCH-SIM_StartSpeechCommand: mInitiator=SHORT_BUTTON_PRESS
SPCH-SIM_SimStateMachine:    ReadyState -> ListenState
```

- `deviceId=24` is the clone. That measures press-to-talk only. Stopping a
  timer and setup mode use the same `uber` key and `KeyListener`, and were not
  measured separately.
- Releasing the grab to pass a key through would give two paths to Android. A
  key held at the release keeps its native key-down and never gets an up:
  `EVIOCGRAB` synthesizes no release, and an up through the clone cannot clear
  it, because `dumpsys input` tracks `KeyDowns` per device. `BUTTON_MODE` held
  past 600 ms is setup mode.

### Latching

- A press latches its mode at the key-down until the key-up, because a toggle
  can arrive between the two. Consuming the down and passing the up is
  harmless. Passing the down and consuming the up leaves Android holding
  `BUTTON_MODE` for ever.
- A key held across daemon start has no latch: Android took its key-down from
  the real node before the grab. Nothing is reported. The release reads the
  current mode, and is passed on unless that mode is intercept. It cannot clear
  the native key-down, but it can end the app layer's state machine, which
  ignores the device.
- Autorepeats of an unlatched key are dropped in every mode. `EventHub` reads
  any non-zero value as a down, so a stale repeat on the clone is a fresh
  key-down.
- `Interceptor` owns the mode and the server reads it. The read loop checks it
  on every event and cannot take the server lock, so one copy lives with the
  button.

### Publishing a mode change

- Nothing is published from `handle`. `handle` holds the server lock for its
  whole body, `publish` takes it, and `sync.Mutex` is not reentrant: that
  deadlocks the accept path. A mode change wakes `PollSensors` instead, which
  publishes the select.
- That wake is the one thing a peer can repeat on a connection it already
  holds, so `PollSensors` enforces `wakeGap`. The no-op guard does not bound it:
  a peer alternating two modes passes the guard every time.
- The log line for a mode change leaves `handle` on the connection
  (`conn.noted`), because the log is a file on `/data` and the lock gates the
  accept path. It goes through `untrustedLog` like every peer-caused line, so a
  peer past the run's ceiling changes modes unlogged. Exempting it would give a
  peer an unbounded write to `/data`.
- The select sends no `missing_state`: its state is what this end last set, and
  no device read can fail.

## What a press reports

- Each button is one ESPHome *event* entity, `action_button` and
  `mute_button`. An event is a moment, not a value: nothing replays it, and a
  client that was not connected missed it. Replaying presses would fire every
  automation on every reconnect.
- Home Assistant drops an event whose type the listing did not advertise.
  `event_test.go` holds `actionEvents` to the four types `FirePress` sends.
- The device class is `button`, and Home Assistant draws the icon from it, so
  the entity sends no icon. It sends no `entity_category`: a categorised entity
  is filed away from the device's controls.
- The gestures are Home Assistant's `ButtonEventType` verbatim, from its
  architecture discussion 1377 (July 2026). Automations written for any other
  button work here.
- Two of the six types are absent; none is mandatory. `press_start` was dropped
  as a trigger in that decision, and no gesture here needs a lone key-down.
  `multi_press_ongoing` costs a message per press for a signal nothing uses. A
  single press is `press_end`, not a `multi_press_end` of 1.

### The two timers

- A run of presses is one gesture. 350 ms of silence after a release closes it
  and reports the count, so a single press waits the full gap. The chime does
  not wait: it plays at the key-down, 33 ms in.
- The gap must stay under the hold threshold. `main_test.go` asserts this.
- A hold is reported while the key is still down. The 600 ms threshold is
  Alexa's: `BUTTON_MODE` held past it is setup mode.
- `AfterFunc` promises only "not before", so on a busy core the hold timer can
  lose to the release. The release then decides by the duration it carries,
  and fires `long_press_start` and `long_press_end` together. The alternative
  reports a long hold as a press.
- `long_press_start` and `long_press_end` are unpaired. An event carries no
  state, so a Home Assistant that missed the end leaves the hold's action
  running. Discussion 1377 accepted this cost; the alternative is a binary
  sensor of the raw key, which reports the key rather than the gesture.
- A hold ends the run in front of it, and the run is reported first. One lock
  held across a whole report orders the read loop and both timers.
- A run cannot close while a key is down. Otherwise a press inside the gap,
  then held, would have its run closed under it and be reported twice.
- A fired timer cannot be stopped, so a generation counter marks it stale. The
  key event bumps it, because the key is what made the timer stale.

### The count and the duration

- They ride a service call, because `EventResponse` carries only a key and a
  type. Home Assistant's ESPHome platform passes only the type string to
  `_trigger_event`, so the standard `multi_press_count` attribute has nowhere to
  go.
- The call is `esphome.overdub_pressed` with `is_event` set. It always carries
  `event_type`, `device` and `button`; `multi_press_count` only on
  `multi_press_end`; `held_ms` only on `long_press_end`. `button` is there
  because every button fires the same service name.
- A count on a single press is a 1 nobody asked about, and a run has no single
  duration, so both are absent rather than zero.
- Home Assistant fires an `is_event` call only for its own `esphome.` domain,
  and adds `device_id` itself.
- Numbers go in field 3, `data_template`. Every value in field 2, `data`, stays
  a string. Home Assistant renders `data_template`, and `_parse_result` turns a
  numeric string back into a number, so an automation gets integers.
- A literal is not compiled: without Jinja markers `is_static` is true and
  `async_render` returns the parsed value. This matters because a
  `TemplateError` drops the whole event.
- Strings stay in field 2, `device` especially. It is the operator's `-name`,
  and Home Assistant evaluates Jinja in a `data_template` value. A number this
  end formatted cannot carry a marker.
- The call needs a second subscription, `SubscribeHomeassistantServices`,
  tracked per connection. A peer that subscribed only to states gets the
  gesture without the numbers. Home Assistant asks for both.

### Sending

- The chime answers a different question: the daemon has the button. So it
  plays once per press, not once per gesture: 4 presses are 4 chimes and one
  `multi_press_end`. It plays only for the action button, and only in
  intercept.
- Gestures go out from whichever goroutine recognised them: the gap timer, the
  hold timer, or the read loop. `FirePress` holds the server lock only to queue
  a frame per subscriber, so mute through the read loop is not measurably
  delayed. A queue in between would drop presses it cannot take.
- A press with no server yet is logged and dropped. The read loop runs while
  `wlan0` is still awaited, and a queued press would arrive at a moment it did
  not happen.

### The gesture tests race their own timers

- `multipress_test.go` drives `MultiPress` with a 20 ms gap and a 60 ms hold,
  then sleeps, so every assertion races a timer.
  `TestARunDoesNotCloseWhileAKeyIsDown` uses a local hold of `10 * testGap`. A
  hold of `3 * testGap` fails 3 runs in 200 under `qemu-arm-static`.
- One race remains, never seen to fire: a `m.tap()` chained into what follows
  has only the gap to finish in, and 5 tests depend on 20 ms of wall clock
  arriving on time. A wider `testGap` would multiply every `settle()`.
