# Hard constraints

## The target

- overdub runs on rooted Fire OS 6. It has no `app_process`, AudioFlinger or
  OpenSL ES, and sound goes through Amazon's mixer. A Fire OS 5 Dot needs a
  release from before the move.
- `GOARCH=arm` is required. Every model runs a 32-bit ARM userspace,
  `armeabi-v7a`: biscuit and radar on an `armv7l` kernel, donut on an arm64
  one (`armv8l`).
- `struct input_event` is 16 bytes there and 24 bytes in a 64-bit userspace. A
  wrong-arch build would desynchronise every evdev read, not fail.
- `internal/evdev` asserts an 8-byte `syscall.Timeval` at compile time, so a
  64-bit build does not compile. The guard is on word size, not architecture:
  `GOARCH=386` compiles and passes, and nothing here runs on it.
- Anything that compiles the tree needs the target set, `go vet` included.
  `build.sh` is a convenience, not the guard.

## cgo and the mixer

- Amazon's mixer is the only way to make a sound that is mixed with Alexa's
  rather than fighting it. `internal/audio` reaches it through cgo for the
  chime and the Sendspin stream, loading `libmixerAPI.so` with `dlopen`.
- So the target is `GOOS=android`, and the build needs an NDK. `libmixerAPI.so`
  is stock on the device.
- `chime_android.go` builds only for `android` with cgo. Elsewhere
  `chime_other.go` stands in, so `go vet` and the tests run for linux/arm.
  docs/audio.md says what that stub costs.
- `internal/opus` decodes Alexa's uploads the same way, loading the device's
  `libopus.so` with `dlopen`, so no Opus library enters the tree. Its stub is
  `decode_other.go`. docs/voice.md has the format.

## Flags and settings

- One flag, `-name`: the one thing that cannot be defaulted. Every other fact
  about biscuit is a `const` in `serve.go` or beside the code that needs it.
- Conditional things are read from the filesystem: the ESPHome key at
  `noiseKeyPath`, the Sendspin identity at `sendspinKeyPath`, which the
  daemon creates when it is missing, the Sendspin pairing records at
  `sendspinPairsPath`, which a pairing writes, and the last server to play
  over Sendspin at `sendspinLastPath`.
- A flag whose only correct value is known, or that restates whether a file
  exists, is configuration that can be wrong for no gain.
- Home Assistant sets three properties, and all outlive a reboot:
  `persist.overdub.sendspin` (the Sendspin switch),
  `persist.overdub.sendspin_delay` (its output-delay number) and
  `persist.overdub.voice_mode` (the voice select). They are not flags: nothing
  sets them at install time. A Dot with none written starts with Sendspin on,
  no delay, and voice in `pass through`. docs/device.md has the mechanism.
- `-version` asks the binary what it is, for a binary installed from a release.
  `build.sh` stamps `$OVERDUB_VERSION` through `-ldflags -X main.version`; the
  release job sets it from the tag. A local build leaves it empty and prints
  `overdub (unversioned build)`.
- The variable has a prefix because it is ambient. `install.py` runs
  `build.sh` in the caller's environment, and a bare `VERSION` from another
  project would stamp a binary nobody asked to stamp.

## Dependencies

- There are 3 direct dependencies: flynn/noise, x/net and mewkiz/flac.
  Everything else is hand-rolled where that is checkable in isolation,
  including the protobuf in `internal/esphome/proto.go`.

| dependency | why | licence | linked | size |
|---|---|---|---|---|
| `github.com/flynn/noise` | the Noise handshake | BSD-3-Clause | yes | -- |
| `golang.org/x/crypto` | the Noise ciphers | BSD-3-Clause | yes | -- |
| `golang.org/x/net` | `dns/dnsmessage` for mDNS, `http2` for Alexa's session | BSD-3-Clause | yes | 69 KB |
| `golang.org/x/text` | required by `x/net/http2`, indirect | BSD-3-Clause | yes | 1.3 MB |
| `golang.org/x/sys` | required, indirect | BSD-3-Clause | no | -- |
| `github.com/mewkiz/flac` | FLAC from a Sendspin server or Home Assistant | Unlicense | yes | 71 KB |
| `github.com/icza/bitio` | required, indirect | Apache-2.0 | no | -- |
| `github.com/mewkiz/pkg`, `github.com/mewpkg/term` | required, indirect | Unlicense | no | -- |
| Go standard library | every Go binary | BSD-3-Clause | yes | -- |

- x/text arrives with `x/net/http2`, which needs `httpguts` and so `idna`. It is
  the one dependency nothing here chose, and the most expensive: routing Alexa's
  session through this daemon took the binary from 8,193,116 to 9,291,100
  bytes, 13% for http2 and idna's Unicode tables together.
- flynn/noise: the standard library has X25519 (`crypto/ecdh`) but no
  ChaCha20-Poly1305, and the Noise pattern is not one to hand-roll. `go.sum`
  is the lock.
- The cipher suite is fixed. Home Assistant's client hardcodes
  `Noise_NNpsk0_25519_ChaChaPoly_SHA256` and mixes it into the handshake hash,
  so both ends must name it identically.
- A wrong key fails the Poly1305 tag. The log says
  `chacha20poly1305: message authentication failed`.
- dnsmessage: parsing is the hard half. Compression pointers are a
  peer-supplied graph, arriving from the whole subnet before any
  authentication, and every bound must be right at once. A passing suite proves
  less there than anywhere else.
- Its closure outside the standard library is itself. Every mDNS library routes
  through `github.com/miekg/dns`: about 22,000 lines, and the smallest such
  library adds 11 packages.
- mewkiz/flac: FLAC is lossless, so a decode is checked sample for sample
  against what was encoded, which is the test a hand-rolled decoder would need
  too. Only its decoder is linked: its 3 indirect modules serve the encoder, and
  the binary holds no symbol from them. docs/sendspin.md has its 2 traps.

## Licences

- BSD clause 2 asks that the notices travel with a binary. `THIRD-PARTY.txt`
  carries them, and the release tarball includes it.
- The NDK's clang does the link, because of cgo. It embeds Bionic's startup
  objects (`_start`, `__libc_init`), whose BSD-2-Clause notice is reproduced.
- It also embeds compiler-rt's ARM division builtins. compiler-rt is named but
  its licence is not reproduced: the LLVM exception waives sections 4(a), 4(b)
  and 4(d) for code embedded by compiling it.
- Bionic's libc is not in the binary. `libc.so`, `libdl.so` and `liblog.so`
  are `NEEDED` entries the device resolves. `libmixerAPI.so` and
  `libopus.so` are loaded at run time and are the device's own.

## Alexa's session

- It runs on every Dot that installs the daemon, whatever the voice mode. The
  mode chooses only what the relay does with the session, so `pass through`
  still exercises the install and the uninstall.
- One LIPC property outside this daemon's namespace is written,
  `com.amazon.puffin AVSEndpoint`, and two files outside `/data`, under
  `/system`. All three are undone by `uninstall.py`, and docs/voice.md says
  what happens if they are not.
- In `intercept`, one `OUTPUT` rule cuts Amazon's hybrid engine off the
  network. It lives only in the chain, and `uninstall.py` deletes it.

## Sound

- The chime and the error tone are generated, not stored or encoded.
  `internal/audio` renders sine tones: the chime's at the frequencies Amazon's
  own chime uses, measured by DFT. docs/audio.md has the numbers.
- A clip is played by the daemon. Fire OS 5 handed Alexa the URL, and her
  synthesizer decoded it; Fire OS 6 has no way in. Home Assistant transcodes
  the URL to FLAC, and the decoder Sendspin already links decodes it, so no
  new decoder enters the tree. docs/api.md has the search and the route.
