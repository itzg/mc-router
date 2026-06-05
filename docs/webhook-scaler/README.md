# Webhook auto-scale example (Apple `container`)

A runnable local example of mc-router's [webhook auto scaling](../../README.md#webhook-auto-scale).

A tiny Python webhook receiver ([`scaler.py`](scaler.py)) starts and stops an
`itzg/minecraft-server` container running under [Apple's `container` CLI](https://github.com/apple/container).
mc-router proxies Minecraft connections and calls the receiver to scale the
backend up on the first login and back down after it goes idle.

```
Minecraft client ──▶ mc-router ──▶ minecraft-server (Apple `container`)
                        │   ▲
       POST {action,...}│   │ {"backend":"192.168.64.x:25565"}  (live IP on scale-up)
                        ▼   │
                    scaler.py ──▶ `container start/stop mc.test`
```

Apple `container` hands out a **new IP every time the container starts**, so there is no stable address to hardcode. The
scaler solves this by reporting the container's current IP in its scale-up reply; mc-router uses that address for the wake
instead of re-resolving a name (which would otherwise serve a stale, cached IP and time out). See
[Webhook Auto Scale](../../README.md#webhook-auto-scale) for the optional-response contract.

The point of doing this over a webhook (instead of mc-router managing the
container itself) is the security boundary: mc-router needs no access to the
`container` CLI, no socket, and no shell — only `scaler.py` does. mc-router can
run as a minimal, least-privilege process while the scaling authority lives
here.

## Prerequisites

- macOS with Apple's [`container`](https://github.com/apple/container) installed and started (`container system start`).
- Go (to run mc-router from this repo) and Python 3 (for the receiver).

## 1. Create the Minecraft container

Create it once, **without** `--rm`, so it can be stopped and restarted rather
than destroyed. Name it `mc.test` to match `scaler.py`'s `CONTAINER`:

```shell
container create --name mc.test -e EULA=TRUE itzg/minecraft-server
```

Leave it stopped — mc-router starts it on the first connection. You do **not**
need to look up or hardcode its IP: Apple `container` assigns a fresh IP on
every start, and `scaler.py` reports the current one back to mc-router in its
scale-up reply (see [step 2](#2-start-the-webhook-receiver)). This also means
you don't need the local `.test` DNS domain (`container system property set
dns.domain test`) — mc-router connects to the reported IP directly.

## 2. Start the webhook receiver

```shell
python3 docs/webhook-scaler/scaler.py
```

It listens on `:8080`, runs `container start mc.test` / `container stop mc.test`,
and — on scale-up — replies with the container's current IP as
`{"backend":"192.168.64.x:25565"}` so mc-router always connects to the live
address.

## 3. Run mc-router pointed at the receiver

In another terminal, from the repo root:

```shell
go run ./cmd/mc-router/ \
  -port 25565 \
  -mapping mc.localhost=mc.test:25565 \
  -auto-scale-down-after 2m \
  -auto-scale-webhook-url http://localhost:8080/scale \
  -auto-scale-asleep-motd  "Asleep — connect to wake me up" \
  -auto-scale-loading-motd "Starting up, hang on…"
```

Notes:
- Providing the webhook URL is the opt-in — you do **not** also need
  `-auto-scale-up` / `-auto-scale-down` (those gate the Docker/Kubernetes paths).
- The `mc.test:25565` backend is only a fallback label; the address mc-router
  actually dials is the live IP returned by `scaler.py` on each scale-up.
- `mc.localhost` resolves to loopback, so the Minecraft client reaches mc-router
  on the host; the handshake's server address (`mc.localhost`) is what the
  receiver sees in the payload.

## 4. Try it

1. In the Minecraft client, add a server with address `mc.localhost`.
2. Refresh the server list — while stopped you'll see the asleep MOTD, and pings
   alone won't start it.
3. Click **Join**. mc-router POSTs `up`, `scaler.py` runs `container start
   mc.test` and replies with its IP, mc-router waits until that backend is
   reachable (up to `-auto-scale-webhook-wake-timeout`, default 60s), then
   connects you.
4. Disconnect and wait `2m`. With no connections left, mc-router POSTs `down`
   and `scaler.py` runs `container stop mc.test`.

Watch the `scaler.py` terminal to see each `container start`/`stop` as it
happens, and `container ls` to confirm the container's state.

## Cleanup

```shell
container stop mc.test 2>/dev/null
container rm mc.test
```
