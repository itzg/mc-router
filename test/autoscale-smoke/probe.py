"""Send a Java login handshake through the router to the synthetic echo backend."""

import argparse
from datetime import datetime, timezone
import select
import socket
import struct
import time


def varint(value):
    result = bytearray()
    while value > 127:
        result.append((value & 127) | 128)
        value >>= 7
    result.append(value)
    return bytes(result)


def string(value):
    data = value.encode("utf-8")
    return varint(len(data)) + data


def frame(payload):
    return varint(len(payload)) + payload


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("hostname")
    parser.add_argument("--router", default="router")
    parser.add_argument("--port", type=int, default=25565)
    parser.add_argument("--hold", type=float, default=20)
    parser.add_argument("--wait", type=float, default=90)
    args = parser.parse_args()
    if args.hold < 0 or args.wait <= 0:
        parser.error("--hold must be nonnegative and --wait must be positive")

    # Protocol 758 (1.18.2) uses a username-only Login Start packet.
    payload = frame(b"\x00" + varint(758) + string(args.hostname) + struct.pack(">H", 25565) + varint(2))
    payload += frame(b"\x00" + string("SmokePlayer"))
    deadline = time.monotonic() + args.wait
    while True:
        connected = False
        try:
            with socket.create_connection((args.router, args.port), timeout=5) as conn:
                conn.sendall(payload)
                echoed = bytearray()
                while len(echoed) < len(payload):
                    chunk = conn.recv(len(payload) - len(echoed))
                    if not chunk:
                        raise ConnectionError("router closed before the login reached the echo backend")
                    echoed.extend(chunk)
                if echoed != payload:
                    raise ConnectionError("backend did not echo the expected login bytes")
                connected = True
                print(f"{datetime.now(timezone.utc).isoformat()} CONNECTED {args.hostname}; holding {args.hold:g}s", flush=True)
                time.sleep(args.hold)
                if select.select([conn], [], [], 0)[0] and not conn.recv(1):
                    raise ConnectionError("connection closed while player should remain active")
            print(f"{datetime.now(timezone.utc).isoformat()} DISCONNECTED {args.hostname}; idle timer should now start", flush=True)
            return
        except (OSError, ConnectionError) as error:
            # Once the active hold begins, a disconnect is a failure, not a wake retry.
            if connected:
                raise SystemExit(f"FAIL: {error}") from error
            if time.monotonic() >= deadline:
                raise SystemExit(f"FAIL: login did not reach backend: {error}") from error
            time.sleep(1)


if __name__ == "__main__":
    main()
