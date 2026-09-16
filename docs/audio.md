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

**The player is written to rather than handed a clip.** `audio_open(rate,
channels)` builds it empty; `audio_write` copies up to one chunk into a pool of
its own and enqueues that; `audio_start` sets it playing; `audio_reset` stops it,
clears the queue and drops what was queued. A sound is reset, written, started,
which is exactly the order the old single `audio_play` did those three things in,
so the chime behaves as it did -- a second press restarts it rather than queueing
a second copy behind the first.

The copy is the point of the pool. OpenSL's queue holds the **pointer** it was
given until the buffer has played, so whatever is enqueued has to outlive the
sound; the clip was therefore allocated in C and freed only at `Close`. Copying
at the write makes the caller's bytes ordinary Go memory that nothing outlives
the call to, which is what lets a stream arriving over the network be written
straight through later, and it retires the use-after-free named at the end of
this page. Eight buffers of 16 KB is 128 KB, about 1.37 seconds at 48 kHz mono,
and a write beyond that is refused rather than overwriting a buffer that is still
playing: `GetState` reports how many are in flight, and a full queue takes
nothing.

The ceiling on a single sound did not move -- the chime is primed whole before it
is started, so it still has to fit -- and it is still checked once, before the
player is built, against `audio_capacity` rather than against a number written
down twice. That check is worth keeping where it is: without it the daemon starts
normally and every press fails instead, which is a line per press for a clip that
was always too big. Measured by growing the chime to two seconds: the startup
warning reads `the chime is 192000 bytes against a 131072 byte queue`, presses
are silent as they were before, and nothing is logged per press.

The feeder behind it is Go rather than cgo, so it is tested off the device. It
refuses a player that takes nothing, and one that claims more bytes than it was
offered, which would otherwise slice past the end of the clip. It is the
**chime's** feeder: a full queue is a failure to it, because the chime is written
in one go and has just cleared the queue itself, so there is nothing to wait for.
A stream is the other case entirely -- a full queue is the ordinary state to wait
out -- and it wants a loop paced by the clock rather than this one. Measured, with
the probe above holding the queue full: feeding the chime into it fails with
`the player took 0 bytes of the 38400 it was offered`, which is the feeder
refusing to wait, correctly, in the one situation a stream is in constantly.

A partial trailing frame is refused rather than padded or held: `audio_write`
rounds down to whole frames and answers -1 when nothing whole is left. For the
chime that is unreachable, and for a stream it says that splitting a frame across
two writes is the caller's mistake rather than something the player will paper
over.

### What the queue does, measured rather than read

The three things a stream will depend on had never run: the chime writes 38,400
bytes into a 131,072-byte queue and resets before every sound, so it never fills
the queue, never underruns, and never starts an already-playing player. A
throwaway probe in the daemon exercised all three on a Dot, writing silence so
that none of it made a sound.

- **The refusal is exact.** The queue took 131,072 bytes and then refused, which
  is `CHUNK * NUM_BUFFERS` to the byte, so `GetState`'s count means what the
  arithmetic assumes.
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

**One number in there is a warning for synchronised playback.** A 16 KB buffer is
170 ms of audio, and the queue is the smallest unit anything here can observe or
schedule against, so the write path as it stands cannot place a sound inside a
window finer than one buffer. The plan's target is ±1 ms. Nothing needs to change
while the chime is the only caller -- it is started, not scheduled -- but a
stream that must start on a timestamp wants buffers of about ten milliseconds
rather than a hundred and seventy, and that is a decision for the slice that
schedules, made with this number in hand.

`audio_reset` sets the pool back to its first slot, which is safe only because
`Clear` on a stopped player releases the buffer the mixer was reading before it
returns. That was not a property the old code needed: what it enqueued was the
one immutable clip, so a pointer surviving `Clear` would still have read valid
PCM. Now the next write copies over that buffer, so the ordering is load-bearing.

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
keeping a pointer to it. The copy is not free, though: the pool it copies into is
a 128KB static array, four times the clip and paged in as it is written, so the
resident cost of making a sound is about 166KB rather than 39.4KB. Still 0.03% of
the device, and it is what buys a clip that can be regenerated, or arrive over a
network, without the queue holding a pointer into it.
