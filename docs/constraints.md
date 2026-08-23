# Hard constraints

**`GOARCH=arm` is not optional.** FireOS 5 on biscuit runs a 32-bit ARM
userspace even though the kernel is arm64. `struct input_event` is 16 bytes there
and 24 in a 64-bit userspace, so a wrong-arch build would desynchronise every
evdev read rather than fail. `internal/evdev` asserts the 32-bit `syscall.Timeval`
at compile time, so a 64-bit build does not compile at all. The guard is over
the word size rather than the architecture, so `GOARCH=386` compiles and passes
and nothing here would run on it. `build.sh` is the convenience, not the guard:
anything that compiles the tree, `go vet` included, needs the target set.

**Dependencies are pure Go, and rare.** The binary must cross-compile with
`CGO_ENABLED=0` and need nothing on the device. `proto.go` hand-rolls its wire
format because it is simple and a mistake is visible.

**A flag has to earn its place.** One flag, `-name`, the one thing that cannot
be defaulted. Every other fact about biscuit is a `const`, in `serve.go` or
beside the code that needs it. A flag whose only correct value is already known
is configuration that can be got wrong for no gain.

**The clip is served over http, from loopback.** `SpeechSynthesizer` casts the
connection to `HttpURLConnection` without checking, so `file://` throws
`ClassCastException` and a path on disk is not an option. `127.0.0.1` needs no
other machine and no firewall rule: FireOS's INPUT chain is policy DROP with a
port allowlist, but it accepts `lo` unconditionally. Measured on biscuit: the
fetch arrives from Alexa's own process (`Dalvik/2.1.0 ... AEOBC`) and
`tts-Server` reports `Playback ended: SUCCESS`.

The clip is embedded with `go:embed` rather than installed beside the binary,
so there is no second file to lose and no path to get wrong.

**Audio must be CBR 48 kbps / 24 kHz / mono MP3.** Alexa's demuxer reads the
MPEG header of the first frame and rejects everything else. Measured by
re-encoding one clip each way and playing it:

| Encoding | Result |
|---|---|
| **48 kbps, 24 kHz, mono** | **plays** |
| 32 kbps, 24 kHz, mono | `cannot estimate length of the next mp3 frame: data-type=44` |
| 64 kbps, 44.1 kHz, mono | ... `data-type=50` |
| 64 kbps, 48 kHz, mono | ... `data-type=54` |
| 64 kbps, 24 kHz, stereo | ... `data-type=84` |
| VBR (`-q:a 0`) | fails on whichever low-bitrate frame comes first |

`data-type` is the frame header's bitrate and sample-rate nibble, so this is
narrower than "CBR": one combination, and no way round it on the device. ID3 is
innocent, and so is transport: `Content-Length` and chunked both play.

**A Xing header is not innocent, and is the easy way to get this wrong.** The
demuxer reads the *first* frame, and `libmp3lame` writes the Xing/LAME info frame
as frame 0 at a bitrate of its own choosing. So `-b:a 48k -ar 24000 -ac 1` alone
yields a file whose first frame says 64 kbps, rejected with
`data-type=ffffff84`, while `ffprobe` reports the stream as 48 kbps and looks
correct. `-write_xing 0` drops it. When a clip is refused, read the first frame
header rather than the container; `TestChimeIsTheOneEncodingAlexaAccepts` does
exactly that, so a re-encoded clip fails in CI rather than on the device.
