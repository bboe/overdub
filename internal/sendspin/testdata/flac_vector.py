"""Write the FLAC streams that flac_test.go decodes, with the reference encoder.

    uv run --with 'aiosendspin[server]==10.2.1' python testdata/flac_vector.py

aiosendspin-stereo.flac is aiosendspin's own encoder, so its header and frames
are what a server sends a player that asked for flac: 2 blocks of the pattern,
then 1 of silence. stereo-<layout>.flac is 1 short block through ffmpeg with
its channel layout forced, 1 file for each of FLAC's 4 stereo layouts. The
audio is integers only, so flacStereo in flac_test.go rebuilds it exactly. Each
file is the header followed by the frames, which is itself a FLAC stream.
"""

from __future__ import annotations

import struct
import sys
from pathlib import Path

import av
from aiosendspin.audio.codecs import FlacEncoder

FRAMES = 3
HERE = Path(__file__).parent
LAYOUTS = {
    "independent": "indep",
    "left-side": "left_side",
    "mid-side": "mid_side",
    "right-side": "right_side",
}
PERIOD = 200
RATE = 48000


def forced(mode: str) -> bytes:
    ctx = av.AudioCodecContext.create("flac", "w")
    ctx.sample_rate, ctx.layout, ctx.format = RATE, "stereo", "s16"
    ctx.options = {"ch_mode": mode, "compression_level": "0"}
    ctx.open()
    frame = av.AudioFrame(format="s16", layout="stereo", samples=ctx.frame_size)
    frame.sample_rate = RATE
    frame.planes[0].update(pack(pattern(ctx.frame_size)))
    packets = list(ctx.encode(frame)) + list(ctx.encode(None))
    header = bytes(ctx.extradata)
    return (
        b"fLaC\x80"
        + len(header).to_bytes(3, "big")
        + header
        + b"".join(map(bytes, packets))
    )


def main() -> int:
    (HERE / "aiosendspin-stereo.flac").write_bytes(reference())
    for name, mode in LAYOUTS.items():
        (HERE / f"stereo-{name}.flac").write_bytes(forced(mode))
    for f in sorted(HERE.glob("*stereo*.flac")):
        print(f"{f.name}: {f.stat().st_size} bytes")
    return 0


def pack(samples: list[int]) -> bytes:
    return struct.pack(f"<{len(samples)}h", *samples)


def pattern(n: int) -> list[int]:
    out = []
    x, y = 1, 12345
    for i in range(n):
        x = (x * 1103515245 + 12345) & 0xFFFFFFFF
        y = (y * 1103515245 + 12345) & 0xFFFFFFFF
        tri = i % PERIOD
        if tri >= PERIOD // 2:
            tri = PERIOD - tri
        left = tri * 160 - 8000 + (x >> 24) - 128
        out += [left, (left >> 1) + (y >> 25) - 64]
    return out


def reference() -> bytes:
    encoder = FlacEncoder(bit_depth=16, channels=2, sample_rate=RATE)
    header = encoder.get_header()
    block = encoder.frame_samples
    samples = pattern(2 * block) + [0] * (2 * block)
    frames = [
        f
        for f, _ in encoder.process(
            pack(samples), 0, FRAMES * block * 1_000_000 // RATE
        )
    ]
    if len(frames) != FRAMES:
        message = (
            f"the encoder made {len(frames)} frames of {block} samples, want {FRAMES}"
        )
        raise SystemExit(message)
    return header + b"".join(frames)


if __name__ == "__main__":
    sys.exit(main())
