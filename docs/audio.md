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
the second rather than documenting the rule, and `Close` is safe to call twice,
because the caller's signal handler and its defer can both reach it.

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
silent for the rest of the boot. That is a worse thing to leave lying in a header
than to write again, so the slice that needs `stream/clear` adds a clear that ends
playing, and measures it.

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
not. So the open question is not whether sub-millisecond is reachable but whether
this offset is the same on every start and on every unit -- and 6.6 ms across
starts is, as measured here, indistinguishable from the instrument's own noise.

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
playing together.

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
port, and a Dot's speaker is on the near side of it. The 95 ms is the client's
own to subtract from a timestamp before scheduling, which docs/sendspin.md
explains at more length and with the trap that makes declaring it look like it
works. The eight-block queue does not add to it either -- the first frame is at
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

### Asking the player where it is

`Chime.Played` holds the player's own lock and refuses after a close, because the
alternative is not a wrong answer but a crash: `audio_close` clears its pointer and
then destroys the object, so a reader that passed the null check a moment earlier
dereferences a freed vtable. The supervisor turns that into a five-second restart
with the button ungrabbed, so an ordinary shutdown would become an outage.

`Chime.Played` is that pair of instruments behind one call: `GetPosition` on
`SLPlayItf` for the frames AudioFlinger has taken from our track, minus the ALSA
`delay` for what the HAL still holds, with the moment of the reading beside it. A
caller wanting the origin computes `At - Frames/48000`; a caller wanting to
schedule frame N plays it at `origin + N/48000`.

`Frames` is signed on purpose, and it really does go negative: measured on a Dot,
the first reading after a sound starts reported **-2,704** frames. That is not an
error. It says our audio is queued and none of it has reached the DAC yet, and how
much is still to come is exactly what a scheduler wants to know. Discarding the
sign would turn "56 ms early" into "here now".

The two halves of a reading are taken around the stamp rather than before it. The
`/proc` read is the slow one, so it goes first, then `At`, then `GetPosition`,
which is a function call. Taking both after the stamp would make `delay` stale by
the length of the file read -- and stale one way: the HAL drains while the file is
being read, so the delay would be understated and every origin would come out
early by that much. Measured on a Dot over twelve readings, the `/proc` read costs
253 to 389 us, mean about 300 -- which is the whole of the bias, and the same size
as the tightest agreement this instrument has produced. A systematic error the
size of your best case is not noise that averages away.

A position of `SL_TIME_UNKNOWN` is refused rather than returned. `GetPosition` can
answer `SL_RESULT_SUCCESS` and still hand back `(SLmillisecond)-1`, which Android
does when the underlying `AudioTrack` is not there yet. Unsigned, that is
4,294,967,295 ms: positive, so a sign check passes it, and about 49.7 days of
audio. The C side answers -1 instead, because a position nobody knows should look
like a failure rather than like a confident number.

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

## Testing audio here

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
