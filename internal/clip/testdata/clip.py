#!/usr/bin/env python3
"""Write clip.flac the way Home Assistant's ffmpeg proxy streams one.

The proxy asks ffmpeg for FLAC on a pipe, so the STREAMINFO block keeps no
totals. Prints the SHA-256 of the PCM ffmpeg decodes from it, which
clip_test.go holds.
"""

import hashlib
import pathlib
import subprocess

HERE = pathlib.Path(__file__).parent
PROXY = [
    "-f",
    "flac",
    "-ac",
    "2",
    "-ar",
    "48000",
    "-sample_fmt",
    "s16",
    "-map_metadata",
    "-1",
]
SOURCE = [
    "-f",
    "lavfi",
    "-i",
    "sine=frequency=440:duration=0.5:sample_rate=44100",
    "-f",
    "lavfi",
    "-i",
    "sine=frequency=660:duration=0.5:sample_rate=44100",
    "-filter_complex",
    "amerge=inputs=2",
]


def main() -> None:
    flac = subprocess.run(
        ["ffmpeg", "-v", "error", *SOURCE, *PROXY, "pipe:"],
        capture_output=True,
        check=True,
    ).stdout
    (HERE / "clip.flac").write_bytes(flac)
    pcm = subprocess.run(
        ["ffmpeg", "-v", "error", "-i", "pipe:", "-f", "s16le", "pipe:"],
        capture_output=True,
        check=True,
        input=flac,
    ).stdout
    print(len(pcm) // 4, "frames, sha256", hashlib.sha256(pcm).hexdigest())


if __name__ == "__main__":
    main()
