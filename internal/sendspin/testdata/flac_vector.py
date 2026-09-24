"""Write the FLAC stream that flac_test.go decodes, with the reference encoder.

    uv run --with 'aiosendspin[server]==9.1.1' python testdata/flac_vector.py

The encoder is aiosendspin's own, so the header and frames are what a server
sends a player that asked for flac. The audio is integers only, so
flacPattern in flac_test.go rebuilds it exactly: 2 blocks of a triangle wave
with noise from a fixed LCG, then 1 block of silence. The file is the header
followed by the frames, which is itself a FLAC stream.
"""

from __future__ import annotations

import struct
import sys
from pathlib import Path

from aiosendspin.audio.codecs import FlacEncoder

RATE = 48000
OUT = Path(__file__).with_name("aiosendspin-mono.flac")


def pattern(n: int) -> list[int]:
    out = []
    x = 1
    for i in range(n):
        x = (x * 1103515245 + 12345) & 0xFFFFFFFF
        tri = i % 200
        if tri >= 100:
            tri = 200 - tri
        out.append(tri * 160 - 8000 + (x >> 24) - 128)
    return out


def main() -> int:
    encoder = FlacEncoder(sample_rate=RATE, bit_depth=16, channels=1)
    header = encoder.get_header()
    block = encoder.frame_samples
    samples = pattern(2 * block) + [0] * block
    pcm = struct.pack(f"<{len(samples)}h", *samples)
    frames = [f for f, _ in encoder.process(pcm, 0, len(samples) * 1_000_000 // RATE)]
    if len(frames) != 3:
        print(f"the encoder made {len(frames)} frames of {block} samples, want 3", file=sys.stderr)
        return 1
    OUT.write_bytes(header + b"".join(frames))
    print(f"{OUT.name}: {len(header)}-byte header, frames of {[len(f) for f in frames]} bytes")
    return 0


if __name__ == "__main__":
    sys.exit(main())
