# Audio

What it takes to make a sound on this Dot. One of the obvious routes damages the
device until a reboot.

## The topology

- Card 0 is `mtsndcard` with 26 PCM devices. The speaker is **device 23**,
  `TLV320AIC3204 Playback`, a discrete codec on I2S1. The MediaTek internal
  codec's amps all read `Off`. Routing is already live.
- Device 23 has **1 substream**. The audio HAL in `mediaserver` holds it open
  for the whole boot and idles in `XRUN` between sounds.
- At rest, `trigger_time` sits about 8,000 seconds behind `tstamp`. That is the
  normal steady state, not damage.

## Raw ALSA is closed, and trying it does harm

- A second open of `/dev/snd/pcmC0D23p` gets **`EBUSY`** with `O_NONBLOCK`, and
  blocks for ever without it. `tinypcminfo` hangs until killed.
- Front-ends 0, 2, 8, 20 and 25 accept a non-blocking open; 5 and 18 answer
  `EINVAL`. They reach the speaker, which is the trap.
- `tinyplay` to device 25 is audible and useless. The front-end has no clock of
  its own: `hw_ptr` resets to 112 again and again, and never passes about 7,568
  of the 240,000 frames in a 5-second clip.
- Played while Alexa speaks, her `hw_ptr` goes backwards (7360, 4544, 3072,
  1072): her playback restarts under ours.
- The damage outlives the writes. The driver logs
  `mtk_pcm_I2S0dl1_get_next_write_timestamp: MEM path to DL1 isn't enable` and
  `mtk_pcm_I2S0dl1_pointer underflow` fast enough to empty the kernel ring
  buffer. Every sound pops, and every playback measurement is off by about
  5.4%. Only a reboot clears it.

## AudioFlinger is the way in

- Alexa does not own the speaker. She opens an AudioTrack, and AudioFlinger
  mixes her with everyone else.
- `libOpenSLES.so` and `libwilhelm.so` are stock, so cgo in the daemon creates a
  track like any app. It plays cleanly while device 23 stays `RUNNING` and
  Alexa's stream is undisturbed.
- There is one player per process: the engine, player, queue and pool are C
  globals. A second `Chime` would overwrite the first's handles.
- `Close` returns early the second time. That makes `closed` mean "the player is
  going", and stops a second `close(c.stop)` panicking the daemon.
- `SLAndroidConfigurationItf` is optional; it only sets the stream type.

### The rate

- Each output runs at one rate. `/system/etc/audio_policy.conf` gives the A2DP
  output `sampling_rates 44100` and nothing else. The primary output (speaker
  and jack) lists `48000|44100`, but it opens at 48 kHz at boot, and every dump
  read 48,000.
- AudioFlinger converts a track at another rate. A 48 kHz stream to a Bluetooth
  speaker was resampled on the Dot, and a 44.1 kHz track twice: by the server,
  then by AudioFlinger.
- So the player opens at the stream's rate, and `internal/sendspin` asks the
  server for the output's rate (docs/sendspin.md). Over a JBL Go 3 the daemon's
  track then read 44,100 Hz on the 44.1 kHz output, where it had read 48,000.
  Back on the speaker it read 48,000 and `F`, a fast track, which AudioFlinger
  grants only to a track at its output's rate.
- Not 44.1 kHz everywhere: the speaker would then resample every stream, and a
  48 kHz source twice.
- A stream at another rate joins the mixer only after the writer closes the
  player and opens it again at that rate. No block of it plays at the old rate,
  and its 100 ms settle starts on the new player.
- Over Bluetooth, 2 streams that reopened the player were placed 220 and 255 ms
  after they opened.
- A chime sounding when the rate changes is cut off. At the new rate it would
  play off pitch.
- A player that will not open again stops the writer, as a failed write does.

### The writer and the queue

- Sounds are added to a mixer, not played. A second press rewinds the chime
  instead of stacking a copy.
- There is no stop-and-clear. `SL_PLAYSTATE_STOPPED` is a one-way door: nothing
  restarts the player, every later `audio_write` still reports success, and the
  Dot is silent for the rest of the boot.
- OpenSL's queue holds the **pointer** it was given until the buffer plays. The
  C pool copies each block, so the caller's bytes stay ordinary Go memory.
- A block is 10 ms (480 frames, 1,920 bytes of stereo), with 8 buffers: the
  queue holds 80 ms. That is also the scheduling quantum: a stream can start no
  more finely than one buffer. At 44.1 kHz the same block is 10.9 ms, and the
  queue 87 ms.
- A full queue is ordinary. The writer retries every 2 ms and gives up after
  1 second, about 12 times the queue's depth. Only a queue that has stopped
  draining reaches that: a wedged track, an AudioFlinger restart, or the driver
  state above. Nothing else signals that failure.
- The writer loops over short writes, because the Go block size and the C chunk
  size are both 1,920 bytes with nothing tying them together.
- Any failed write stops the writer, and it logs once:
  `the writer stopped: ...; the Dot is silent until the daemon restarts`.
- Seen once, 4 minutes after a Bluetooth speaker dropped mid-stream. The next
  stream opened the idle speaker output, which took 1.2 seconds to start its
  amp. 0.7 seconds after that, AudioFlinger logged
  `BUFFER TIMEOUT: remove(4096) from active list`, and 67 ms later the writer
  gave up. Alexa kept playing, and a daemon restart cleared it. The cause is
  not known: the same drop, idle gaps and restarts did not repeat it.
- `Close` does not wait for a sound to end. The writer exits at its next refused
  write, and the player is destroyed with up to 80 ms queued. The wait only
  stops anything writing into a destroyed player.
- The writer stops when nothing plays, and the device reaches standby on its
  own. Writing silence would hold the amp awake for the life of the daemon.
- `chime_android.go` and `audio_android.c` need `GOOS=android` and an NDK, so
  no test covers them. The mixer and `Stream` are plain Go and fully tested.

### What the queue does, measured

- The queue refuses at exactly `CHUNK * NUM_BUFFERS` bytes, so `GetState`'s
  count means what the arithmetic assumes.
- Buffers come back at the sample rate. That is the only rate feedback a writer
  gets.
- An underrun is not a stall. After 4 seconds idle the queue drained, refilled,
  and freed a slot with no reset and no second start. `audio_start` on a playing
  player answers 0 and changes nothing.
- ALSA cannot see our samples: `/proc/asound/card0/pcm23p/sub0/status`
  describes the HAL's stream. After 4 idle seconds it still read `RUNNING`, with
  `delay` at 2,752 frames.
- Ordinary playback leaves about 1 pair of `underflow` lines in `dmesg` per
  sound, at the moment it ends.
- A 30-second sweep (120 Hz to 6 kHz, 3,000 blocks) played with **no
  `underflow` line**. The writer made 12,897 attempts, so about three quarters
  were refused.
- So a healthy writer is refused often. Starving looks the opposite: every write
  accepted at once, and underflow pairs through the run.

## Latency is process startup, not hardware

Press to sound, timed to the codec substream going `RUNNING`, 5 trials each:

| route | mean | own work | RSS |
|---|---|---|---|
| Alexa's `SpeechSynthesizer` | 691 ms | -- | -- |
| native helper, spawned per sound | 366 ms | 4 ms | -- |
| Java via `app_process`, spawned per sound | 511 ms | 10-20 ms | -- |
| native helper, resident | 33 ms | 0.4-1.8 ms | 10.0 MB |
| Java, resident | 25 ms | 4.9-28.6 ms | 25.3 MB |
| cgo in the daemon, resident | 33 ms | 1.8-6.9 ms | +9 MB |

- A spawn costs 333 ms of `exec` and linking `libOpenSLES`. Residency is the
  whole saving, so the daemon holds the player.
- Java's jitter is its garbage collector. Java gets audio focus, and so ducking,
  through `ActivityThread.systemMain()` by reflection. Music ducks, but the
  process segfaults on teardown inside `app_process32`.

## The clock

- Take the DAC clock from `/proc/asound/card0/pcm23p/sub0/status`: `hw_ptr`
  with `tstamp`, from period interrupts on CLOCK_MONOTONIC, Go's `time` base.
- Over 292 seconds it fits a line at **48000.19 Hz** (+3.9 ppm), residual
  0.136 ms mean and 0.485 ms worst.
- Do not trust `AudioTrack.getTimestamp`. Three runs gave 48000.000, 48028.07
  and 48053.01 Hz, residuals 0.007, 19.3 and 24.1 ms, because
  `getMinBufferSize` returned 18432 on one run and 36864 on the next. A perfect
  48000.000 Hz is computed, not observed.

### How long until a written sample is heard

About **95 ms** from the first write to the first audible frame, with the queue
empty.

- The first frame is audible at `T - (position - delay)/48000`: `GetPosition`
  gives frames AudioFlinger has played from our track, and ALSA `delay` gives
  what the HAL still holds.
- 5 starts, 40 samples each:

| start | origin, after writing began | spread over the run |
|---|---|---|
| cold, from standby | 68.7 ms | 16.4 ms |
| warm | 95.7 ms | 3.3 ms |
| warm | 90.5 ms | 3.3 ms |
| warm | 97.1 ms | 13.5 ms |
| warm | 95.7 ms | 13.3 ms |

- The instrument floor is about 3 ms: `GetPosition` is quantised to 1 ms and
  moves in bursts, and `delay` moves a period at a time. **±1 ms is not a claim
  these instruments support.**
- The DAC timeline is known to 0.485 ms. Only the bridge to our frames is
  ±3 ms, and that mostly cancels between two Dots.
- 3 Dots playing one stream reported pipeline depths of 144, 142 and 141 ms,
  and sounded like one speaker.
- The figure depends on conditions: about 95 ms inside the daemon, about 75 ms
  from a standalone binary on the same Dot minutes later.
- The cold start reads 27 ms low with the widest spread. The HAL buffer is still
  filling, so `delay` understates.
- This is **not** Sendspin's `static_delay_ms`, which covers delay past the
  device's audio port. The queue does not add to it: the first frame is at the
  head of the queue.

### Asking the player how much it still holds

- **`GetPosition` is not a cumulative playback head.** It restarts from zero
  each time our track resumes, and read 0 for 6 seconds after a chime ended.
  Read as cumulative, it placed a stream 3.66 seconds out, dropped every chunk
  as late, and reported nothing wrong.
- So depth comes from the near end: `audio_pending` (frames in the buffer queue)
  plus the HAL's `delay`. `Chime.ahead` returns that with the moment of reading.
- `audio_pending` sums C state the writer mutates with no lock. Only the writer
  goroutine may call it.
- After a chime and 6 seconds idle, the player reported **131 to 144 ms** across
  5 runs: 80 ms of queue plus 58 to 64 ms of HAL buffer.
- A negative `delay` is refused on its own. The queue holds up to 3,840 frames,
  so a `delay` from -1 to -3,840 sums to a plausible depth and places every
  frame late.
- The sum is still checked against 0 to 1 second: a `delay` near the top of
  `int64` wraps it.
- `Chime.ahead` refuses after a close. `audio_close` clears the pointer and then
  destroys the object, so a racing reader would use a freed vtable.
- The `/proc` read is slow (253 to 389 us) and goes before the timestamp. Read
  after, `delay` is stale by the read, and every origin comes out early.
- `internal/device` globs for any status file because it asks whether anything
  plays. This package names one path because it wants the delay on the stream
  ours mixes into.
- **A reading needs something of ours playing.** Between our sounds our
  position freezes while Alexa's does not, and the origin slides back 1 second
  per second. With nothing of ours playing, the output read `RUNNING` with
  `delay` wandering from 2,464 to 3,072 frames for minutes.
- So readings stop about 80 ms before a sound ends: the mixer drains into the
  queue faster than the speaker empties it.
- A reading also needs the output `RUNNING`. Between 2 chimes 4 seconds apart
  it read `XRUN` with `delay: 0`, which is about 57 ms wrong.
- The first sample of a sound is off by several ms either way (6.3 ms early,
  8.9 ms late), while later samples agree to 90 us. The first frame count is
  always exactly **-3,056**; the variation is in the HAL's delay.
- The HAL's `delay` holds 2,768 to 3,072 frames: 58 to 64 ms.
- The HAL's `delay` counts 48 kHz frames whatever the stream's rate, and
  `a2dpDelay` is kept in the same frames. `point` converts both into the
  player's frames. Unconverted, a 44.1 kHz stream is placed about 5 ms off on
  the speaker and 36 ms off over Bluetooth.

## Playing audio at a time somebody else chose

`Stream` is a mixer source whose samples carry the moment they are due. There is
one at a time, as there is one player.

- The stream slot is an `atomic.Pointer`. The writer retires an ended stream and
  then blocks on an empty mixer, so a slot under the player's lock stays full
  until another sound plays. Music Assistant starts the next stream 60 ms after
  one ends.
- `OpenStream` clears the slot itself after closing a spent stream. `Close`
  logs before it clears the slot, so a second caller returns early and still
  finds it full.
- `Resume` reports whether it took. A separate spent check leaves a gap where
  the writer retires the stream.
- The writer checks the stream again before it parks. A stream that retires
  itself during a block would otherwise never reach `Close`, which is the only
  place it reports what it did with the audio.
- `Chime.Close` retires a held stream after the writer stops and before the
  player is destroyed. Otherwise the handle takes writes into a dead mixer with
  no error.
- `OpenStream` checks and attaches under one hold of the player's lock. Lock
  order is the player's, then the mixer's, then the stream's.

### Placing a stream

- The frame-to-moment mapping is measured, so the 95 ms is subtracted nowhere.
  `Chime.ahead` gives the frames between the next write and the speaker.
- A stream plays silence until it has the mapping. Audio that arrives first
  waits.
- The mapping is the median of 5 readings, a block apart, after `anchorSettle`
  (100 ms). The first reading is worthless and the next few are noisy. Without
  the settle, 5 readings span 20 ms while the HAL buffer still fills.
- Across 9 runs a stream was placed 134 to 164 ms after it opened.
- A server needs about **308 ms** of lead: placement plus pipeline depth,
  164 + 144 ms in the worst run. 200 chunks of 1,200 frames, each run after a
  chime and 6 seconds idle:

| lead | placed | dropped late | re-placed |
|---|---|---|---|
| 500 ms | 5 s, all of it | 0 | 0 |
| 500 ms | 5 s, all of it | 0 | 0 |
| 500 ms | 5 s, all of it | 0 | 0 |
| 300 ms | 5 s, all of it | 0 | 0 |
| 200 ms | 4.88 s | 121.0 ms | 0 |

- 1,200-frame chunks and 480-frame blocks never share a boundary. The clean
  runs crossed 200 boundaries with no hole and no repeat.

### Correcting it

- Once placed, a reading arrives every tenth block. It moves the mapping only
  past `anchorSlip`, **50 ms**, for 3 readings in a row. Each correction is an
  audible skip.
- The reading swings about **22 ms** out and back, about 2 blocks of queue, and
  a swing can hold for **657 ms**. A 20 ms threshold fired on that alone.
- The anchor cannot drift except through 3.9 ppm, 0.7 ms over a 3-minute track:
  the queue paces output at the DAC rate. Only a large starve invalidates it.
- At 50 ms it has not fired: 0 corrections across 3 Dots playing one stream for
  about 10 minutes. It stays because a real slip is silent.
- `deadBand` is **5 ms**; the spec suggests about 100 us. At 100 us the
  per-chunk correction chased the instrument: 613 edits in 30 seconds, 303
  undoing the other 303. At 5 ms: 168, all one direction.
- `smoothOver` is **50 chunks**. At 400 the average lagged, overshot each
  crossing and oscillated: 1,163 edits in 30 seconds.
- Every count and position here is in frames, and a copy, a trimmed or inserted
  frame, or a smoothed seam moves both channels together. One sample moved
  alone would swap left and right for the rest of the stream.

### Over Bluetooth

- A2DP never touches the MTK PCM device. While a speaker plays, the status file
  reads `XRUN` with `delay: 0`, so a reading from it fails and nothing is
  placed.
- The route comes from `/proc/net/unix`: a connected
  `@/data/misc/bluedroid/.a2dp_data` socket (state `03`) means audio goes to a
  speaker. The A2DP output never enters standby while a speaker is connected,
  so the socket stays up between sounds.
- The socket table is about 170 lines. A read costs about 1 ms, 3 times the PCM
  status.
- The socket is checked before the PCM. A sound mirrored to the speaker can
  leave the PCM running while ours goes over the air.
- The depth over Bluetooth is our queue plus `a2dpDelay`, 407 ms. Android
  reports 258 ms for the A2DP output: its 2,560-frame buffer at 44.1 kHz plus a
  flat 200 ms. A JBL Go 3 measured 149 ms more, against Dots on their own
  speakers.
- That figure belongs to one speaker model. The output delay can trim another
  one, and it applies on every route.
- The mixer pulls the A2DP output about every 66 ms, irregularly: 4 to 5, 7,
  or 11 to 12 blocks at a time. One reading lands anywhere in about 100 ms of
  that cycle. The mean of 50 readings holds within 2 ms, and the readings
  drift 59 ppm.
- The anchor's 5 readings share one burst, so a stream starts up to about
  50 ms off. A Bluetooth reading is marked `Bursty`, and the stream then:
  - averages its first 20 readings, about 2 seconds, and jumps the mapping
    there once. The jump cuts or pads up to about 50 ms; 25 ms measured.
  - follows a running average over 128 readings, about 13 seconds, and moves
    the mapping only past 4 ms. The audio follows, eased, once it is 5 ms out
    (`deadBand`). That tracks drift and walks a poor first settle in.
- A slip past 150 ms for 3 readings in a row, wider than the bursts ever swing,
  places the stream again at once and settles again. The average alone takes
  over 20 seconds to follow a 1-second slip.
- Easing is 1 frame per chunk: 0.83 ms a second with Music Assistant's
  1,200-frame chunks. Eased instead of jumped, a 25 ms first correction took
  28 seconds, heard as doubling in a group.
- Measured with a microphone against chirps scheduled on one server clock, 6
  fresh starts: the JBL settled at a median of 2.9 ms from a Dot on its
  speaker, from -2.7 to +14.9 ms, about 2 seconds in. Over 2 minutes it moved
  about 3 ms.
- A settle 11 ms off held for 20 seconds with the readings agreeing with it,
  so part of that spread is past what the Dot can see. The speaker's own
  buffer is the likely cause.
- Measure with both clocks converged. A fresh connection over a busy radio
  moved one Dot's clock 17 ms in 30 seconds, which the stream then eased.
- A change of output drops the mapping and any readings toward one, and the
  next 5 readings anchor it again, without the 100 ms settle a new stream
  waits. The depth moves by about 330 ms.
- **Switching back to the speaker mid-stream is not handled.** The speaker's
  output had been idle, and the re-anchor read 82 ms against a warm 141. The
  mapping was then placed again 3 times in 15 seconds, and the stream stayed
  off until the next one. A switch with the output already awake read 141.
- A server that changes format replaces the stream instead. The Dot asks for
  48 kHz once the speaker goes, and the new stream is placed afresh. Twice it
  was placed at 142 and 143 ms and played on.
- That costs about 1 second of audio at each disconnect: the new streams were
  placed 0.93 and 1.08 seconds after they opened. It is the route change, not
  the reopen or standby: a stream on a speaker idle for 3 minutes was placed in
  143 ms.
- Connecting costs about 0.3 seconds. The new 44.1 kHz stream's first chunk was
  due 343 ms ahead, and 320 ms was dropped as late.
- A stream opening over Bluetooth loses its start the same way. Music
  Assistant's first chunk was due 141 ms ahead of a 429 ms pipeline, and 548 ms
  was dropped. A 48 kHz stream over Bluetooth dropped 495 ms the same way.
- A longer lead and buffer over Bluetooth fix that start. docs/sendspin.md
  has them, under "Over Bluetooth".

### What a peer's audio can cost

- The queue is ordered by due time. Only the head is examined, so one chunk
  stamped far ahead would otherwise hold everything behind it, silently. A
  chunk the mixer is partway through keeps its place.
- `streamHold` is 30 seconds of frames, `aiosendspin`'s `max_duration_us`.
  `buffer_capacity` counts bytes, and FLAC through a quiet passage reaches that
  cap; PCM fills to 2.4 seconds. 30 seconds of 16-bit stereo is 5.8 MB, against
  a daemon resident at 9.3 MB and about 105 MB available on the Dot.
  `streamAhead` refuses a chunk due more than 30 seconds either way.
- `streamChunks` (3,000) bounds entries, because a server picks the frames per
  chunk. With 1-frame chunks, an earliest-stamped insert walks the whole queue
  under the mutex the mixer takes for every block. On a development machine:
  10,000 chunks cost 0.26 seconds, 50,000 cost 7.3 seconds, 200,000 reached
  113,443 entries in 40 seconds of unbroken CPU.
- A gap past 2^31 frames (12.4 hours) is clamped, not cast. Cast to the 32-bit
  `int`, it goes negative and `fill` spins for ever inside the mixer's lock: the
  Dot is silent with nothing logged until reboot. `streamAhead` does not bound
  that gap: it bounds a chunk against now, not against the mapping's origin.
- `frameCount` splits a duration into whole seconds and the rest before it
  multiplies by the rate. Nanoseconds times 48,000 overflow `int64` after 53
  hours, and the time since the origin grows for as long as one stream is open.
  Past that, every chunk was placed wrongly.
- Queued audio keeps its monotonic reading. `Round(0)` would make `frameAt`
  fall back to the wall clock, and a wall-clock step would move every frame
  while the slip check reads zero.
- A stream with no `stream/end` is never retired. Music Assistant keeps one
  stream open across a track change and a pause, so an idle timeout would lose
  the audio on resume. The cost: a server that stops sending keeps the amp
  awake.
- `stream/clear` empties the jitter buffer only. Up to 80 ms already in the
  queue plays out; clearing it would need
  `SLAndroidSimpleBufferQueueItf::Clear`, which flushes the `AudioTrack`.

## Testing audio here

- **Never test with a sustained pure tone.** A 20-second 440 Hz sine pulses
  through every route, Alexa's included, and reads as a device-wide fault. The
  Echo's echo canceller misbehaves on a stationary tone. Use a sweep.
- Keep two controls: the same clip through Alexa's `SpeechSynthesizer`, and the
  file on the development machine.
- With a cable in the jack, Android routes to `headset`, but raw ALSA writes
  still reach the speaker. `STREAM_MUSIC` has a level per route.
- Device 23 stays `RUNNING` for about 10.5 seconds after a sound (`XRUN` by
  11). Wait for it between trials.
- The Dot hums while the substream is up. The stock volume sounds do the same
  with no daemon running.
- `sendevent` forks once per event, so an injected press costs 235-322 ms. Use
  it to compare two builds, not to time latency.

## The chime

- A press plays the daemon's own sound through OpenSL ES, as a track beside
  Alexa's.
- The chime is a mixer source so a stream plays through a press. A second press
  inside the first chime hears up to 80 ms of it before the restart.
- The player is stereo, and the chime is the same in both channels. The
  speaker sums them, so a chime in one channel would sound 6 dB quieter.
- Alexa's `SpeechSynthesizer` takes 691 ms against 33 ms, and needs four quirks
  of `SpeechInteractionManager`, one exact mp3 encoding, and a stack a debloated
  Dot may not run.
- `internal/alexa` keeps that route for the media player, where those costs do
  not matter. Her encoding rule: `cannot estimate length of the next mp3 frame`
  in `logcat -s tts-Server` means a variable bitrate she will not demux. It
  looks like a missing file, since both end `Playback ended: ... FAILED`, so the
  watcher reports that line as the reason.
- A separate resident helper also measured 33 ms, but costs a second binary, a
  pipe, a child to supervise, and 10 MB.
- The target is `GOOS=android`. Go's linux runtime hangs before `main` against
  Bionic, with no output. Bionic folds pthread into libc while cgo still links
  `-lpthread`, so `build.sh` makes empty `libpthread.a` and `librt.a` stubs.
- Holding the media stack costs about 9 MB of RSS. The package is behind a
  build tag, so `GOOS=linux` still builds, with a stub player.
- The tones are the original recording's, measured by DFT: 880.0 Hz and
  1320.0 Hz, within 2 cents of A5 and E6, with no partial above the fundamental
  that matters.
- The chime is generated at both rates, once at startup. A mono 48 kHz clip
  took 12.7 ms on the Dot; the stereo clips are not timed. The resident cost is
  about 163 KB: clips of 76,800 and 70,560 bytes, and the 15,360-byte pool.
