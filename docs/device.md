# Network adb, and the microphone

The two things Home Assistant can switch on the Dot itself. What it *reads* off
the Dot -- the signal, the temperature, the memory, the jack, the speaker, the
volume -- is in docs/api.md, with the polls that drive it.

## Network ADB

`internal/device/adb.go`, and a select of the *device* rather than of this
daemon: the button modes in docs/button.md say what overdub does with a key, and this one turns a
port on. It exists so the Dot can be worked on without a cable, which is the
thing a rooted Echo on a shelf is otherwise worst at.

Three positions, and what separates them is authentication rather than reach.
**Off** stops adbd listening on the network and closes tcp/5555, in that order.
The rule goes after the properties rather than before them, because a step here
can fail: a rule taken out in front of a property that did not take leaves adbd
listening with nothing in the chain to say so, and the re-assert below cannot
tell that apart from a port somebody wants open. It puts the rule back within
the minute, and the Dot the operator asked to close is serving the subnet again.
Failing before the chain is touched leaves the device where it was, which the
reading then reports truthfully. **Insecure**
opens it to anyone who can route to the Dot. **Secure** opens it to a client
holding the installed key, because `ro.adb.secure` is 1.

`ro.adb.secure` is a property init has already frozen, so `setprop` will not
move it and Magisk's `resetprop` is what does. That is the one thing here that
needs Magisk rather than root.

The select carries `mdi:console-network` and not the obvious name.
`mdi:android-debug-bridge` does not exist -- Material Design Icons dropped its
Android brand icons -- and a name Home Assistant cannot resolve is an error
nowhere: the field is sent, the frontend finds nothing, and the entity is drawn
with no icon at all. So icon names are checked against the library rather than
guessed, and nothing here can test one, because the set of real names lives in
the frontend.

**Secure is offered only when there is a key to authenticate against.** The
listing leaves it out, and `setADBLocked` refuses it a second time in case a
peer sends it anyway. Without a key, asking for Secure does not produce a
stricter adbd: it produces a listening one that authenticates nobody, which is
Insecure arrived at by asking for its opposite.

**The device is read back rather than trusted.** adbd is restarted through a
property, so nothing hands back a result. The mode is set, the device is given
`adbSettle`, and then it is asked what it became. A read that could not be taken
is not an answer: `ro.adb.secure` is a fork with a budget like everything else
here, and a getprop that failed says nothing about whether authentication is on.
Collapsing that into "not secure" would publish `Insecure` for a Dot that is in
`Secure` -- the weaker of two security postures, invented rather than measured
-- and would send this read down the fail-closed branch below, shutting adb off
on a device already exactly where it was asked to be. The gap is why Secure fails
closed: if the property did not take while adbd came up anyway, the Dot is open
to the subnet with no authentication, which is strictly weaker than what was
asked for and arrived at silently. That one case is closed rather than reported.

A mode that could not be read is published as nothing at all. The rule check
runs iptables, which waits on the lock netd holds constantly, so it fails on a
working device -- and a select carries no `missing_state`, so the choice is
between the last value Home Assistant holds and a position invented here. The
invented one says the port is shut, which is docs/api.md's `p2p0` row again: a reading
nobody took, drawn as a measurement.

The reading is `CurrentADBMode`, and it is on the minute tick because of what it
costs. A port that is not listening is answered from `/proc/net/tcp` alone; only
one that is costs two forks, one of them an `iptables -C` that waits on the lock
netd holds constantly. So the cheap gate answers the ordinary case, which is the
shape the speaker's substream test in docs/api.md has.

**The rule is re-asserted by the sensor poll rather than by a ticker of its
own.** netd rebuilds the INPUT chain and discards what it finds, the same hazard
tcp/6053 answers with `HoldTCPOpen` -- but this port is held open only while
adbd is listening, so a Dot with adb off runs no iptables at all.

What decides that is `ADBListening`, and not `CurrentADBMode`, which is the
reading everything else here uses. That reading folds the firewall rule into
itself: a Dot whose rule netd has just wiped is still listening but no longer
reachable, so it reports `Off`. A re-assert gated on it would therefore stop at
exactly the moment it is needed, and the mode would never come back. The
question the re-assert asks is the narrower one the procfs table answers on its
own.

Nothing remembers an intention across a restart of this daemon either, which
falls out of the same choice: the supervisor respawns on any fatal exit, and a
flag would start false beside an adbd still listening. The device is asked
instead.

The poll does not re-assert at all while a position is being applied, and that
is a second rule rather than a refinement of the first. "adbd is listening" lags
a restart that was asked for: `ctl.restart` is a property init acts on when it
gets to it, so `SetADBMode` returns with the old adbd still up. A re-assert
landing there puts back the rule the close has just taken out, and once adbd
does go down nothing would ever remove it -- the mode reads `Off`, so no poll
re-asserts it, the no-op guard turns away a repeated `Off`, and `uninstall.sh`
leaves this port alone on purpose. So a close deletes the rule a second time
after its settle, and the poll stays out of the way while a worker is in flight.

`HoldADBOpen` takes the same lock as `SetADBMode`, because the decision and the
call have to be one step. Split, a poll that read "listening" a moment before
the worker closed the port puts the rule back afterwards, and the chain keeps an
ACCEPT for a port the select truthfully reports as closed -- which is the
duplicate-rule hazard in docs/pitfalls.md arriving by another road. It is not
the server's lock: this waits on iptables, which waits on netd.

The second close after the settle takes that lock too, through `DenyADB` rather
than `DenyTCP`, and the window it shuts is not a narrow one. `AllowTCP` waits on
netd's xtables lock for up to ten seconds, which is longer than the settle the
worker spends, so a re-assert released by the end of the apply can still land
its rule after the close that follows. `DenyTCP` alone takes the chain lock,
which orders the two calls against each other and says nothing about which
decision came first.

**Home Assistant can move a dropdown faster than adbd restarts.** So the command
hands a mode to a worker and returns: `SetADBMode` takes seconds and `handle`
holds the server lock for its whole body, which would park the accept path and
every other connection behind a property write. The worker keeps one pending
mode rather than a queue, so a dropdown dragged through three positions restarts
adbd once and lands on the third. Every position in between is a port opened
because somebody's finger passed over it.

A command asking for the position the device is already in is turned away, as
the button select in docs/button.md turns one away. There it saves a wake; here
it saves an adbd
restart, which drops every live adb session -- so a peer resending one position
on a connection it already holds would otherwise cost a restart every couple of
seconds for as long as it liked, on a path the eight slots do not bound. It is
compared against the position the last apply landed on rather than against the
device, because reading the device forks and this runs under the server lock --
and only when that apply reached where it was aimed. The published state is not
that position and cannot stand in for it: the worker wakes the poll rather than
publishing, so `published` trails a successful apply by a whole poll turn -- a
wake gap, an iptables pair that waits on netd, then the tick's own reads. A
guard reading it lets a peer resend the mode just applied all the way through
that window, at a restart apiece, which is the cost the guard is there to
refuse.

The apply has to have succeeded because a step of it can fail while the rest
took: the properties can land and the rule stay behind, and the device then
reads `Off` truthfully. Without that condition the guard would turn away every
`Off` after it, leaving an operator watching a stale rule with no way to drive
it out.

What retires that belief is a poll reading that disagrees with it. adbd can go
down for reasons nothing here asked for, and a guard that only ever learns from
its own applies would latch the select shut: it would sit on a position the
device is not in and turn away the one command that would bring it back. So a
tick that reads a mode other than the last applied one drops the guard, and one
that agrees costs nothing.

The worker wakes the poll rather than publishing what it read. Publishing would
make it a second reader of the device, which is the thing one published state
exists to prevent: the two can read either side of a change, and the later
publish is not the later reading.

Uninstalling does not undo any of this, and cannot. The position lives in the
property store and the INPUT chain rather than on disk, and deleting the rule
would cut the connection the uninstall may be running over. So the script says
so and leaves it to a reboot.

## Whether the microphone is muted

`internal/device/mic.go`, and a switch rather than a binary sensor: Home
Assistant reads the microphone and mutes it.

**The state is AudioFlinger's `mMicMute`.** It is what
`AudioManager.setMicrophoneMute` reaches through `AudioSystem`, so the mute key
and "Alexa, mute" both land there. No dumpsys prints it -- `dumpsys audio`'s
`Mute count` is the per-stream *output* mute, zero on this Dot whatever the
microphone is doing.

So the reading is a binder call, `GET_MIC_MUTE` on `IAudioFlinger`, by number
because binder offers no name. docs/hardware.md carries the transaction and the
thresholds behind everything below.

**What counts as a reading:**

- The reply is one int32, read as the bool that crossed binder: `0` or `1`,
  nothing else.
- A call that failed is not a reading, and that is a separate check: `Output`
  hands back what it captured alongside the error, so a child killed at its
  deadline can return bytes that parse.
- Either way it is published as nothing at all. A switch has no `missing_state`,
  so the choice is the select's, and an invented "not muted" says the Dot is
  listening.

**Setting it presses the key.** `Interceptor.Press` puts a whole key through the
clone, down and up, as the read loop re-emits one -- so the switch performs the
button's mute, ring included, and Alexa remains the only writer of her own
state. Writing `mMicMute` directly would leave her believing the microphone was
live and her code would overwrite the value.

The two halves differ:

- The down does the work. A lone down mutes; a lone up does nothing.
- The up is only the release. Losing it leaves Android holding the key, which is
  a reset gesture running unattended.
- A failed write cannot say which half landed, since `Emit` writes the key and
  its SYN under one error and Android acts on the key as it arrives. Both are
  `button.ErrKeyStuck`, and serve.go exits on it: the clone goes with the
  process.

**The switch is a toggle underneath**, so the worker reads before it presses:

- The key says "change", not "be muted". A press aimed at a state somebody else
  already reached puts the microphone back where it was not.
- A device that could not be read is not pressed. Guessing there unmutes a
  microphone that was deliberately muted.
- A press that did not take is not repeated. The line says what the device is,
  and the poll publishes the truth a tick later.
- The settle is 250ms, which is slack: the mute lands inside the first read
  after the press.

**A command is turned away only when the last apply landed where it is aimed.**
The published state cannot stand in for that -- it trails an apply by the settle
and a poll turn, and a command disagreeing with it inside that window is
somebody changing their mind rather than a repeat. A poll reading that disagrees
with the last apply retires it, so the switch cannot latch on a position the
device has left.

**The wake goes to the live poll**, which is the one that reads the microphone.
It rarely shortens anything: `PollLive` drops a wake inside `wakeGap` of its
last read, and its last read is the sound sample half a second ago, so the state
follows on the next heavy tick. Measured at 1.65-2.13s from command to state,
which Home Assistant draws as a toggle that springs back and flips again. The
gate bounds the poll rather than each group of forks in it; changing that
touches every wake the poll takes.

**It rides the heavy tick beside the volume**, under the volume's rule in
docs/api.md:
somebody mutes the Dot and then looks to see whether it took. 12ms against the
volume's 11.7ms.

**What it costs is a guarantee.** Unmuting used to need a hand on the Dot. It
needs the API key now, which is the whole of the access control, and an
automation that misfires can turn the microphone back on.

With `mute_button_mode` in `intercept` the physical button no longer mutes while
the switch still does. The mode governs keys arriving from the real node; this
press does not.

**No `entity_category`**, which is the event entity's rule rather than the
selects'. They are `config` because they change what the daemon does with a key;
this is the microphone, and a categorised entity is filed away from the
controls.
