"""Drive a real aiosendspin server against overdub's Sendspin client.

Run through internal/sendspin/interop_test.go, which passes the Dot's
WebSocket URL and client_id. Exits non-zero with a reason on any mismatch.
With --play-seconds, the client must have a player: the server streams that
much of flac_vector.pattern in whatever format the client prefers, and the Go
side checks what arrived.
"""

from __future__ import annotations

import argparse
import asyncio
import logging
import struct
import sys

from aiosendspin.audio.format import AudioFormat
from aiosendspin.models.core import ClientTimeMessage
from aiosendspin.noise.keys import Identity
from aiosendspin.noise.trust_store import InMemoryServerPairingStore
from aiosendspin.server.connection import SendspinConnection
from aiosendspin.server.server import SendspinServer
from flac_vector import RATE, pattern

SETTLE_TIMEOUT_S = 15.0
POLL_S = 0.1
WANTED_EXCHANGES = 3
CHUNK_FRAMES = RATE // 50
BUFFER_US = 2_000_000
FORMAT = AudioFormat(sample_rate=RATE, bit_depth=16, channels=2)


def count_time_exchanges() -> list[int]:
    """Count the client/time messages the reference server parses and answers."""
    seen = [0]
    handle = SendspinConnection._handle_message

    async def counting(self, message, timestamp_us):  # noqa: ANN001, ANN202
        if isinstance(message, ClientTimeMessage):
            seen[0] += 1
        return await handle(self, message, timestamp_us)

    SendspinConnection._handle_message = counting
    return seen


class _Complaints(logging.Handler):
    """Collect anything the reference server warns about."""

    def __init__(self) -> None:
        super().__init__(level=logging.WARNING)
        self.lines: list[str] = []

    def emit(self, record: logging.LogRecord) -> None:
        self.lines.append(record.getMessage())


async def play(client, seconds: float) -> None:
    samples = pattern(int(RATE * seconds))
    stream = client.group.start_stream()
    for at in range(0, len(samples), 2 * CHUNK_FRAMES):
        part = samples[at : at + 2 * CHUNK_FRAMES]
        stream.prepare_audio(struct.pack(f"<{len(part)}h", *part), FORMAT)
        await stream.commit_audio()
        await stream.sleep_to_limit_buffer(BUFFER_US)
    print(f"streamed            = {len(samples) // 2} frames")
    await asyncio.sleep(BUFFER_US / 1_000_000 + 1)
    await client.group.stop()


async def run(url: str, client_id: str, play_seconds: float) -> int:
    loop = asyncio.get_running_loop()
    exchanges = count_time_exchanges()
    complaints = _Complaints()
    logging.getLogger("aiosendspin").addHandler(complaints)
    logging.getLogger("aiosendspin").setLevel(logging.WARNING)
    server = SendspinServer(
        loop,
        Identity.generate(),
        "interop test server",
        pairing_store=InMemoryServerPairingStore(),
    )
    # Operator approval: what lets an unpaired Sentinel session reach playback.
    await server.trust_unpaired(client_id)
    server.register_client_url(client_id, url)

    failures: list[str] = []
    try:
        await server.connect_to_client_and_wait(url)

        client = server.get_or_create_client(client_id)
        deadline = loop.time() + SETTLE_TIMEOUT_S
        while loop.time() < deadline:
            if (
                client.is_connected
                and client.info_or_none is not None
                and exchanges[0] >= WANTED_EXCHANGES
                and (not play_seconds or client.available)
            ):
                break
            await asyncio.sleep(POLL_S)

        print(f"connected           = {client.is_connected}")
        print(f"name                = {client.name!r}")
        print(f"client_id           = {client.client_id}")
        print(f"security            = {client.connection_security}")
        print(f"paired              = {client.is_paired}")
        print(f"negotiated roles    = {sorted(client.negotiated_role_ids)}")
        print(f"active roles        = {sorted(client.active_role_ids)}")
        print(f"available           = {client.available}")
        print(f"time exchanges      = {exchanges[0]}")
        info = client.info_or_none
        print(f"device info         = {info}")

        if not client.is_connected:
            failures.append("the server never completed a connection")
        if client.client_id != client_id:
            failures.append(f"client_id came back as {client.client_id}")
        if "player@v1" not in set(client.negotiated_role_ids):
            failures.append(
                f"player@v1 missing from negotiated roles {sorted(client.negotiated_role_ids)}"
            )
        if exchanges[0] < WANTED_EXCHANGES:
            failures.append(
                f"the client asked the time {exchanges[0]} times in {SETTLE_TIMEOUT_S}s,"
                f" want {WANTED_EXCHANGES}"
            )
        if play_seconds:
            if not client.available:
                failures.append("a client with a player never reported available")
            else:
                await play(client, play_seconds)
        elif client.available:
            failures.append("the client reported available while it has no clock or audio")
        for line in complaints.lines:
            if "non-compliant" in line or "Malformed" in line:
                failures.append(f"server complained: {line}")
    finally:
        await server.close()

    for f in failures:
        print(f"FAIL: {f}", file=sys.stderr)
    if failures:
        return 1
    print("\nINTEROP OK")
    return 0


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--url", required=True)
    ap.add_argument("--client-id", required=True)
    ap.add_argument("--play-seconds", type=float, default=0)
    args = ap.parse_args()
    return asyncio.run(run(args.url, args.client_id, args.play_seconds))


if __name__ == "__main__":
    sys.exit(main())
