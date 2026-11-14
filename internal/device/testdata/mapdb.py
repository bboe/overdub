#!/usr/bin/env python3
"""Build the SQLite fixtures the map.db reader is tested against.

Run from anywhere; it writes beside itself. The stores mirror the shape of
/data/ace/kvstorage/map.db on Fire OS 6: 1 KB pages, a deviceData table whose
root is an interior page, and two values long enough to spill into overflow
pages, one of them by less than the minimum local share so that SQLite keeps
the minimum on the page. Every value is invented.
"""

import pathlib
import sqlite3

ACCESS_TOKEN = "Atna|" + ("fedcba9876543210" * 24)[:370]

ADP_TOKEN = "{enc:" + ("A" * 2600) + "}"
DEVICE_CERT = (
    "-----BEGIN CERTIFICATE-----\n" + ("Q" * 990) + "\n-----END CERTIFICATE-----\n"
)
HERE = pathlib.Path(__file__).resolve().parent
PRIVATE_KEY = (
    "-----BEGIN RSA PRIVATE KEY-----\n"
    + ("MIIEpAIBAAKCAQEA" * 160)
    + "\n-----END RSA PRIVATE KEY-----\n"
)
REFRESH_TOKEN = "Atnr|" + ("0123456789abcdef" * 22)[:348]

REGISTERED = {
    "access_token": ACCESS_TOKEN,
    "adp_token": ADP_TOKEN,
    "country_of_residence": "US",
    "device_cert": DEVICE_CERT,
    "device_private_key": PRIVATE_KEY,
    "preferred_marketplace": "ATVPDKIKX0DER",
    "refresh_token": REFRESH_TOKEN,
    "reg_app_name": "MYAPP",
    "reg_app_version": "0.1",
    "reg_country_code": "US",
    "reg_device_model": "EchoDevice",
    "reg_endpoint_type": "1",
    "reg_os_version": "1.1",
    "registration_state": "REGISTERED",
    "signature_algorithm": "SHA256WithRSA:1.0",
    "token_expire": "3528",
    "token_fetch_time": "36",
    "user_name": "Somebody",
}

UNREGISTERED = {
    "country_of_residence": "US",
    "preferred_marketplace": "ATVPDKIKX0DER",
    "registration_state": "UNREGISTERED",
}


def build(name: str, rows: dict[str, str], page_size: int, filler: int) -> None:
    path = HERE / name
    path.unlink(missing_ok=True)
    connection = sqlite3.connect(path)
    connection.execute(f"PRAGMA page_size = {page_size}")
    connection.execute(
        "CREATE TABLE deviceData (key TEXT PRIMARY KEY, value BLOB NOT NULL)"
    )
    for index in range(filler):
        connection.execute(
            "INSERT INTO deviceData VALUES (?, ?)",
            (f"filler_{index:03d}", f"filler value {index:03d} ".ljust(100, "-")),
        )
    for key, value in rows.items():
        connection.execute(
            "INSERT INTO deviceData VALUES (?, ?)", (key, value.encode())
        )
    connection.commit()
    connection.execute("VACUUM")
    connection.close()
    print(name, path.stat().st_size, "bytes")


def main() -> None:
    build("registered.db", REGISTERED, 1024, 60)
    build("unregistered.db", UNREGISTERED, 1024, 60)
    build("default-pages.db", REGISTERED, 4096, 0)
    (HERE / "empty.db").write_bytes(b"")


if __name__ == "__main__":
    main()
