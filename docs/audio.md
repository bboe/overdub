# Audio

What it takes to make a sound on this Dot, measured. The chime needs a fraction
of this; the rest is here because the obvious routes are wrong in ways that cost
an evening to discover, and one of them damages the device until a reboot.

## The topology

Card 0 is `mtsndcard` and carries twenty-six PCM devices. The speaker is
**device 23**, `TLV320AIC3204 Playback` -- a discrete codec on I2S1 rather than
the MediaTek internal one, whose amps all read `Off`. The routing to it is
already live and there is nothing to switch on: `Audio_I2S1_Setting On`,
`Audio_DacMux_Setting On`, `HP DAC Playback Switch On On`.

`/proc/asound/pcm` declares device 23 `playback 1`: **one substream**. The audio
HAL inside `mediaserver` holds it open for the life of the boot, idling in
`XRUN` between sounds rather than closing. Measured at rest, `trigger_time` was
eight thousand seconds behind `tstamp`, and forty seconds of sampling on an idle
device found it busy every time. That is normal steady state, not damage.

## Raw ALSA is closed, and trying it anyway does harm

A second process opening `/dev/snd/pcmC0D23p` gets **`EBUSY`** immediately with
`O_NONBLOCK`, and blocks for ever without it -- `tinypcminfo` hangs until killed.

The front-ends around it do open: devices 0, 2, 8, 20 and 25 all accept a
non-blocking open, and 5 and 18 answer `EINVAL`. They also reach the speaker,
which is the trap. `tinyplay` to device 25 is audible, and useless: its `hw_ptr`
resets to 112 over and over, never passing about 7,568 of the 240,000 frames in
a five-second clip, which takes eleven seconds to drain and sounds like a
fragment stuttering. The front-end has no clock of its own; the DSP starts and
stops it.

Playing into one while Alexa is speaking is worse. Her stream is running then,
so ours does progress further -- the longest monotonic climb in the trace sits
exactly inside her window -- but her own `hw_ptr` goes *backwards* in the same
rows, 7360 to 4544 to 3072 to 1072. That is her playback restarting under us.

**And it does not stop when the writes do.** The driver is left logging
`mtk_pcm_I2S0dl1_get_next_write_timestamp: MEM path to DL1 isn't enable` and
`mtk_pcm_I2S0dl1_pointer underflow` fast enough to empty the kernel ring buffer
-- `dmesg` spanned nine seconds of history. In that state every sound on the
device pops, including Alexa playing music, and every playback measurement is
wrong by about 5.4%. A reboot clears it: one such line afterwards instead of a
flood. Several confident conclusions were drawn from numbers taken in that
state and had to be withdrawn, which is the real reason this section exists.

## AudioFlinger is the way in

Alexa does not own the speaker. She opens an AudioTrack and AudioFlinger mixes
her with everyone else, which is how anything shares an Android audio device.
Reaching that layer needs no APK: `libOpenSLES.so` and `libwilhelm.so` are both
stock here, so a native binary -- or cgo inside the daemon -- can create a track
like any app. Measured, ours plays cleanly while device 23 stays `RUNNING`
throughout and Alexa's stream is undisturbed.

There can be one player. The OpenSL ES engine, the player, the buffer queue and
the pool written into it are C globals, so a second `Chime` would overwrite the
first's handles, and closing either would then destroy the other's player and
leave its writes landing in a pool nothing is playing from. `NewChime` refuses
the second rather than documenting the rule, and `Close` returns early the second
time. Nothing in the tree calls it twice today -- the one call site is a `defer`,
and the signal handler beside it exits the process rather than unwinding -- so
that early return is load-bearing for something else: it is what makes `closed`
mean "the player is going", which `Play`, `OpenStream` and `ahead` all read, and
it is what stops a second `close(c.stop)` from panicking a daemon whose
supervisor cannot tell that exit from any other.

`audio_open` unwinds what it built; the writes do not, so a failed enqueue costs
one chime rather than every chime after it. `SLAndroidConfigurationItf` is asked
for and not required: it only sets the stream type, so a ROM without it still
chimes rather than leaving the daemon permanently silent.

**The player is written to, and one writer owns it.** `audio_open(rate, channels)`
builds it empty and it is started once, at open. A goroutine then mixes a block at
a time and writes it: `audio_write` copies the block into a pool of its own and
enqueues it. Sounds are added to the mixer rather than played -- the chime is a
clip that gets rewound and re-added -- so a second press restarts it rather than
stacking a second copy.

There is no stop-and-clear any more. The player is started once and left playing,
so a `reset` that set `SL_PLAYSTATE_STOPPED` would be a one-way door: nothing sets
it playing again, every later `audio_write` still reports success, and the Dot is
silent for the rest of the boot. `stream/clear` arrived and did not bring one back:
it empties the jitter buffer instead, and the section on playing audio at a time
somebody else chose says what the 80 ms still in the queue costs.

The copy is the point of the pool. OpenSL's queue holds the **pointer** it was
given until the buffer has played, so whatever is enqueued has to outlive the
sound; the clip was therefore allocated in C and freed only at `Close`. Copying at
the write makes the caller's bytes ordinary Go memory that nothing on the C side
outlives the call to, which is what lets audio arriving over the network be
written straight through. A write beyond what the queue holds is refused rather
than overwriting a buffer that is still playing: `GetState` reports how many are
in flight, and a full queue takes nothing.

**A block is ten milliseconds, and that is the number to think about.** 480
frames, 960 bytes, one block to a buffer and eight buffers in the queue, so the
player holds 80 ms and the writer has that long to come back with the next block.
The queue is the smallest unit anything here can schedule against, so a stream
that must start on a timestamp can place it no more finely than one buffer: at the
16 KB this page used to describe, that quantum was 170 ms against a target of one
millisecond. Ten costs nothing the driver notices, measured below, and the pool
came down from 128 KB to 7.7 KB with it.

A full queue is the ordinary state rather than a failure, so the writer waits it
out: a refused write is retried two milliseconds later, a fifth of a block and
well inside the 80 ms the queue holds. **It waits for a second and then gives up**,
which is the difference between a queue that is full and one that has stopped
draining. The second happens -- a wedged track, an AudioFlinger restart, the
driver state at the top of this page -- and it is the one failure with no signal
of its own: an outright error gets the line below, but a refusal that never ends
looks exactly like the ordinary case it is supposed to be. A second is twelve
times the queue's own depth, so nothing healthy reaches it. A **short** write is the other thing it has
to survive, and looping over what is left is what keeps the block size and the C
chunk size from having to agree. They are both 960 bytes today, in two languages,
with nothing tying them together: a block of 20 ms against a chunk of 10 would
otherwise be written once, half-accepted, and read as a failure on the very first
write of the run -- with the daemon starting cleanly and the Dot silent for the
whole boot.

That failure is worth a word on its own, because the writer is one goroutine and
nothing restarts it. If a write is refused outright the writer stops, and it says
so **once**, with the reason and the time, rather than leaving the next press to
report it. Measured by making every write fail: one line reading `the writer
stopped: audio_write would not take the block; the Dot is silent until the daemon
restarts`, and three presses afterwards logged nothing at all. The alternative --
a line per press for the rest of the boot, naming neither cause nor time -- is the
shape this page argues against everywhere else.

`Close` waits for the writer to stop before destroying the player, which is not
the same as waiting for the sound to end: the writer goes idle as soon as the
mixer is spent, while up to 80 ms is still queued behind it, and the player is
then destroyed under those buffers. A sound in its last 80 ms when the daemon
stops is cut off. That was true before there was a writer to wait for, and the
wait is there so that nothing writes into a destroyed player rather than to drain
one.

**The writer stops when there is nothing to play.** An idle mixer reports no
block, the goroutine waits on it, the queue drains, and the device reaches standby
on its own schedule. Writing silence to keep the player fed would hold the amp
awake for the life of the daemon, which is exactly what the hum further down makes
expensive. Starting again needs nothing: a write after the queue has emptied
resumes on its own, which is one of the measurements below.

Nothing caps the length of a sound. A source is read a block at a time, so how
long it runs is the mixer's business and never the queue's -- which is worth
saying because the queue does cap what can be *outstanding*, and the two are easy
to conflate.

The mixing is Go rather than cgo, so it is tested off the device: summing,
saturation at both ends of int16, a source that ends mid-block, a spent source
being dropped so the writer can go idle, and a restart rewinding a sound rather
than doubling it.

The block a caller hands `next` sizes the work, rather than a constant it is
assumed to match: the scratch buffer grows to it. A block is `BlockFrames` today
because one writer asks for that, and the first caller to size a block from
something else -- a server's idea of a chunk, say -- gets audio rather than the
first ten milliseconds of it followed by silence.

A partial trailing frame is refused rather than padded or held: `audio_write`
rounds down to whole frames and answers -1 when nothing whole is left. Blocks are
whole frames by construction, so this says that splitting a frame across two
writes is the caller's mistake rather than something the player will paper over.

### What the queue does, measured rather than read

The three things a stream will depend on had never run: the chime writes 38,400
bytes into a 131,072-byte queue and resets before every sound, so it never fills
the queue, never underruns, and never starts an already-playing player. A
throwaway probe in the daemon exercised all three on a Dot, writing silence so
that none of it made a sound.

- **The refusal is exact.** The queue took 131,072 bytes and then refused, which
  is `CHUNK * NUM_BUFFERS` to the byte, so `GetState`'s count means what the
  arithmetic assumes. Measured against the 16 KB buffers of the time; the
  arithmetic is the same at 960 bytes.
- **Buffers come back at the sample rate.** With the queue full, the wait for one
  slot was 152-200 ms over four runs against the 170.7 ms a 16 KB buffer is worth
  at 48 kHz mono. That is also the only rate feedback a writer gets.
- **An underrun is not a stall.** After four seconds of silence the queue had
  drained completely -- it refilled with the whole 131,072 bytes -- and a slot
  freed up again 152 ms later with **no reset and no second start**. So a writer
  that falls behind resumes by writing, which is what lets a stream be paced by
  the clock rather than by restarts. Calling `audio_start` on a playing player
  answers 0 and changes nothing.

ALSA cannot confirm any of that, which is worth knowing before trying. Everything
under `/proc/asound/card0/pcm23p/sub0/status` describes the HAL's own stream, not
ours: after four idle seconds it still read `RUNNING` with `delay` at 2,752
frames, and `hw_ptr` advanced at 48 kHz across the write either way, because
AudioFlinger mixes our track into a stream that runs whether we feed it or not.
The buffer queue is the only place our samples are visible.

**One number in there is why a buffer is now ten milliseconds.** A 16 KB buffer is
170 ms of audio, and since the queue is the smallest unit anything can schedule
against, that was the finest window a sound could be placed in, against a target
of one millisecond. Shrinking it costs nothing the driver notices: five chimes at
16 KB and five at 960 bytes produced 11 and 10 `underflow` lines in `dmesg`, which
is one pair per chime either way, at the moment each ends. That is the ordinary
end-of-playback line rather than a starved writer -- a goroutine feeding 10 ms
blocks keeps up on this device, with 80 ms of queue behind it. Confirmed by ear as
well as by counting, over many presses in a row: no stutter, no clipping where two
chimes meet, and no late start.

**Half a minute is the real test, though, and the chime is 400 ms of it.** A
throwaway probe played a 30-second exponential sweep, 120 Hz to 6 kHz, as a mixer
source: 3,000 blocks, **not one `underflow` line in `dmesg`**, and smooth by ear.
A sweep rather than a tone for the reason two sections down, and the audible check
matters as much as the count -- a 10 ms gap is far easier to hear than to find in
a log.

The number that says *why* it held is the write count: 12,897 attempts for 3,000
blocks, so about three quarters of them were refused. The writer ran ahead, filled
the queue, and spent the run waiting on a full one. Starving looks like the
opposite -- every write accepted at once, because the queue always has room --
with underflow pairs scattered through the run rather than absent. So the useful
health check on this path is not "did anything fail" but "is the writer being
refused", and a run where it never waits is a run to look at.

Nothing resets the pool, so `slot` simply wraps: the safety is the refusal above
and nothing else. A slot is reused only after `GetState` has reported fewer
buffers in flight than the queue holds, and buffers complete in the order they
were enqueued, so the slot coming up is always one the mixer has finished with.

"The chime" below says what the daemon does with that, and why the build
target is `GOOS=android`.

## Latency is process startup, not hardware

Press to sound, timed to the moment the codec substream goes `RUNNING`, five
trials each:

| route | mean | own work | RSS |
|---|---|---|---|
| Alexa's `SpeechSynthesizer` | 691 ms | -- | -- |
| native helper, spawned per sound | 366 ms | 4 ms | -- |
| Java via `app_process`, spawned per sound | 511 ms | 10-20 ms | -- |
| native helper, resident | 33 ms | 0.4-1.8 ms | 10.0 MB |
| Java, resident | 25 ms | 4.9-28.6 ms | 25.3 MB |
| cgo in the daemon, resident | 33 ms | 1.8-6.9 ms | +9 MB |

The 333 ms a spawn costs is `exec` plus linking `libOpenSLES`, not the audio
path waking: the helper's own work from `main` to the buffer queue is 4 ms. So
residency is the whole trick, and once something must be resident the daemon may
as well be it.

Java's jitter is its garbage collector, and it buys one thing the others cannot:
audio focus, and so ducking. `AudioManager` needs a `Context`, which outside an
app means `ActivityThread.systemMain()` by reflection. Measured, that works and
the focus request is granted and music does duck -- and the process segfaults on
teardown, on the main thread inside `app_process32`.

## The clock

For synchronised playback, take the clock from
`/proc/asound/card0/pcm23p/sub0/status`: `hw_ptr` with `tstamp`, which the driver
updates from period interrupts and which is CLOCK_MONOTONIC, the same base as
Go's `time` and Java's `System.nanoTime`. Over 292 seconds it fits a line at
**48000.19 Hz**, +3.9 ppm from nominal, with a residual of 0.136 ms mean and
0.485 ms worst. It is a plain file read.

### How long until a written sample is heard

About **95 ms**, and the number is stable enough to compensate rather than chase.

Measuring it needs three instruments, because no single one sees the whole path.
ALSA cannot: `/proc/asound` describes the HAL's stream, which runs whether we feed
it or not, and the section above says so. What can be seen is where our samples
are: `GetPosition` on `SLPlayItf` reports the frames AudioFlinger has played from
*our* track, and the `delay` field of the ALSA status reports the frames the HAL
still holds beyond that. So the frames that have actually reached the DAC at an
instant `T` are `position - delay`, and the moment our first frame was audible is
`T - (position - delay)/48000`. That origin is the useful quantity: with it, frame
N is heard at `origin + N/48000` for as long as the stream does not starve, which
is the whole of what synchronised playback needs from this end.

Measured over five starts, forty samples each, a six-second sweep per start:

| start | origin, after writing began | spread over the run |
|---|---|---|
| cold, from standby | 68.7 ms | 16.4 ms |
| warm | 95.7 ms | 3.3 ms |
| warm | 90.5 ms | 3.3 ms |
| warm | 97.1 ms | 13.5 ms |
| warm | 95.7 ms | 13.3 ms |

The four warm starts sit within 6.6 ms of each other and the quietest runs hold
to 3.3 ms, which is about the instrument floor: `GetPosition` is quantised to a
millisecond and AudioFlinger advances it in bursts, and `delay` moves a period at
a time. So the true figure is at least that steady and may be steadier; **±1 ms
is not a claim these instruments can support**, and a measurement that produced
one would be the `getTimestamp` story below repeating itself.

**That is not the same as saying sub-millisecond timing is out of reach here, and
the two numbers on this page are easy to read as contradicting each other.** The
clock above is sub-millisecond -- 0.485 ms at worst over 292 seconds, and 3.9 ppm
of rate error, which is 0.4 ms of drift per hundred seconds and trackable from a
file read. What that clock gives is the DAC's *timeline*. What it cannot give is
which of **our** frames is on it, because AudioFlinger mixed ours into a stream
that runs whether we feed it or not. The 95 ms is the bridge between the two, and
only the bridge is ±3 ms.

Which is the useful shape, because the bridge is a constant and mostly cancels.

Two of these Dots running one build should share it, and what multi-room playback
needs is that they agree with each other rather than that either knows its own
latency in absolute terms: a shared 95 ms is inaudible, while 6 ms between them is
not. That was the open question -- whether the offset is the same on every start
and on every unit -- and **three Dots playing one stream together have now
answered it.** Each reported its own pipeline depth as it placed its stream:

| name | ahead of the speaker |
|---|---|
| bryce | 144 ms |
| caroline | 142 ms |
| daniel | 141 ms |

Three milliseconds apart, measured on three units at once rather than one at a
time, and they sounded like one speaker. That is the agreement the cancellation
argument needs, and it is the number the table further down could not produce.

**The experiment that settles it is two players and one server**, because a
difference between two speakers cancels the absolute error this page cannot
remove. docs/sendspin.md carries it as what a player has to pass before it is
finished. Measuring three Dots one at a time is what that claim replaces, and it
was tried first:

| Dot | three runs | mean | run to run |
|---|---|---|---|
| bryce | 72.7, 85.9, 66.4 | 75.0 ms | 19.5 ms |
| caroline | 70.9, 67.5, 77.7 | 72.0 ms | 10.2 ms |
| daniel | 63.3, 59.1, 73.9 | 65.4 ms | 14.8 ms |

One standalone binary, pushed and run under `su` on each, so their daemons were
left alone and the three were measured identically. The means differ by 9.6 ms
and every unit's own runs differ by 10 to 20, so **the units are indistinguishable
from each other by this instrument**. That is not the same as being identical: it
says the question needs a sharper method, and the sharper method is two of them
playing together -- which is what the table above is, and it separates them to
3 ms rather than 9.6.

**And the number moves with how it is measured, which is the finding that matters
most.** The same Dot read about 95 ms from inside the daemon and about 75 ms from
a standalone binary minutes later. The production shape is the daemon -- a player
opened once at startup and living for the boot, with the rest of the daemon around
it -- so 95 ms is the figure to compensate by. But a 20 ms sensitivity to
conditions is wider than the 3 ms any single run suggested, and it is the honest
uncertainty on that number until two speakers settle it. Compensating is the
client's own job, and the next section says why it is not something to declare on
the wire.

**The cold start is the one to design around.** The first sound after the device
has gone to standby reads 27 ms lower than the rest, with the widest spread of any
run, and the reading is least trustworthy exactly there -- the HAL buffer is
filling while the samples are being taken, so `delay` understates and the origin
comes out early. Whether the true cold-start latency differs or only the
measurement does is not settled here. Either way a player that has just woken the
amp should not assume the warm number, and the cheap answer is to not go cold:
keeping the stream fed across a gap costs the standby the hum section describes.

This 95 ms is **not** what Sendspin's `static_delay_ms` carries, which is the
first thing anybody will assume: that field is for delay past the device's audio
port, and a Dot's speaker is on the near side of it. The delay inside the Dot is
the client's own to compensate for before scheduling, and the section below on
playing audio at a time somebody else chose says why it is read off the player
each time rather than subtracted as this constant; docs/sendspin.md carries the
trap that makes declaring it look like it works. The eight-block queue does not add to it either -- the first frame is at
the head of the queue rather than behind it -- and what the queue bounds is how
far *ahead* the writer may run.

`AudioTrack.getTimestamp` is reachable without an APK -- `javac`, `d8`, then
`CLASSPATH=x.dex app_process /system/bin Main` -- and answered all 1194 calls
across a five-minute run without one refusal. Do not trust it. Three runs of the
same measurement gave 48000.000, 48028.07 and 48053.01 Hz, with residuals of
0.007 ms, 19.3 ms and 24.1 ms; the variable was the track buffer, since
`getMinBufferSize` itself returned 18432 on one run and 36864 on the next.

The first of those runs is the cautionary one. Exactly 48000.000 Hz with a
0.007 ms residual is not a crystal, it is a number computed rather than
observed, and it looked like the best result of the night. A clock claim here
needs a second independent source -- measured over the same span as the first,
since comparing 295 seconds of one against 12 seconds of the other produced a
confident wrong verdict on the way to this one.

### Asking the player how much it still holds

**`GetPosition` is not a cumulative playback head on this player, and reading it
as one is the most expensive mistake this page records.** It counts from zero each
time our track resumes: measured on a Dot, it read **0 ms for six full seconds
after a 400 ms chime had finished playing**, and then counted 240, 496, 752 ms
from zero once the next audio started. So `position - delay` says something true
only *inside* one continuous run of writes, and it says nothing that can be
compared against a counter of everything the writer has ever handed over.

That is the same fact as the `-3,056` below, which was measured before it was
understood: every first reading of a press came back at exactly that because
position was 0 and the HAL held 3,056 frames. Three presses in a row all starting
from zero is a cumulative counter saying it is not one.

What it cost is worth writing down, because nothing failed. A daemon that had
played one chime half an hour earlier read its pipeline as **2.802 s** deep, and
the next stream read **6.462 s** -- exactly 3.66 s more, which is exactly the
silence the first stream had written. It placed each stream that far out, dropped
every chunk of real audio as late, played nothing, and reported **zero**
re-placements, because the error was perfectly consistent: the reading and the
timeline drifted together, so the slip check had nothing to see. Music Assistant
showed a player that was connected, available, and silent.

So the depth is measured from the near end instead, where there is no origin to
share. `audio_pending` sums the frames still in the buffer queue -- C records how
many each slot was enqueued with, and `GetState` says how many are still in
flight, which are the most recent that many slots -- and the HAL's `delay` is
added to it. `Chime.ahead` is that pair behind one call, with the moment of the
reading beside it: `Ahead` frames sit between the next frame the writer hands over
and the speaker, so that frame is audible at `At + Ahead/48000`.

It is unexported, and that is the whole of its thread safety. `audio_pending` sums
C state the writer mutates on every `audio_write` with no lock of its own, so the
one goroutine that writes is the only one that may read: a reading taken from
anywhere else would sum the wrong slots and answer a wrong-but-plausible pipeline
under the ceiling, which is once more the consistent error the slip check cannot
see. Exported, it would be one diagnostic call away.

The per-slot count is not the same as `count * 480`, and the difference is the kind
this page argues about elsewhere: the block size and the C chunk size are both 960
bytes in two languages with nothing tying them together, so the queue reports what
it was actually given rather than what a constant says it should have been.

It measures the same quantity the old arithmetic did, which is why the first
numbers taken this way agreed with it. `written - (position - delay)` is
`(written - position) + delay`, and `written - position` is the frames still in
our queue -- so the two formulas differ only in that one needs an origin and the
other does not.

Measured across five runs in the shape that broke -- a chime, six seconds of idle,
then a stream -- the player reported **131 to 144 ms**, and the streams placed
every frame. Before the change the same shape read 569 ms and dropped all of it.

**A reading outside nothing-to-a-second is refused, at both ends, and a negative
`delay` is refused on its own.** The queue is eight blocks and the HAL held 58 to
64 ms, so a second of pipeline is not a measurement -- and neither is a negative
one. Guarding the *sum* is not enough for that second half, which is the mistake
this paragraph used to describe as the fix: our own queue holds up to 3,840
frames, so any `delay` from -1 down to -3,840 still sums to something positive,
shallow and entirely plausible. A full queue over a delay of -3,000 reads as 840
frames rather than the 6,840 the two would hold between them, and the stream then
anchors 62.5 ms short and places every frame that far late -- consistently, which
is exactly the failure below.
So the sign is checked before the sum, and the test that covers it passes a full
queue rather than an empty one, because an empty queue is where the sum guard
happens to agree.

The check on the sum stays even so, and it is not the redundancy it looks like.
Both its terms are now known to be positive, so the sum cannot be negative by
addition -- but `delay` is parsed as any `int64`, and a driver reporting one near
the top of that range makes the sum **wrap**, which lands under the ceiling
rather than over it and reads as a pipeline behind the speaker. That is what the
lower bound on the sum catches, and there is a test that feeds the largest
`int64` to say so. All of it is refused for the same reason the failure above
gives: the mapping that comes out of a bad reading is wrong
*consistently*, which is the one shape the slip check cannot catch, and the sign
decides only whether the stream ends up early or late. Refused, it never places
the stream at all, and the stream says so when it closes. Whether this driver ever
reports a negative delay while `RUNNING` is not known; the guard is there because
the ceiling on the other side of the same sum is.

`Chime.ahead` holds the player's own lock and refuses after a close, because the
alternative is not a wrong answer but a crash: `audio_close` clears its pointer and
then destroys the object, so a reader that passed the null check a moment earlier
dereferences a freed vtable. The supervisor turns that into a five-second restart
with the button ungrabbed, so an ordinary shutdown would become an outage.

The two halves of a reading are taken around the stamp rather than before it. The
`/proc` read is the slow one, so it goes first, then `At`, then the queue, which
is a function call. Taking both after the stamp would make `delay` stale by
the length of the file read -- and stale one way: the HAL drains while the file is
being read, so the delay would be understated and every origin would come out
early by that much. Measured on a Dot over twelve readings, the `/proc` read costs
253 to 389 us, mean about 300 -- which is the whole of the bias, and the same size
as the tightest agreement this instrument has produced. A systematic error the
size of your best case is not noise that averages away.

A queue count `GetState` will not give, or one past the buffers the queue holds,
answers -1 rather than a number, because a queue nobody can read should look like
a failure rather than like an empty one -- an empty queue is a legitimate reading,
and it means the writer is about to fall behind.

The path to the status file is a constant here -- card, PCM and subdevice alike --
while `internal/device` globs for it, and the two are asking different questions: that one wants to know whether
*anything* is playing and will take any PCM, this one wants the delay on the
stream ours is mixed into, which is `pcm23p` on biscuit. The hazard the glob
exists for -- a path resolved while ALSA is still registering -- does not reach
here either, because this path is resolved on every call rather than cached, so a
file that is not there yet is an error this time and fine the next. A Dot that
enumerated its outputs under another card or subdevice would fail this read with
ENOENT every time rather than answer wrongly, which is the direction to fail in:
`Played` would go dark and say so, while `SpeakerPlaying` kept working.

**A reading is refused unless something of ours is playing**, and that is a
different question from whether the output is running. `GetPosition` counts frames
taken from *our* track; the `delay` beside it describes the shared queue that
stock Alexa is mixed into. Between our own sounds our track's position freezes
while hers does not, so a reading taken then subtracts her buffer from our frozen
count, and the origin it implies slides backwards a second per second for as long
as she talks. The `RUNNING` check does not catch it -- the output really is
running, just not for us. The mixer knows the answer, so the reading asks it.

Measured, sampling twice a second with nothing of ours playing: the output read
`RUNNING` with a delay wandering between 2,464 and 3,072 frames, for minutes at a
time, whether or not Alexa was making a sound. So the idle case is not an unlucky
window -- a reading taken any time we are not writing would have come back with a
plausible number built out of somebody else's queue, and the `RUNNING` check alone
would have passed every one of them.

What the guard costs is the tail. The mixer drains a clip into the player's queue
faster than the speaker empties it, so it goes idle while the last ~80 ms is still
audible, and readings stop before the sound does. Sampling every 50 ms across a
chime gives six readings over about 250 ms and then nothing. For a player being
fed continuously that is invisible; for a short sound it means the reading is
available while the audio is being written rather than while it is being heard.
Refusing there is the conservative direction: the alternative is a number that
looks right.

**A reading is also refused when the output is not `RUNNING`.** This is not a
theoretical guard: between two chimes four seconds apart, the status file read
`XRUN` with `delay: 0`, because the queue had drained and the HAL had stopped. A
zero delay there does not mean the queue had emptied into the speaker -- it means
there is no queue to describe, and taking it at face value would place our frames
about 57 ms later than they are.

Three presses, the origin sampled every ten blocks while the chime played. A
fourth was pressed and fell past the probe's own cap, so it carries no row:

| press | spread, all four samples | spread, after the first |
|---|---|---|
| 1 | 6.3 ms | 0.09 ms |
| 2 | 6.3 ms | 6.3 ms |
| 3 | 8.9 ms | 3.8 ms |

**The first sample of a sound is the one to distrust, and it is inconsistent about
which way it is wrong.** On press 1 it sat 6.3 ms early and the other three agreed
to 90 microseconds; on press 3 it sat 8.9 ms late; on press 2 it was
unremarkable and a later sample was the outlier instead. What is reproducible is
the frame count: every first reading came back at exactly **-3,056** frames, so the
writer is a fixed distance ahead when the first block lands and the variation is in
the HAL's delay rather than in our own position.

That is the cold-start finding above in miniature -- the buffer is filling while
the sample is taken. A scheduler that needs the origin should take it after audio
is flowing rather than at the first opportunity, and should expect its own best
case only once the stream is steady.

The `delay` itself held between 2,768 and 3,072 frames across all three presses,
which is 58 to 64 ms of HAL buffer.

### Playing audio at a time somebody else chose

`Stream` is what a network stream plays through: a mixer source like the chime,
but one whose samples carry the moment they are due rather than starting when
they arrive. `Chime.OpenStream` attaches one, `Write(at, pcm)` schedules audio at
a client-clock instant, `Clear` throws away what is buffered, `Finish` throws it
away and retires the source, and `Close` stops it where it stands so the writer
can go idle and the amp can reach standby. One at a time, for the same reason
there is one player: the second would be scheduled against the first's timeline.

The player's slot for it is an `atomic.Pointer` rather than a field under the
player's lock, and that is a race rather than a preference. A stream that has
been ended is retired by the writer, which runs the check at the top of
its loop -- and the writer then **blocks**, because a mixer with no source has no
block to ask for. So the slot stayed full for as long as nothing else made a
sound, and the next `stream/start` was refused with a stream already open: one
silent track, from a window about a block wide. Measured on hardware, Music
Assistant starts the next stream 60 ms after one ends, which is inside it.

Making the slot atomic is not sufficient on its own, and the second half of that
bug is worth the paragraph because the first fix looked complete. `Close` is
idempotent by early return, and the goroutine that *wins* the close goes on to
write a log line before it clears the slot. So a second caller finding the stream
spent calls `Close`, gets an immediate return because the flag is already set, and
then finds the slot still full -- refused again, with the window now as wide as a
write to `/data` rather than as wide as a block, and an error message naming a
stream that is not open but being retired. So `OpenStream` clears the slot itself
after closing a spent stream, rather than trusting the closer to have done it.
Both paths then converge: whoever gets there first retires it, the loser's `Close`
and the closer's own clear are both no-ops, and neither costs anything.

Retiring a stream and taking one up again are one locked step for the same class
of reason. `Resume` reports whether it took, because a check for spent followed by
a separate resume is two acquisitions with a hole in the middle: the writer can
retire the stream in between, and the caller is then left holding a handle the
mixer has already dropped, refusing every write for a whole track.

The writer's blocking is why it looks once more before it parks. The same shape as
the slot above, one step further on: a stream that retires itself is dropped by
the mixer during the block the writer just asked for, so the check at the top of
the loop already ran while the stream was live, and the writer then waits on a
mixer with nothing in it. `Close` is therefore never reached, and `Close` is the
only place the stream says what it did with the audio -- so a last track that
dropped every chunk reported nothing at all until something else made a sound.
Looking again on the way into the wait costs one call per idle period and closes
it.

What that report says when the stream was never placed is its own small
correction. It used to name what it dropped, and nothing can be dropped before
the mapping exists: dropping is counted while filling a block, and blocks are
only filled once the stream is placed. So the line read `dropped 0s` however much
audio the stream was holding when it went. It names what it threw away instead,
which is the number that is not zero.

`Chime.Close` retires the stream it still holds, after the writer has stopped and
before the player is destroyed. Otherwise the one path that actually reaches
`Close` -- the button's read loop failing, the daemon on its way out -- tears the
player down with a stream still attached, and the close report is the only place
the stream says what it did with the audio. A shutdown that happened to drop
every chunk said nothing at all. The handle stayed live as well, taking writes that
returned no error into a mixer whose writer had exited -- the end state the
paragraph below prevents for a race. That half is the smaller one, and saying so
is the point: `Close` is reached only as the daemon goes, so the window is the
milliseconds before the process exits rather than anything that outlives it. The
report is what was actually lost.

`OpenStream` does its check and its attach under one hold of the player's lock,
where it used to read `closed` and release it first. The window is a shutdown
only, which is why it is last here: a `Close` arriving in it leaves a stream
attached to a mixer whose writer has exited and whose OpenSL objects are gone, and
writes to it then succeed and play nothing. `Play` and `ahead` were already
check-and-act under that lock, so this is the one that was not. The lock order is
the player's lock then the mixer's, everywhere, and `Close` releases the player's
before it waits for the writer, so holding it across the attach adds no cycle.

**The mapping between a frame and a moment is measured rather than assumed, and
that is the whole of why the ~95 ms above is not subtracted anywhere.**
`Chime.ahead` says how many frames sit between the next one the writer hands over
and the speaker, so the stream is placed by asking rather than by declaring, and
what a fixed 95 ms would have to get right -- a cold start, a full queue, a
starve -- is read off the player each time instead.

The number that comes back is **not** the 95 ms, and the difference is the queue.
Measured over five probe runs on a Dot, the player reported itself **131 to
144 ms** ahead of the speaker: the eight-block queue the writer fills at once
(80 ms) plus the HAL's own 58 to 64 ms. The 95 ms is the interval from the first
write to the first audible frame, when the queue is still empty; 144 ms is what
the pipeline holds once the writer has run ahead, which is the steady state and
the one a scheduler has to place against.

**A stream plays silence until it has that mapping, and audio that arrives first
waits rather than being spent.** The alternative is placing a server's first
chunk against nothing, which is a quarter of a second of error nothing reports.

**The first reading of a sound is worthless and the next few are noisy, so the
mapping is the middle of five.** docs above measure the first sample of a chime
6.3 ms early on one press and 8.9 ms late on another, with the honest samples
agreeing to 90 us. A median of five outvotes two outliers wherever they fall,
which a mean does not and "wait, then take one" does not either.

Five readings is not enough on its own, because they can all be taken inside the
window that is wrong. The writer fills the whole queue in the first few
milliseconds, so ten blocks is 20 ms of real time rather than 100, and the HAL
buffer is still filling through all of it -- which is the cold-start bias above,
27 ms of it, arriving as a confident number. So the first reading waits
`anchorSettle`, **100 ms**, and the five then follow a block apart. Measured
across nine runs, a stream was placed 134 to 164 ms after it opened.

**What that costs is the lead a server has to give.** Audio cannot be placed
before the mapping exists, and once it does the earliest frame is the pipeline
depth away, so the first chunk of a stream is playable only if it is due later
than those two together: 164 + 144 ms in the worst run, about **308 ms**.
Measured with a probe writing 200 chunks of 1,200 frames, stamped like a server's,
each run preceded by a chime and six seconds of idle so the player was in the
state a daemon's really is:

| lead | placed | dropped late | re-placed |
|---|---|---|---|
| 500 ms | 5 s, all of it | 0 | 0 |
| 500 ms | 5 s, all of it | 0 | 0 |
| 500 ms | 5 s, all of it | 0 | 0 |
| 300 ms | 5 s, all of it | 0 | 0 |
| 200 ms | 4.88 s | 121.0 ms | 0 |

The 200 ms run is the arithmetic above coming true rather than a surprise: it was
about 100 ms short of what the stream needed, and the first 121 ms of the track
-- the chunks whose moment had passed before there was a mapping to place them
against -- were dropped. Everything after them played. The 300 ms run is the
margin: it cleared 308 ms by nothing much and lost nothing.
docs/sendspin.md carries what gets declared on the wire because of this.

The four clean runs are the result that matters, and it is the one that could
not be argued: **every frame placed, none late, and the mapping never moved.**
A 1,200-frame chunk and a 480-frame block never share a boundary, so continuity
across them was the thing most likely to be wrong, and five seconds of audio at
25 ms a chunk is 200 boundaries with no hole and no repeat at any of them. `dmesg`
carried one `underflow` pair per run, at the moment each ended, which is the
ordinary end-of-playback line rather than a starved writer.

**The mapping is re-measured but only rarely acted on.** A reading arrives every
tenth block once the stream is placed, and it moves the mapping only past
`anchorSlip`. Following every reading would chase the instrument's own
noise, and each correction is a skip forward or back in the audio; ignoring them
all would leave a stream that starved permanently behind the group.

**How big the threshold has to be is the part Music Assistant settled, and the
first answer was wrong.** At 20 ms the correction fired on nothing but the
instrument: the reading swings about **22 ms** out and back again, and it did so
three times over one session -- `+22, -22` at 18:56:33.96 and 18:56:34.22, then
the same pair at 18:57:49 and 18:58:01, each landing within a few milliseconds of
the same sub-second offset. That is about two blocks' worth of queue, which is the
size of the quantisation the reading is built out of: the queue count moves a
whole block at a time and the HAL's `delay` a period at a time. So the mapping was
dragged out and straight back, twice per episode, and each of those is an audible
skip that achieved nothing -- and a 22 ms error held for the 258 ms in between,
which is the one thing this is all supposed to prevent.

Requiring the slip to **persist** was the first fix and it was not enough. A run
of three readings suppressed the 258 ms episodes and then the next session showed
the same swing holding for **657 ms** -- `+21` at 19:03:45.96 and `-24` at
19:03:46.61 -- which is long enough to satisfy any persistence rule worth having.
The swing is not a blip to be filtered out; it is what this instrument does.

So the threshold is **50 ms**, about twice the largest swing measured, and the
persistence requirement stays at three readings alongside it. What justifies
sitting that far out is what the re-reading is actually for. The anchor is durable
by construction: the queue paces the output at exactly the DAC rate, so the frame
index and the clock cannot drift apart except through the 3.9 ppm rate error
above, which is 0.7 ms over a three-minute track. The only thing that can
invalidate it is a starve -- and a starve the instrument can actually distinguish
from its own noise is a big one. A threshold below that catches nothing real and
skips the audio to chase quantisation.

**This mechanism has never once been observed correcting a real slip.** Every
firing so far has been the instrument, and at 50 ms it has not fired at all: zero
corrections across three Dots playing one stream together for about ten minutes,
and none on any single-Dot run since the threshold moved. So the group test did
not condemn it -- it produced no evidence either way, which is what a guard
against a rare failure should produce. It stays because what it guards against is
silent and its own line is cheap.

**Two numbers in the per-chunk correction were measured rather than derived.**
`softStep`, `easeApart` and `snapAbove` come from the spec's suggested strategy
and from `sendspin-go`'s window -- `easeApart` is its 0.5% figure applied to how
often a frame may move, rather than to how far one chunk may be moved, so a
server sending short chunks is still corrected. These two do not.

- `deadBand` is **5 ms**, where the spec suggests ~100 us. The clock estimate
  wobbles by milliseconds, so at 100 us the correction chased the instrument:
  613 edits in 30 seconds, 303 of them undoing the other 303. At 5 ms the same
  run made 168, all one direction -- the real crystal difference.
- `smoothOver` is **50 chunks**, about a second. At 400 the average lagged far
  enough to overshoot each crossing and oscillate: 1163 edits in 30 seconds,
  nearly one per chunk.

**The queue is ordered by when audio is due, not by when it arrived.** Only the
head is ever examined, so one chunk stamped far ahead of the rest would sit there
and hold everything behind it: no audio placed, nothing counted late, nothing
logged, and silence for as long as that one chunk's lead. Ordering on insert costs
an append in the ordinary case, because a server's stamps arrive in order. A chunk
the mixer is partway through keeps its place regardless, since moving audio in
front of it would restart a sound mid-way.

Three bounds on what a peer's audio can cost, all refused rather than absorbed: a
stream holds at most `streamHold`, four seconds of frames, which is past the
2.4 seconds Music Assistant was measured filling to; a chunk due more than
`streamAhead`, thirty seconds, either way is refused, because a stamp near the
ceiling `onAClock` allows turns into a frame count that overflows on the way to a
frame index; and the queue holds at most `streamChunks` entries.

**That third one is the frame ceiling's blind spot, and the ordering above is what
makes it cost anything.** `streamHold` bounds frames, and a server chooses how
many frames a chunk carries, so one-frame chunks reach 192,000 entries under a
ceiling that is never tripped -- and an insert whose stamp is the earliest walks
the whole queue, under the same mutex the mixer takes for every 10 ms block.
Measured on a development machine, one frame per chunk with each stamp a
microsecond earlier than the last: 10,000 chunks cost 0.26 s, 50,000 cost 7.3 s,
and 200,000 filled the queue to 113,443 entries in 40 seconds of unbroken CPU. On
the Dot's ARM, a few megabytes on the wire buys minutes of a starved speaker, and
nothing exits, so the supervisor never restarts it. The ceiling is
`streamHold / BlockFrames`, four hundred: enough for any chunking down to one
mixer block with the buffer full, and small enough that the walk is free.

**A gap wider than a 32-bit frame count is clamped, not cast.** Walking to a
chunk that is not due yet advances by the gap, and `int` is 32 bits here, so a
gap past 2^31 frames -- 12.4 hours of audio -- truncates negative, `pos` goes
backwards, the loop's own condition stays true, and `fill` spins forever holding
the stream's mutex inside the mixer's. That is not a panic the supervisor
recovers from: the writer never returns, the chime blocks behind it, and the Dot
is silent with nothing logged until it reboots. Reproduced under
`linux/arm/v7`, which is the only place it exists: 25 of 31 step sizes sampled
between 11 and 26 hours wedged the reader permanently, and the same sweep out to
forty days is clean on amd64. So the gap is clamped to what is left of the block,
which needs no bound on the numbers going in.

Which is the correction to what this section used to claim -- that the thirty
seconds keeps every later conversion bounded, because the gap is a difference of
two numbers that each track the clock. It does not. `streamAhead` bounds a chunk
against **now**; the gap is measured against the mapping's origin, and nothing
bounds the distance between those two.

**Queued audio keeps the monotonic reading it arrived with.** A `time.Time` from
the Sendspin client carries both readings, and stripping the monotonic one --
`Round(0)`, which an earlier draft of `Write` did -- makes the subtraction in
`frameAt` fall back to the wall clock on both sides. Those two wall readings are
not comparable: the stream's origin is read live, and the client's reference was
frozen at daemon start, before wifi and before anything set the clock. So a
single wall-clock step, which is how Android sets the time and this Dot has no
RTC to avoid, moves every frame the stream places while the slip check reads
zero -- that check compares two monotonic readings and sees a perfect mapping.
Forward, the audio is dropped as late; far enough forward, it is the wedge above.
The guard is that the two sides of every subtraction carry the same clock, and the
test asserts the queue holds the exact `time.Time` that arrived rather than a copy
of it.

**A stream with no `stream/end` is never retired, and that is deliberate.** A
server can leave one open and simply stop sending: the source stays in the mixer,
the writer feeds it silence, and the amp never reaches standby, which is the cost
the section above says `Close` exists to avoid. An idle timeout looks like the
answer and is worse than the problem. Music Assistant keeps one stream open across
a track change, measured, so a pause that keeps the stream open is ordinary rather
than hostile -- and retiring the stream under it would make the audio that arrives
on resume land in a stream nothing is reading, which is a silent track for a
paused listener. What the peer gains is a warm amp and a hundred idle writes a
second; what an idle timeout would cost is audio. The trade only changes if a
server is seen doing it for long enough to matter.

**A `stream/clear` throws away the jitter buffer and nothing else.** Up to 80 ms
is already inside the player's queue and plays out. Clearing that as well means
`SLAndroidSimpleBufferQueueItf::Clear`, which flushes the `AudioTrack` under it,
and the player here is never stopped -- the one-way door the section above
describes. Eighty milliseconds at the far end of a seek is not worth that, so the
`reset` that used to exist stays gone.

## Testing audio here

**`chime_android.go` is the one file here no test covers, and it is where the
player's lifecycle lives.** It needs `GOOS=android` and an NDK, so CI cannot build
it at all, which puts the atomic slot, the look before the writer parks, and
`OpenStream`'s single hold of the lock outside the suite by construction -- every
one of them a race found by reading or on hardware rather than by a test. `Stream`
is deliberately the other side of that line: it is plain Go, it holds all of the
scheduling, and everything in it is tested. What the untestable half gets instead
is a run on a Dot with the log read afterwards, and the close report is the thing
to read, because it is the one line that says what the stream did with the audio.

**Never test with a sustained pure tone.** A 20-second 440 Hz sine pulses
audibly through every route -- ours at 48 kHz and at 44.1 kHz, and Alexa's own
24 kHz mono mp3 alike -- which reads as a device-wide output fault and nearly
closed out the whole question on false evidence. An Echo runs acoustic echo
cancellation continuously, because the mic array is always live, and a
stationary tone is what makes an adaptive canceller misbehave. A sweep through
the same routes is smooth.

**Keep two controls.** Play the same clip through Alexa's own
`SpeechSynthesizer`, which separates our code from the device; and play the file
on the development machine, which separates the file from both. Those two
located the fault when direct measurement could not.

**Know which output you are listening to.** With a cable in the jack, Android
routes to `headset` while raw ALSA writes bypass routing entirely and reach the
speaker. Two tests can differ for that reason alone. `STREAM_MUSIC` also carries
a level per route -- measured here, speaker 1 of 30 and headset 5 of 30 -- so
"nothing played" and "played where you were not listening, quietly" look
identical.

**Wait for the substream between trials.** Device 23 stays `RUNNING` for about
ten and a half seconds after a sound, so a trial started inside that window
measures nothing. Timed again here: `RUNNING` at 1, 3, 5, 7 and 9 seconds after a
chime, `XRUN` by 11.

**The Dot hums for that whole window, and it is not ours.** The amp stays powered
while the substream is up, so a sound is followed by about ten seconds of audible
noise floor. It is tempting to read that as something the daemon is doing --
a track left playing with an empty queue, say -- and it is not: the stock volume
up and down sounds do exactly the same thing, with no daemon involved. AudioFlinger's
standby decides the window. Two things follow. Nothing here can shorten it, and
`dmesg` is what separates it from the fault above, which sounds similar and is
ours: ordinary playback leaves about one `mtk_pcm_I2S0dl1_pointer underflow` line
per sound, at the moment it ends, rather than the flood that fills the ring
buffer.

**An injected press cannot be timed against the table above.** `sendevent` is one
event per process, so the four that make a press and its release cost 235-322 ms
of forks, measured -- an order of magnitude more than the sound is waiting for,
and the key-down lands somewhere inside that window with nothing to say where.
What the injection **can** do is compare two builds, since the overhead is the
same for both: `/proc/timer_list`'s `now at` and the substream's `trigger_time`
are both CLOCK_MONOTONIC, so the interval between them is one subtraction. Doing
that across the change from a clip player to a written one, five trials each:
93.8-122.2 ms before and 92.2-114.1 ms after, means 110.6 and 100.0. The spread
inside either set is wider than the gap between them, so the reading is that
writing the clip in chunks costs nothing measurable, and it is not a claim about
what press-to-sound is -- that is the resident cgo row above, measured another
way.

## The chime

`internal/audio`. A press is acknowledged by the daemon's own sound, played
through OpenSL ES, which is the layer AudioFlinger mixes. That is what lets it
coexist with Alexa: her speech is a track like ours rather than an owner of the
device. Writing to ALSA directly is not an option and fails worse than silently:
card 0 device 23 is the speaker, declares one substream, and the audio HAL holds
it open for the life of the boot, so a second opener gets `EBUSY`. The DSP
front-ends around it do accept a write and do reach the speaker, and doing so
corrupts the codec stream and leaves the driver logging
`mtk_pcm_I2S0dl1_pointer underflow` fast enough to empty the kernel ring buffer,
until a reboot. Every sound on the device pops in that state.

**The chime is a mixer source rather than the thing being played.** The daemon
holds one player and one writer; a press rewinds the chime's clip and hands it to
the mixer, which sums whatever is active into 10 ms blocks. With one source that
is a copy, and the reason it is built that way is the second source: a stream has
to be able to play through a press rather than be interrupted by it. A press no
longer clears the queue, because the queue will not belong to the chime alone, so
a second press inside the first chime hears up to 80 ms of it before the restart
-- measured as the queue depth rather than heard.

Handing a URL to Alexa's `SpeechSynthesizer` via `am startservice` was the route
before this, and it worked. What it cost was 691ms from press to sound against
33ms now, measured five trials each at the moment the codec substream goes
RUNNING. Nearly all of the difference was hers: binder to her service, an http
fetch, an mp3 decode. It also required a loopback listener alive for the life of
the daemon, four separate quirks of `SpeechInteractionManager`, one exact mp3
encoding, and a stack that a debloated Dot may not have running at all. None of
that is needed to make a sound.

It is needed to play one. `internal/alexa` keeps that route for the media
player, where every one of those costs is either irrelevant or somebody else's:
691ms is nothing against a clip somebody asked Home Assistant to speak, the
fetch and the decode are hers, and the listener is gone because the clip is
served by whoever asked for it. What stays is her encoding rule, which is the
part that fails in a way worth naming -- `cannot estimate length of the next mp3
frame` in `logcat -s tts-Server` is a variable bitrate she will not demux, and
it reads exactly like a file that is not there. Both end `Playback ended: ...
FAILED`, so the watcher keeps that line and reports it as the reason.

**The player is built once and held.** Building it per press was measured at
333ms, and almost all of that is `exec` plus linking `libOpenSLES`: the helper's
own work, from its `main` to the buffer queue, was 4ms. A resident process is
therefore the whole trick, and once one is resident it may as well be this one --
a separate helper measured the same 33ms while costing a second binary to push
and verify, a pipe, a child to supervise, and 10MB of its own.

**It is cgo, so the target is `GOOS=android` rather than `GOOS=linux`.** That is
not a preference. Go's linux runtime hangs before `main` against Bionic, so a
linux build does not fail, it stops -- with no output, which is what makes it
worth stating. Bionic also folds pthread into libc and ships no `libpthread`,
while cgo appends `-lpthread` regardless, so `build.sh` makes the empty stub
archives older NDKs used to carry.

The daemon pays about 9MB of RSS for holding the media stack in its own address
space, roughly doubling it, and CI keeps building for `GOOS=linux` because the
audio package is behind a build tag. What that buys is the tests and `go vet`
running exactly as they did, on the real 32-bit target, with the audio path
verified on the device like everything else that touches hardware.

**The sound is generated rather than stored**, which is why no asset is in the
tree and nothing needs ffmpeg to regenerate one. Two sustained sine tones a
fifth apart, A5 then E6, ramped at both ends and fading over the last tenth of
a second. Those
are the recording's own numbers rather than a choice: a DFT over each half of the
clip this replaces reads 880.0 Hz and 1320.0 Hz, within two cents of A5 and E6,
with no partial above the fundamental carrying enough energy to matter, and an
envelope that holds its level for three quarters of its length rather than decaying
like a bell.

Generating it once costs 12.7ms on the Dot and 39.4KB held for the run.
Generating it per press would spend that 12.7ms inside the 33ms budget, which is
most of what the change bought. The 39.4KB is 0.008% of what this device has, and
it is held as Go memory now that the player copies what it is written rather than
keeping a pointer to it. The pool it copies into is 8 x 960 bytes, so the resident
cost of making a sound is about 47KB. What that buys is a clip that can be
regenerated, or arrive over a network, without the queue holding a pointer into
it.
