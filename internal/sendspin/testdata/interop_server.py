"""Drive a real aiosendspin server against overdub's Sendspin client.

Run through internal/sendspin/interop_test.go, which passes the Dot's
WebSocket URL and client_id. Exits non-zero with a reason on any mismatch.
With --play-seconds, the client must have a player: the server streams that
much of flac_vector.pattern in whatever format the client prefers, and the Go
side checks what arrived. With --pairing-psk, the server then pairs with that
Pairing PSK, reconnects as a fresh server holding only the record, and unpairs.
With --unapproved-seconds, the server never pairs, as Music Assistant does after
an unpair, and the client must keep that one connection.
"""

from __future__ import annotations

import argparse
import asyncio
import logging
import struct
import sys
from typing import TYPE_CHECKING

from aiosendspin.audio.format import AudioFormat
from aiosendspin.models.core import ClientTimeMessage
from aiosendspin.models.types import PairMethod
from aiosendspin.noise.keys import Identity
from aiosendspin.noise.pairing import PairingAttempt
from aiosendspin.noise.trust_store import InMemoryServerPairingStore
from aiosendspin.server.connection import SendspinConnection
from aiosendspin.server.server import SendspinServer
from flac_vector import RATE, pattern

if TYPE_CHECKING:
    from aiosendspin.server.client import SendspinClient

BUFFER_US = 2_000_000
CHUNK_FRAMES = RATE // 50
FORMAT = AudioFormat(bit_depth=16, channels=2, sample_rate=RATE)
POLL_S = 0.1
SETTLE_TIMEOUT_S = 15.0
WANTED_EXCHANGES = 3


class _Complaints(logging.Handler):
    """Collect anything the reference server warns about."""

    def __init__(self) -> None:
        super().__init__(level=logging.WARNING)
        self.lines: list[str] = []

    def emit(self, record: logging.LogRecord) -> None:
        self.lines.append(record.getMessage())


def count_time_exchanges() -> list[int]:
    seen = [0]
    handle = SendspinConnection._handle_message  # ruff: ignore[private-member-access]

    async def counting(self, message, timestamp_us):  # ruff: ignore[missing-type-function-argument, missing-return-type-private-function]
        if isinstance(message, ClientTimeMessage):
            seen[0] += 1
        return await handle(self, message, timestamp_us)

    SendspinConnection._handle_message = counting  # ruff: ignore[private-member-access]
    return seen


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--url", required=True)
    ap.add_argument("--client-id", required=True)
    ap.add_argument("--pairing-psk", default="")
    ap.add_argument("--play-seconds", default=0, type=float)
    ap.add_argument("--unapproved-seconds", default=0, type=float)
    args = ap.parse_args()
    if args.unapproved_seconds:
        return asyncio.run(
            unapproved(
                client_id=args.client_id, seconds=args.unapproved_seconds, url=args.url
            )
        )
    return asyncio.run(
        run(
            client_id=args.client_id,
            pairing_psk=bytes.fromhex(args.pairing_psk),
            play_seconds=args.play_seconds,
            url=args.url,
        )
    )


async def pair(
    *,
    client_id: str,
    identity: Identity,
    pairing_psk: bytes,
    server: SendspinServer,
    url: str,
) -> list[str]:
    loop = asyncio.get_running_loop()
    failures: list[str] = []
    await server.initiate_pairing(
        client_id,
        PairingAttempt(
            PairMethod.PAIRING_PSK, client_id=client_id, pairing_psk=pairing_psk
        ),
    )
    client = server.get_or_create_client(client_id)
    print(f"paired              = {client.is_paired}")
    if not client.is_paired:
        return ["the server did not pair over the pairing psk"]
    store = server.pairing_store
    await server.close()

    again = SendspinServer(loop, identity, "interop test server", pairing_store=store)
    try:
        again.register_client_url(client_id, url)
        await again.connect_to_client_and_wait(url)
        client = again.get_or_create_client(client_id)
        deadline = loop.time() + SETTLE_TIMEOUT_S
        while loop.time() < deadline:
            if client.is_paired and "player@v1" in set(client.active_role_ids):
                break
            await asyncio.sleep(POLL_S)
        print(f"reconnected paired  = {client.is_paired}")
        print(f"reconnected roles   = {sorted(client.active_role_ids)}")
        if not client.is_paired:
            failures.append("a fresh server holding the record did not connect paired")
        if "player@v1" not in set(client.active_role_ids):
            failures.append("the paired reconnect never activated the player")
        await again.unpair(client_id)
        deadline = loop.time() + SETTLE_TIMEOUT_S
        while loop.time() < deadline:
            if not client.is_connected:
                break
            await asyncio.sleep(POLL_S)
        if client.is_connected:
            failures.append("the client stayed connected after server/unpair")
    finally:
        await again.close()
    return failures


async def play(*, client: SendspinClient, seconds: float) -> None:
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


async def run(  # ruff: ignore[complex-structure, too-many-branches, too-many-statements]
    *, client_id: str, pairing_psk: bytes, play_seconds: float, url: str
) -> int:
    loop = asyncio.get_running_loop()
    exchanges = count_time_exchanges()
    complaints = _Complaints()
    logging.getLogger("aiosendspin").addHandler(complaints)
    logging.getLogger("aiosendspin").setLevel(logging.WARNING)
    identity = Identity.generate()
    store = InMemoryServerPairingStore()
    server = SendspinServer(loop, identity, "interop test server", pairing_store=store)
    # Operator approval: what lets an unpaired Sentinel session reach playback.
    await server.trust_unpaired(client_id)
    server.register_client_url(client_id, url)

    failures: list[str] = []
    try:
        await server.connect_to_client_and_wait(url)

        client = server.get_or_create_client(client_id)
        deadline = loop.time() + SETTLE_TIMEOUT_S
        while loop.time() < deadline:
            synced = bool(pairing_psk) or exchanges[0] >= WANTED_EXCHANGES
            if (
                client.is_connected
                and client.info_or_none is not None
                and synced
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
            roles = sorted(client.negotiated_role_ids)
            failures.append(f"player@v1 missing from negotiated roles {roles}")
        if not pairing_psk and exchanges[0] < WANTED_EXCHANGES:
            failures.append(
                f"the client asked the time {exchanges[0]} times in"
                f" {SETTLE_TIMEOUT_S}s, want {WANTED_EXCHANGES}"
            )
        if play_seconds:
            if not client.available:
                failures.append("a client with a player never reported available")
            else:
                await play(client=client, seconds=play_seconds)
        elif client.available and not pairing_psk:
            failures.append(
                "the client reported available while it has no clock or audio"
            )
        if pairing_psk and not failures:
            failures += await pair(
                client_id=client_id,
                identity=identity,
                pairing_psk=pairing_psk,
                server=server,
                url=url,
            )
        failures.extend(
            f"server complained: {line}"
            for line in complaints.lines
            if "non-compliant" in line or "Malformed" in line
        )
    finally:
        await server.close()

    for f in failures:
        print(f"FAIL: {f}", file=sys.stderr)
    if failures:
        return 1
    print("\nINTEROP OK")
    return 0


async def unapproved(*, client_id: str, seconds: float, url: str) -> int:
    loop = asyncio.get_running_loop()
    server = SendspinServer(
        loop,
        Identity.generate(),
        "interop test server",
        pairing_store=InMemoryServerPairingStore(),
    )
    failures: list[str] = []
    try:
        server.register_client_url(client_id, url)
        await server.connect_to_client_and_wait(url)
        client = server.get_or_create_client(client_id)
        deadline = loop.time() + SETTLE_TIMEOUT_S
        while loop.time() < deadline:
            if client.is_connected:
                break
            await asyncio.sleep(POLL_S)
        end = loop.time() + seconds
        while loop.time() < end:
            if not client.is_connected:
                failures.append("the client dropped a server that had not approved it")
                break
            if client.active_role_ids:
                failures.append(
                    f"the server activated {sorted(client.active_role_ids)}"
                )
                break
            await asyncio.sleep(POLL_S)
        print(f"held unapproved     = {seconds}s")
    finally:
        await server.close()
    for f in failures:
        print(f"FAIL: {f}", file=sys.stderr)
    if failures:
        return 1
    print("\nINTEROP OK")
    return 0


if __name__ == "__main__":
    sys.exit(main())
