# Running a command as though it were spoken

`internal/alexa/command.go`, `deploy/mapdump/`, and the text entity and user
service in `internal/esphome`. Off unless `mapdump.jar` is installed *and* the
Dot is registered to an Amazon account; either one missing and the entity is
never advertised.

## Why the cloud, when the Dot is right here

Audio cannot be injected into Alexa on this device. `amazon.speech.sim` looks
exactly right and works in the narrow sense -- it fires the wake word and opens
a real cloud session -- but **only the detector hears the file**: the upload
still comes from the live microphone.

"Alexa" followed by eight seconds of silence, and "Alexa, play dance party music
on Amazon Music", ended at **1.16s and 1.11s** -- identically, because the
command audio never arrives. Opening the session first with
`SpeechRecognizer_ExpectSpeech` does not help: it opened 0.79s into the injected
speech and ended 199ms later. The `avs-device-sdk` recipe does not transfer,
because there the injector replaces the microphone the whole SDK reads from.

So a command goes the long way round: `Alexa.TextCommand` through
`/api/behaviors/preview`, the Alexa app's own endpoint and the only layer that
takes text. The Dot is named in the payload by its serial, which is read from
`ro.serialno` and matched against the device list the account returns, so the
command lands on this Echo rather than on whichever one the account happens to
list first.

## The credential

MAP's store is encrypted at rest, so scanning `map_data_storage_v2.db` yields
nothing. Its gate is *identity* rather than a signature, which is the opening:
`deploy/mapdump/MapDump.java` runs as MAP's own uid and asks MAP's own storage
class to decrypt, so no crypto is reimplemented here.

**MAP's gate is not the wake word's gate.** The permissions guarding audio
injection are `signature|system` and enforced by Android, so a `/system/priv-app`
install is granted them. MAP is the opposite: Android grants nothing, so a
privileged install buys nothing either.

**Root reaches the store too, and uid 32051 is a choice rather than a
requirement.** Run back to back with nothing else changed, `su -c` and
`su 32051 -c` both report `accounts: 1` and return the same 353 characters. The
directory holds no optimised dex and the jar is world-readable, so root is not
borrowing anything 32051 set up. The daemon uses 32051 anyway: it is MAP's own
uid and the least privilege that works, and a root shell has no business holding
someone's account credential when a nobody-uid will do.

**The older store answers confidently about the wrong file.** It reports zero
accounts on this build rather than an error, which reads exactly like "there is
nothing stored". The live store is `map_data_storage_v2`, reached through
`BackwardsCompatiableDataStorage` -- Amazon's spelling, not a typo here -- and
that class takes MAP's own context wrapper rather than a plain `Context`, which
is the other half of reaching the right file.

**The package context has to lie about one method.** MAP's helpers start from
`getApplicationContext()`, and here it is null: nothing in this process ever
created an `Application` for MAP's package, so they build on null and then
dereference it. A `ContextWrapper` that answers itself is enough, and everything
else still delegates to the package context, so the class loader and the data
directory stay MAP's.

**The accounts are sorted before the first is read.** A Dot on more than one
account would otherwise return whichever token the `Set` happened to iterate
first, and a credential that changes between runs for no visible reason is the
kind of thing that is diagnosed twice. `getAccounts` answering null is reported
as `accounts: 0` rather than thrown, because zero accounts is the case somebody
is actually in and a `NullPointerException` sends them looking elsewhere.

Extraction costs 0.55s measured: too much to repeat per command, cheap once per
boot, so the token is held in memory for the life of the process. A refusal
drops that copy -- a 400, 401 or 403, or an authenticated call answering without
what it should -- because Amazon rotates the token, and a daemon that extracted
it once would work until it silently stopped.

The second half of that is not only the status codes. A stale session can answer
200 with a sign-in page, or with an empty device list, and neither is a refusal
to a reader that only checks the code: the first fails as a parse error and the
second as "no device on the account matches this Dot's serial", which reads like
a device problem. Both are treated as refusals, so the token is dropped and
taken again once rather than every command failing the same way until a restart.

What MapDump wrote is checked before it is spent, too: a token has no whitespace
in it and is between 16 and 4096 bytes. Anything dalvik prints on stdout beside
the value would otherwise be posted to `api.amazon.com` as the credential and
come back as an opaque 400.

The token never reaches the disk. MapDump puts the value on stdout and every
step, count and failure on stderr, so stderr is what may be logged and stdout
never is.

## What the token buys, measured rather than assumed

`exchangeCookies` asks for `.amazon.com` because that is the domain the Alexa app
itself asks for, and the reply is `at-main`, `sess-at-main`, `session-id`,
`session-token`, `ubid-main` and `x-main` -- a signed-in amazon.com session
rather than an Alexa-scoped one. `at-main` is the retail authentication cookie.

**Asking for less does not get you less, and this was tried.** Requesting
`.alexa.amazon.com` returns the same six cookies and the command still works --
but the reply comes back keyed `.amazon.com` either way, so the server issues an
apex session whatever is asked for. The `Domain` this code stamps on each cookie
is its own choice and binds nothing at Amazon's end: narrowing it changes only
where this client will send a credential that is unchanged.

There is no smaller credential to hold, so the scope of this feature is the
account, and the only lever left is whether the jar is installed at all: the
registration check below decides whether the feature can work, never how much it
reaches. SECURITY.md and README.md both say so where somebody would meet them.

## The two user agents

The `api.amazon.com` one is MAP's own, read off the device rather than
reconstructed, because its middle field is the MAP client library version and is
not derivable from system properties:

```
AmazonWebView/MAPClientLib/130050002/Android/5.1.1/AEOBC
```

The `alexa.amazon.com` one has no on-device ground truth, because the Dot never
contacts that host: it is the Alexa app's shape, built from this device's real
model and release with an invented version.

## What the daemon offers

A text entity, `alexa_command`, and a user service, `send_command`, both listed
only when `UseCommand` has been given something to call. They are two front
doors on one path: the text entity is for a person looking at the device page,
the service for an automation, and both end in `commandLocked`.

The send is a network call of up to thirty seconds, so it never happens on the
read loop: the text is queued under the lock and a worker runs it.

**The queue is where this stops copying playback.** Two URLs cannot play at
once, so the media player keeps the latest and drops what it replaced. Two
commands are two things somebody asked for, and a minute is a long window to
lose one in -- `extractToken` and the HTTP call have thirty seconds each, and an
auth failure runs the pair twice. An automation that sets a timer and then turns
on a lamp would have had the timer silently dropped, with both lines in the log
saying they ran. So commands queue, four deep, and the fifth is refused with a
line telling the peer it was dropped rather than accepted. A peer holding the
key can otherwise queue without bound.

The text the peer sent is echoed into the entity's state once the worker picks
it up, so the box shows the last command rather than emptying. It is published
through the ordinary reading path, which means a subscriber arriving later is
told the same thing.

**An empty state is published the moment the box is wired**, and that is not a
tidiness. Home Assistant draws a text entity it has never had a state for as
*unavailable*, and an unavailable text box cannot be typed into -- so a box whose
only source of state is somebody typing into it stays grey for ever. Measured on
a Dot: the entity listed, the Dot registered, the credential working, and the
card reading `Alexa command: unavailable`, with the service the only way to
reach the feature at all. The empty state costs one reading and takes the whole
case away.

A command is peer-supplied text, so it is truncated to the peer-string length
before it reaches the log. The failure it may cause is not: that sentence is
this daemon's own, and the client bounds every body and every diagnostic it
quotes at 300 bytes, so cutting it again at 64 lands in the middle of the
reason. The retry reports the second refusal rather than both, because two
wrapped 300-byte messages would be twice the line docs/pitfalls.md budgets for
and the first adds nothing: they are the same refusal either side of a fresh
token. Measured on an
unregistered Dot, the 64-byte cut left `extract token: exit status 1: step:
system context = android.app...` and threw away everything that said why. Both
lines go through the peer rate limit, because a peer is what causes them.

Empty text is dropped rather than sent, because Home Assistant clears the box by
writing an empty string to it -- and the box is cleared with it, since leaving
the last command published would make the card snap back to the text somebody
just deleted.

The clear rides the queue rather than jumping it. Deciding whether a clear is
worth publishing by looking at what is published is wrong while a command is
still queued: the state it would consult is the one *before* that command, so a
clear sent a moment after a command is dropped as redundant and then overwritten
by the command it was meant to erase. Queued, it is simply the next thing the
worker does, and the order somebody typed in is the order the box shows.

Text longer than the box is refused rather than trimmed. `commandMaxLength` is
advertised in the listing, which binds Home Assistant's field and nothing else:
a peer holding the key can send a whole frame, and every byte of it would be
published as entity state into the recorder and posted to Amazon. The limit is
enforced where the text arrives, on both the entity and the service, and it
counts runes rather than bytes because that is what the number in the listing
means to Home Assistant: 200 CJK characters pass its own field and would
otherwise be refused here for being 600 bytes.

## Registration

The command needs a Dot registered to an Amazon account, because the credential
is the registration, so `serve.go` checks both the jar and the registration
before wiring anything and the entity is absent unless both hold.
`binary_sensor.<name>_alexa_registered` reports the second directly; docs/api.md
has how it is read.

`commandReady` fails open on the reading rather than on the feature: a
registration that answers "not registered" hides the box, and one that could not
be taken at all does not. The jar is a deliberate act and the reading is an
observation, so an observation that failed should not revoke the act. That is
the one case where the box can be offered and still fail, and README.md says so
too.

What that check is worth was measured on an unregistered Dot before it existed.
The box was listed, a command was accepted, and the whole diagnosis in the log
was `extract token: exit status 1: step: system context = android.app.ContextImpl@...`.
That exit code is MapDump's own answer -- it exits 1 for "not found on any of 0
accounts" -- but the sentence saying so never arrives: run from the daemon,
stderr after the first line does not reach the pipe, and logcat carries no
exception either. Run by hand as uid 32051 on the same Dot, the same jar prints
all four steps and `accounts: 0`. So the state is knowable and the failure is
not self-explaining, which is the case for refusing to offer the entity rather
than explaining it afterwards.

A Dot that has both at startup wires the box there and then. One that is missing
either is asked again every five minutes, because "run once at startup" is the
rule this tree breaks everywhere it depends on something that appears later. The
retry re-reads both, since a jar can land after the daemon starts as easily as a
registration can: it costs a stat, and a fork only where the jar is there and
the account is not -- 0.003% of a core -- and it stops for good the moment both
hold.

The registration is read only where it is used, which is after the jar check.
The reading can cost a second and a half, and `serveAPI` is on the cold boot's
critical path in front of the firewall rule and the responder.

Wiring the box late drops every connection, the way `UsePlay` does, because
Home Assistant reads the entity list once per connection: a client that listed
before the box existed would never hear about it otherwise.

Deregistering a Dot while the daemon runs leaves a box that fails, which is the
narrow case this does not cover.

Registering a Dot is Alexa's own process and README.md carries it: give the
button back with `pass through`, hold it until the ring turns orange, and add
the device in the Alexa app.

There was a script for this, and it went. What it did was start Alexa's own OOBE
service directly, which is worth keeping written down even though nothing here
runs it now:

```sh
am startservice -a com.amazon.device.oobe.services.action.SETUP_MODE_ON \
  -n com.amazon.echo.csm.oobe/.services.SonarOOBEMainService --ei reason 2
```

The `reason` extra is the part that has to be right: 2 is
`OOBE_ENABLED_BUTTON_PRESS`, and without it
`logcat -s AmazonOOBE.SonarOOBEMainService` reports `Start/Stop Reason = 0` and
setup mode does not start. Setup mode is visible as an address on `p2p0`, which
is the Dot's own network coming up, and it takes the Dot off the LAN with it.

What the script bought over the button was registering a Dot you are not
standing next to, against a root shell, an undocumented intent and a USB cable.
The button route needs none of those and is one control this daemon already
offers, so the script was the more fragile of the two paths to the same place.
