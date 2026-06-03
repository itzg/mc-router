#!/usr/bin/env python3
"""
Minimal webhook receiver for mc-router's webhook autoscaler.

Starts/stops an itzg/minecraft-server container named "mc.test" under Apple's
`container` CLI (https://github.com/apple/container) on mc-router's scale up/down
webhooks. Apple `container` hands out a fresh IP on each start, so the scale-up
reply reports the current IP ({"backend": "<ip>:25565"}) for mc-router to use.

See docs/webhook-scaler/README.md for the full setup and response contract.
"""
import json
import subprocess
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

LISTEN_ADDR = ("0.0.0.0", 8080)
CONTAINER = "mc.test"
SERVER_PORT = 25565


def run_container(*args):
    result = subprocess.run(
        ["container", *args],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.PIPE,
        text=True,
    )
    if result.stderr.strip():
        print(result.stderr.strip(), file=sys.stderr, flush=True)
    return result.returncode == 0


def container_ipv4(name):
    """Return the running container's IPv4 (without the /CIDR suffix), or None."""
    result = subprocess.run(
        ["container", "ls", "--format", "json"], capture_output=True, text=True
    )
    if result.returncode != 0:
        return None
    try:
        containers = json.loads(result.stdout or "[]")
    except json.JSONDecodeError:
        return None
    for c in containers:
        if c.get("configuration", {}).get("id") != name:
            continue
        for net in c.get("networks") or []:
            addr = net.get("ipv4Address", "")
            if addr:
                return addr.split("/")[0]
    return None


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        try:
            payload = json.loads(self.rfile.read(length) or b"{}")
        except json.JSONDecodeError:
            self.respond(400)
            return

        action = payload.get("action")
        print(f"webhook: action={action} server={payload.get('serverAddress')!r}", flush=True)

        if action == "up":
            ok = run_container("start", CONTAINER)
            body = None
            if ok:
                ip = container_ipv4(CONTAINER)
                if ip:
                    body = {"backend": f"{ip}:{SERVER_PORT}"}
            self.respond(200 if ok else 500, body)
        elif action == "down":
            ok = run_container("stop", CONTAINER)
            self.respond(200 if ok else 500)
        else:
            self.respond(400)

    def respond(self, status, body=None):
        self.send_response(status)
        if body is not None:
            payload = json.dumps(body).encode()
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)
        else:
            self.end_headers()

    def log_message(self, *args):
        pass  # quiet the default access log


def main():
    server = HTTPServer(LISTEN_ADDR, Handler)
    print(f"mc-router webhook scaler listening on {LISTEN_ADDR[0]}:{LISTEN_ADDR[1]} (container '{CONTAINER}')", flush=True)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
