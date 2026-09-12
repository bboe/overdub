# The button

## Button interception

`internal/button` over `internal/evdev`. `event1` carries the action button
*and* mute, so an exclusive `EVIOCGRAB` takes both. The fix is a `uinput` clone
advertising exactly the real key bitmap, named `mtk-kpd` so Android applies the
same keylayout; 138 is consumed and the rest re-emitted. `EventHub` picks the
clone up by inotify. Read the bitmap with `EVIOCGBIT`, never from sysfs: that
file's word size differs from `/proc/bus/input/devices` here, and guessing wrong
silently breaks mute.

The clone copies the original's `struct input_id` too, read with `EVIOCGID` for
the reason the bitmap is read with `EVIOCGBIT`: it is what the device says
rather than what this end believes about one model of Dot. All four fields
matter, and the name is only the last resort among them. Android resolves a
keylayout by `Vendor_XXXX_Product_XXXX_Version_XXXX.kl`, then
`Vendor_XXXX_Product_XXXX.kl`, then the device name, then `Generic.kl`. Measured
on biscuit, neither `Vendor_2454_Product_6500.kl` nor the clone's old
`Vendor_0001_Product_0001.kl` exists, so both fell through to `mtk-kpd.kl` and
the name alone was carrying it. Copying the ids means the clone would keep
matching on a Dot that did ship a vendor keylayout, where the name would not.

The bus is the field that was measurably wrong. `EventHub::isExternalDeviceLocked`
looks first for `device.internal` in the device's `.idc` and answers from that
when it is there; with no such property, which is this Dot's case for both
nodes, it calls a device external when its bus is `BUS_USB` or `BUS_BLUETOOTH`. The
clone hardcoded `BUS_USB` while biscuit's keypad is `BUS_HOST`. Measured before
the change, `dumpsys input` agreed on everything else -- both devices
`Sources: 0x00000501`, `KeyboardType: 1`, identical mapper parameters -- and
disagreed on `IsExternal`, false for the real node and true for the clone. That
is the one thing Android could have treated differently about a key arriving
from a passed-through press, so it is the one thing worth removing.

Copying the ids makes the clone's `InputDeviceIdentifier` byte-identical to the
keypad's, and Android disambiguates them anyway. Measured after the change, the
two report the same `bus=0x0019, vendor=0x2454, product=0x6500, version=0x0010`
and different descriptors, `f0d2e427...:24546500` against `d3f110579...:24546500`,
so the per-device settings that key on a descriptor do not collide. That is the
answer to the obvious objection rather than an incidental reading.

What no test holds is the wiring: the id reaching `uinput` is the one read from
the node. `userDev` is tested against an id a test hands it and `idFromBytes`
against bytes a test hands it, and the two are held together by a round trip,
but putting the old invented id back in `NewUinput`'s caller leaves the whole
suite green. Closing that needs `/dev/uinput` and root, which CI has neither of,
so it is verified on the device instead -- the daemon logs the id it cloned, and
`dumpsys input` reports what Android made of it. That is the rule CLAUDE.md
already states, said out loud here because the failure would reach the Dot
before anything else noticed.

The wait for `wlan0` does not give up. Nothing restarts a daemon that has not
exited, so a bounded wait that expired would cost the API for the rest of the
boot on a Dot whose access point came back a minute late. It polls quietly after
the first minute, because a line a minute for the rest of the boot would bury
everything else.

The button is taken before the network is waited for, and the API waits in its
own goroutine. Waiting for the MAC first left a Dot with no `wlan0` exiting
after a minute and being restarted five seconds later, with no button for the
whole boot. Waiting in the read loop would be no better, because mute passes
through that loop.

`main.go` is left holding the constants, the decision about what a press means,
how long to wait for the node, and the signal handler. That is the seam every
later feature arrives through: the package opens the node and reads it, and the
decisions stay with the caller.

A failed grab is fatal. Without the grab the real node still delivers to
`EventHub`, so a clone that echoed anyway would land every key twice and mute
would toggle on and straight back off. Exiting hands the button back to Alexa
and takes the clone with it; a live clone beside a live original does not.

A failed re-emission is fatal for the same reason. Writes fail for the clone
rather than for one key, so carrying on holds the grab with mute going nowhere,
and nothing restarts a daemon that has not exited. Exiting releases the grab and
the supervisor builds a new clone five seconds later.

A key left down is a reset gesture, held. Alexa binds a regular factory reset to
the action button alone at 20s, and an advanced one to mute and volume down
together at 8s; docs/hardware.md carries the config and the thresholds. So the
stake is a Dot that deregisters itself with nobody touching it, and exiting is
what takes the key away, because the clone goes with the process.

The clone is destroyed before the grab is released, and not the other way round.
The read loop can still be running when the close comes, so the reverse order
opens the same window: `event1` ungrabbed and the clone still live, and a key
pressed inside it lands twice. Losing that key is the cheaper failure.

## The button modes

These two selects are the only entities on this page Home Assistant writes to,
and every reading it is told about is read-only. Each is a select rather than a diagnostic reading of whether
the grab took, because what somebody looking at that reading almost always wants
is to change it.

**Two buttons, and each holds its own mode.** `event1` carries the action button
and the microphone mute, the grab takes both, and until now mute was simply
re-emitted. It is now a button in its own right: `mute_button` reports what it
did and `mute_button_mode` says what the daemon does with it. That is why the
mode lives on a `watch` per keycode rather than on the `Interceptor`: one key can
be held while another is pressed, so each needs its own latch as well as its own
mode.

Mute ships in **monitor** where the action button ships in **intercept**, and
that asymmetry is the point. Intercepting mute by default would leave a Dot that
cannot be muted by the button that says mute on it; monitor is additive, so
Alexa still mutes and Home Assistant is told. The zero value is still intercept,
because a key nobody mentioned is one this daemon should keep -- so the shipped
modes are named where the keys are, in `serve.go`, rather than left to the
struct.

`serve.go` holds one map of keycode to entity and shipped mode. One rather than
two, so a key cannot be watched without an entity to report it, or given an
entity nothing watches; `main_test.go` holds that map against the entities
`internal/esphome` actually builds, which is the half a single map cannot make
safe.

A chain per button, too: a run belongs to the key it was pressed on, and one
shared chain would read a press of each as a double press of either.

Three modes, and they are two independent things rather than three points on a
line: whether Android sees the key, and whether Home Assistant hears about it.
**Intercept** keeps the key and reports it. **Monitor** re-emits it *and*
reports it, so Alexa answers the press as she always did and an automation fires
too. **Pass through** re-emits it and reports nothing.

Measured on the Dot, 138 injected into `event1` with each mode set through the
API, counting the daemon's own gesture lines against `uber` in logcat, which is
this Dot's name for the action button. The daemon's count separates reported
from not; logcat is what separates the two modes that report:

| mode | daemon reports | Alexa sees the key |
|---|---|---|
| `intercept` | yes | no |
| `monitor` | yes | yes |
| `pass through` | no | yes |

It was a switch, and monitor is what it could not say. Two states can only offer
"ours" or "hers", and the useful third is both -- press-to-talk still working
while Home Assistant counts the presses.

Intercept is first in the list and is what a Dot ships in, so a device nobody
has configured keeps its button: the daemon exists to take it. `Mode`'s zero
value is intercept for the same reason, so a construction path that never
mentions a mode does not quietly hand the key to Alexa.

A mode the listing did not offer is refused rather than acted on. Home Assistant
only sends what it was told, so anything else is a peer inventing one, and a
select can be asked for a word rather than a bit -- which is a thing a switch
could not get wrong.

**The grab is untouched by all three.** Releasing it is the obvious reading of
"pass the button through" and it is the wrong one: the real node still delivers
to `EventHub`, so a released grab beside a live clone lands every key twice, and
mute would toggle on and straight back off. What the other two modes mean instead
is that keycode 138 is re-emitted through the clone like every other key. The clone
is named `mtk-kpd` so Android applies the same keylayout, and that is the route
mute has always taken here, which is the reason the clone carries the whole key
bitmap in the first place.

How far that is measured is worth being exact about. Android's input layer
treats the two devices identically: the same keylayout, the same
`Sources: 0x00000501`, the same `KeyboardType`, and since the ids are copied the
same `IsExternal`. `mtk-kpd.kl` maps `key 138 BUTTON_MODE` and both devices
resolve to it.

The app layer was the open question and it is now answered. Measured on the Dot
in pass through with 138 injected into `event1`, which reaches the daemon and
not `EventHub` because `EVIOCGRAB` gates reading rather than writing:

```
HeadlessKeyPolicyManager: KEYCODE_BUTTON_MODE, scanCode=138, deviceId=24, source=0x501
KeyEventObserver:         Received uber, keyCode=110, state=0
KeyListener:              STATE_DOWN -> STATE_UP -> STATE_SHORT
SPCH-SIM_StartSpeechCommand: mInitiator=SHORT_BUTTON_PRESS
SPCH-SIM_SimStateMachine:    ReadyState -> ListenState
```

`deviceId=24` is the clone, so Alexa's handlers do not care which device the
`KeyEvent` came from; the daemon logged nothing for that press and did report
the same injection in intercept, which is the control. "uber" is the
Dot's own name for the action button, and is what to grep for.

What that measures exactly is press-to-talk. Stopping a timer and entering setup
mode were not separately exercised, and they are the same `uber` key reaching
the same `KeyListener`: `STATE_SHORT` is what press-to-talk and a timer stop
both hang off, and setup mode hangs off the long press instead. So they follow
from the same evidence rather than resting on it, which is a weaker claim than
the one above and worth keeping apart from it.

Releasing the grab instead, so the real node delivers to `EventHub` directly, is
the obvious alternative and is rejected. It would give exact fidelity and cost a
worse property: two paths to Android rather than one. Toggling between them
inside a press desynchronises Android's key state, and one direction is bad
rather than untidy. Taking the button back while the key is held means Android
already has the key-down natively and never receives the up: `EVIOCGRAB` makes
the kernel synthesize no release, and a synthetic one through the clone cannot
clear it, because `dumpsys input` tracks `KeyDowns` per device. A `BUTTON_MODE`
stuck past six hundred milliseconds is Alexa's long press, which is setup mode.
With the clone as the only path there is nothing to desynchronise: a press is
either delivered whole or not at all, which is what latching at the key-down
buys.

**A press is latched at its key-down and stays latched until the key-up.** A
toggle can arrive between the two, and the two halves of one press must not take
different routes. The failures are not symmetric. Consuming the down and passing
the up gives Android a release for a key it was never told was pressed, which it
shrugs at. Passing the down and consuming the up leaves it holding `BUTTON_MODE`
for ever. So the flag is read once, at the down, and the release follows it.

**A key held across the daemon starting has no latch of its own**, and that is
the one case where the two halves of a press can still read the mode
differently. Android took the key-down from the real node before the grab, so
only the release arrives here. Nothing is reported for it, since there was no
press of ours -- but the release is still passed on for a key Android is allowed
to see. Not to clear that key-down: `dumpsys input` tracks `KeyDowns` per device,
so a release on the clone never reaches the one the real node recorded. It is
the app layer this is for, which does not look at the device at all -- measured,
`deviceId=24` reaching Alexa's handlers -- and so has a state machine our
release can end.

The autorepeats before it are dropped instead, whatever the mode. `EventHub`
reads any non-zero value as a down, so an emitted stale repeat is a fresh
key-down on the clone -- and the release behind it reads the mode again, so a
toggle in between swallows the only thing that could end it. That is Android
holding `BUTTON_MODE` for ever, which is the failure the latch exists to
prevent, arriving through the one pair the latch does not cover. Dropping the
repeat leaves nothing to strand.

**The button owns the flag and the server reads it**, rather than the server
owning it and the button being told. The read loop consults it on every event
and cannot take the server lock to do it, so it needs its own copy whichever way
round this goes; one authority means there is only ever the one. The zero value
is a captured button, so a construction path that never mentions capture keeps
the key rather than quietly handing it to Alexa.

Nothing is published from `handle`, and that is a hard rule rather than a
preference: `publish` takes the server lock, `handle` holds it for its whole
body, and a `sync.Mutex` is not reentrant, so publishing there deadlocks that
goroutine with the lock held and wedges the accept path and every other
connection behind it. So a toggle wakes `PollSensors` instead and the state goes
out on that poll's next turn -- which is also why the reading rides `readTicked` rather than
having a path of its own. That poll publishes once before its first wait, so the
switch has a state before any connection exists.

That wake is the one thing here a peer can ask for repeatedly on a connection it
already holds, and it is why `PollSensors` has a `wakeGap` at all. Subscribing
wakes the polls only on a connection's first request, so a peer spamming that
needs a new connection each time and the eight slots bound it; a switch command
needs neither. The no-op guard turns away a command asking for the state the
button is already in, and that is all it does: a peer alternating on and off
changes the state every time and so passes it every time. What bounds that is
the gap on the poll, which is where the bound belongs, since the guard can only
ever recognise the case a peer would not bother sending.

The log line saying which way the button went is carried out of `handle` on the
connection, the way the hello line is, because the log is a file on `/data` and
the lock gates the accept path and every other connection. It is the only
record that separates a button nobody is answering from one Home Assistant let
go of, and it is not a guarantee: it goes through `s.untrustedLog.Printf` like
every other line a peer causes, so a peer that has already spent the run's
ceiling on connection churn moves the button unrecorded.

Exempting it from that budget is the obvious fix and is wrong. `conn.noted` is
set once per state *change*, which is once per message a peer sends, not once
per wake -- the gap bounds the poll, not the toggles. So an exempt line is an
unbounded write to `/data` by an unauthenticated-until-the-key peer, which is
the hazard docs/pitfalls.md exists for. A budget of its own would work; sharing
the general one and saying so is what is done.

`SelectStateResponse` has a `missing_state` at field 3, as the state messages
carrying a reading do, and nothing here sends one: a select is what this end
last set it to, and there is no read of the device to have failed. Its listing
numbers are its own as usual -- 52, 53 and 54 against the sensor's 16 and 25 --
and `entity_category` is field 8 there, carrying `config` rather than the
`diagnostic` every other entity here
carries.

## What a press reports

The action button is one ESPHome *event* entity, `action_button`. It reports a
moment rather than a value, so nothing publishes it and no snapshot replays it.
A client that was not connected has missed it. Publishing a press instead would
fire every automation hanging off one nobody made, on every reconnect.

`ListEntitiesEventResponse` is 107 and `EventResponse` is 108. Field numbers are
per message: `device_class` is 8 here and a binary sensor's icon, `event_types`
is 9 here and a sensor's `device_class`. Home Assistant drops an event whose type
the listing did not advertise, so one slice is both.

The class is `button`, which Home Assistant draws the icon from, so the entity
sends none. It sends no `entity_category` either: the readings are `diagnostic`
and the switch is `config`, and a categorised entity is filed away from the
device's controls. This is what the device is for.

**The gestures are Home Assistant's `ButtonEventType` verbatim.** Its
architecture discussion 1377 settled the set in July 2026, because integrations
spelled the same gestures differently and no generic automation worked across
them. An automation written for any other button works here.

Two of the six are absent, and none is mandatory: an integration maps what its
hardware produces. `press_start` went because the decision that approved the set
dropped it as a trigger, and no gesture here needs a key-down alone.
`multi_press_ongoing` went because it costs a message per press for a signal
nothing here uses, and a run's count is not settled until the run closes. The
same rule makes a single press `press_end` rather than a `multi_press_end`
carrying one.

**A run of presses is one gesture, and the gap ends it.** A run is closed and
reported with its count by three hundred and fifty milliseconds of silence after
a release. So a single press waits: nothing knows it was single until the gap
passes. The chime does not wait, which is what makes that affordable -- it is at
the key-down, 33ms in.

The gap stays under the hold threshold, and `main_test.go` holds the two
together.

**The hold is reported while the key is still down.** Six hundred milliseconds
is the threshold, and it is Alexa's: a `BUTTON_MODE` she sees held past it is
setup mode. So the hold somebody already has in their hand is the one this
reports. The boundary belongs to the hold.

That is why `MultiPress` takes `Down` and `Up` rather than a finished press.
`long_press_start` has to arrive while the button is held, so an automation can
act *during* a hold, and nothing at the release is early enough. Two timers run:
a hold timer armed at each key-down, and a gap timer armed at each release.

The release is the backstop. `AfterFunc` promises only "not before", so on a
busy core the timer can lose the race to `Up`, and the duration the release
carries decides the boundary when it does. That path fires `long_press_start`
and `long_press_end` together, so the hold is reported but not reported early:
an automation bound to the start runs after the button is already up. It is
reachable only when the timer runs late, and the alternative is reporting a key
held past the threshold as a press.

The pair is unpaired, and that is a cost rather than a solved problem. An event
carries no state, so nothing heals a missing one: a Home Assistant that took the
`long_press_start` and missed the `long_press_end` leaves whatever the hold
started running. Discussion 1377 accepted this. The alternative is a stateful
binary sensor of the raw key, which self-heals on reconnect and is what
ESPHome's own `binary_sensor` does, and which reports the key rather than the
gesture.

A hold ends the run in front of it, at the threshold, and the run is reported
first. They are separate automations at the far end. One lock held across a whole
report is the only ordering between the read loop and the two timers.

Every key event invalidates both timers, so a run cannot close while a key is
down. Without that, a press arriving inside the gap and then held would have its
run closed under it and be reported twice.

A timer that has fired cannot be stopped, so a generation says its moment has
passed -- a hold timer whose key came up, a gap timer whose run a new press took.
Either would report something no longer true rather than a duplicate. The key
event bumps the generation, because the key is what made the timer stale.

**The count and the duration ride a service call**, because `EventResponse`
carries a key and a type and nothing else. The standard puts the count in a
`multi_press_count` attribute, and Home Assistant's ESPHome platform passes only
the type string to `_trigger_event`, so an attribute has nowhere to go. A
`HomeassistantServiceResponse` carries them instead: `esphome.overdub_pressed`,
`is_event` set, with `event_type`, `device` and `button` always,
`multi_press_count` on `multi_press_end`, and `held_ms` on `long_press_end`.
`button` is there because every button fires the same service name, so an
automation filtering only on the device would run for all of them. The key uses the
standard's name.

Each extra key belongs to the gesture that has one. A count on a single press is
a 1 nobody asked about, and a run has several durations and no single one, so
both are absent rather than zero: a zero read as a measurement is the `p2p0` row
in docs/api.md again.

The `esphome.` prefix is Home Assistant's rule -- it fires an `is_event` service
call only for its own domain -- and it adds `device_id` itself, which the daemon
could not know.

**The numbers go in a different field from the strings.** Every value in a
`HomeassistantServiceMap` is a string, so field 2, `data`, arrives as one. Home
Assistant renders field 3, `data_template`, and every render ends in
`_parse_result`, which turns a numeric string back into a number. So the numbers
go in field 3 and reach an automation as integers, the way they would from an
integration that calls the bus directly. Every integration but this one does;
the string is an artifact of ESPHome's transport.

A literal never compiles: `is_static` is true without Jinja markers, so
`async_render` returns the parsed value without rendering. That matters because
a `TemplateError` drops the whole event rather than one value, and there is no
render here to fail.

The strings stay in field 2, `device` especially. It is the operator's `-name`,
and Home Assistant evaluates a `data_template` value carrying Jinja markers: a
Dot named for a template would be one this daemon asked it to run. A number this
end formatted cannot carry a marker, which is what makes the split safe.

That second message is a second subscription, `SubscribeHomeassistantServices`,
tracked per connection. A peer that asked only for states gets the gesture and
not the numbers. Home Assistant asks for both.

**The chime sounds at the key-down**, because it answers a different question. It
says the daemon has the button, and it has to sound before anything knows what
the run will be. One that waited would leave a hold silent while it was held. So
it sounds once per press rather than once per gesture -- four presses are four
chimes and one `multi_press_end` -- and only capture gates it.

The gestures are sent from whichever goroutine recognised them. The gap timer
closes a run, and so does the hold timer when a hold ends one, and so does the
read loop when the release is the backstop -- so all three can send a
run-closing gesture, and `long_press_end` comes from the read loop. `FirePress` holds the server lock only long enough to queue
a frame per subscriber, so the read loop mute passes through is not measurably
delayed. A queue was the shape this came from; it drops what it cannot take,
which loses presses in order to report them, and the lock is what keeps two
gestures in order anyway.

**A press with no server to tell is dropped.** The button is taken before the
network is waited for, so the read loop runs for as long as `wlan0` takes.
`serve.go` holds the server in an `atomic.Pointer`, and a press that finds
nothing there goes no further than the log. Queueing it would deliver a press at
a moment it did not happen.

**The gesture tests race their own timers, and one of them lost.**
`multipress_test.go` drives `MultiPress` with a 20ms gap and a 60ms hold and
then sleeps, which makes every assertion a footrace against a timer rather than
a question about the state machine. `TestARunDoesNotCloseWhileAKeyIsDown` slept
`3 * testGap` and then asserted the hold had not fired -- and `3 * testGap` is
60ms, which is the hold exactly. The two deadlines landed on the same instant
and the reading goroutine won by microseconds, which is a pass; when it lost,
`held` had already reported the run in front of it and started the long press,
so the failure read as a run closing under a key that was still down. Measured
on the branch that found it: three failures in two hundred runs under
`qemu-arm-static`, and one in forty with the emulator held to half a core. It
takes a local hold of `10 * testGap` now, which puts 140ms between the check and
the timer, and neither rate reproduces.

What is left is smaller and has never been seen to fire. Chaining `m.tap()` into
whatever follows has only the gap to do it in, because the tap's release arms the
gap timer and the next call has to beat it; five tests in that file depend on
20ms of wall clock arriving on time. Widening `testGap` would buy that margin and
charge every `settle()` four times over for a hazard with no evidence behind it,
so the number stays until something makes the case.
