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

`docs/architecture.md` says what the daemon does with that, and why the build
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
measures nothing.

## Rendering the chime

Generating it costs 12.7 ms on the Dot and 39.4 KB held for the run, against
0.008% of what this device has. Generating it per press would spend that 12.7 ms
inside the 33 ms budget, and the OpenSL ES buffer queue holds a *pointer* into
the PCM rather than a copy, so a clip regenerated under a second press is a
use-after-free rather than a slow chime.
