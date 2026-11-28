"""Synthetic TCP backend for real-provider lifecycle smoke checks, not Minecraft."""

import signal
import socketserver
import sys


class Echo(socketserver.BaseRequestHandler):
    def handle(self):
        print("CONNECTED", flush=True)
        while data := self.request.recv(65536):
            self.request.sendall(data)
        print("DISCONNECTED", flush=True)


class Server(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True


if __name__ == "__main__":
    # DockerStop must work promptly even when Python is PID 1.
    signal.signal(signal.SIGTERM, lambda *_: sys.exit(0))
    with Server(("0.0.0.0", 25565), Echo) as server:
        print("LISTENING 25565", flush=True)
        server.serve_forever()
