# Architecture

## Button interception

`internal/evdev`, `intercept` in `main.go`. `event1` carries the action button
*and* mute, so an exclusive `EVIOCGRAB` takes both. The fix is a `uinput` clone
advertising exactly the real key bitmap, named `mtk-kpd` so Android applies the
same keylayout; 138 is consumed and the rest re-emitted. `EventHub` picks the
clone up by inotify. Read the bitmap with `EVIOCGBIT`, never from sysfs: that
file's word size differs from `/proc/bus/input/devices` here, and guessing wrong
silently breaks mute.

A failed grab is fatal. Without the grab the real node still delivers to
`EventHub`, so a clone that echoed anyway would land every key twice and mute
would toggle on and straight back off. Exiting hands the button back to Alexa
and takes the clone with it; a live clone beside a live original does not.

A failed re-emission is fatal for the same reason. Writes fail for the clone
rather than for one key, so carrying on holds the grab with mute going nowhere,
and nothing restarts a daemon that has not exited. Exiting releases the grab and
the supervisor builds a new clone five seconds later.

The clone is destroyed before the grab is released, and not the other way round.
The read loop can still be running when the close comes, so the reverse order
opens the same window: `event1` ungrabbed and the clone still live, and a key
pressed inside it lands twice. Losing that key is the cheaper failure.

## Speech

`internal/alexa/say.go` hands a URL to Alexa's `SpeechSynthesizer` via `am
startservice`, so she mixes and ducks it like her own speech. Writing PCM is not
an option: `mediaserver` holds card 0 device 23 permanently, so it contends and
stutters.

`/system/priv-app/SpeechInteractionManager` enforces four things:

1. **Two parsers read the same intent** and want different shapes. One throws
   `IllegalArgumentException: Namespace null` without singular
   `namespace`/`name`/`payload`; the other needs the string arrays
   `namespaces`/`names`/`payloads`/`payloadVersions`. Send both.
2. **This build's `am` does not unescape `\,` in `--esa`.** A JSON payload with
   commas arrives as several broken array elements and fails with `Invalid
   JSON`, so the array payload carries only `{"url":"..."}`.
3. **`sequenceId` is required** alongside `directiveId`, both plain string
   extras. Omit either and the play throws a bare `IllegalArgumentException`,
   logged as `Unable to play` and naming no field, while `am` still exits 0.
   The values are not keys: the same pair sent twice plays twice, because
   `SpeechInteractionManager` mints its own queue id. They are still worth
   choosing. `directiveId` carries the pid and `sequenceId` counts presses
   within it, and both appear verbatim in the `SPCH-SIM_*` and
   `SpeechSynthesizerAgent-TtsEventListener` lines, which is the only way to
   follow one press through to its `onSpeechEnded`.
4. **The URL must be `http://`.** `file://` fails with `ClassCastException:
   FileURLConnection cannot be cast to HttpURLConnection`, because the code
   casts unconditionally.

The clip is served from a loopback listener that lives as long as the daemon.
If it ever stops the daemon exits, because nothing else would notice: the URL
would still be handed over, `am` would still exit 0, and every press for the
rest of the boot would be logged and silent. Exiting hands the supervisor a
restart, and the kernel drops the grab and the clone with the descriptors.

`am` exits 0 whether or not it found the service, printing `Error: Not found; no
service started.` and returning zero, so its output is read as well as its
status. That is exactly the state a suppressed Alexa stack leaves behind, and
without the check the play reads as accepted, and what surfaces is the device's
own two-minute timeout rather than a reason.
