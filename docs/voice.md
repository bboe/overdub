# Alexa's session

Alexa's session runs through this daemon on its way to Amazon.

There is nothing to configure. Every Dot that installs the daemon works this way,
which is what makes the install and the uninstall the part worth testing.

## What was measured

- The app reads where to send its session when it starts, and only then. So it
  is killed and left to come back, which takes about 20 seconds. The kill
  happens only when the endpoint actually moved: a daemon that restarts 5
  seconds after a panic would otherwise keep Alexa dead for as long as the loop
  ran.
- Nothing brings the app back by itself. When its session ends it parks in
  `DisconnectState` and stays there: a wake word is logged as
  `Got SpeechStartEvent in DisconnectState`, the session aborts the event
  without dialling anything at all, and she answers "I'm having trouble
  understanding". Clearing the two properties does not reach it, because it
  never reads them again. So `uninstall.py` kills the app after clearing them,
  and a Dot whose daemon is stopped by hand is mute until something restarts
  either one.
- `am force-stop` does not do it. It exits 0, leaves the pid alone, and leaves
  the daemon with nothing connected to it until the next reboot.
- `setprop` returns before the value is visible to the `getprop` after it. The
  file under `/data/property` already held the value while the read that
  followed came back empty, so each property is read back until it settles.
- A reply that is merely written is a reply the device never sees. The
  long-lived request is answered with a status and nothing else until there is
  something to say, so the headers are flushed as soon as they are written.
- A failure is answered with a failure. Amazon unreachable breaks the stream,
  which is what the device would see if the network ate the connection, and its
  own retry covers it. A status invented here is a status she never sent.

## What it changes

- Two properties, `persist.amazon.scl.host` and `persist.amazon.scl.port`. They
  are the only properties this daemon writes outside its own namespace, and both
  outlive a reboot.
- One file under `/data/local/bin`, which the daemon creates on first run, and
  one under `/system`. The second always lands under the same name, so nothing
  has to be kept to remember it by; `internal/avs` pins that name with a test,
  and `uninstall.py` names it directly.

`uninstall.py` undoes all three and reads each one back. docs/deployment.md has
that order and why it is that order.

## What depends on it staying up

**If the daemon is not running, Alexa cannot answer.** The properties still point
at it. They are cleared when the daemon shuts down, so only a crash leaves them
set, and the boot script restarts the daemon 5 seconds later.

## What this does not do

- It does not read, decode or change anything in the session.
- It asks Home Assistant nothing, and holds no Home Assistant credential.
- It adds no entity and no control.
