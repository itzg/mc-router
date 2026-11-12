# Opt-in autoscaling smoke fixtures

These fixtures prepare a real Docker Engine or single-node Swarm environment for
checking per-resource idle delays. They are manual development tools: `go test`,
Make targets, and CI do not start them. No Minecraft download or EULA acceptance
is involved. The backend echoes TCP bytes; the probe sends a Java login handshake
through the router, verifies the echoed login, holds the connection, then leaves.
This checks discovery, proxy connections, and provider lifecycle operations, not
a Minecraft server implementation.

## Tooling and current validation

The development environment has Docker CLI 29.9.3, Compose v5.7.0, Python 3, and Go.
The reachable daemon is Engine 28.3.0 with Swarm inactive. Docker daemon access
required elevated sandbox permissions. `kubectl`, `kind`, `minikube`, and `k3d`
were not installed. Kubernetes fixtures and validation are deferred until suitable
tooling and a disposable cluster are available.

Shell/Python syntax, Compose configuration, and read-only preflight passed during
preparation. The Python helpers also exchanged login bytes over a local loopback
echo server and reported an unreachable backend as a failure; this did not run
the router or any provider. These checks can be repeated without deployment.
The fixtures have now been deployed locally. Docker checks passed; the Swarm
cold-wake/shared-alias check failed because the backend scaled down with active
connections. See [local results](RESULTS.md) for timings, limitations, and the
harness issue corrected during testing. No production code was changed.

Lifecycle timing assertions are still manual. The probe checks its container's
final exit status and prints its logs after completion, with a default overall
limit of 180 seconds (`SMOKE_PROBE_TIMEOUT` can override that limit).

## Isolation

mc-router's Docker discovery scans the whole daemon. A Compose network alone
would not prevent it from scaling unrelated labelled containers. Therefore, the
host daemon runs only a disposable `docker:28-dind` engine for this project. The
router, backends, client probes, and optional Swarm all run inside that engine.
The router never receives the host Docker socket.

The nested engine requires a privileged container on a development Docker host.
It listens on its internal Unix socket; no host ports are published. Swarm mode
is initialized only inside this disposable engine. No host Swarm configuration,
Kubernetes context, or existing deployment is modified. Each project has its own
engine and storage. Do not reuse a project name belonging to another run.

## Preflight and setup

Run from this directory. Python is supplied by the probe image; host Python is
only needed for optional syntax checks.

```bash
./smoke.sh check
export SMOKE_PROJECT="mc-router-smoke-$(date +%s)-${RANDOM}"
# Choose one provider for this project:
./smoke.sh up docker
# Or use another unique project name and explicitly choose:
# ./smoke.sh up swarm
```

`up` builds the router from the current checkout using the repository Dockerfile,
loads that image into the inner engine, and pulls `python:3.13-alpine` there.
It then starts two backends and the router:

| Backend | Hostnames | Delay |
| --- | --- | --- |
| `inherited` | `inherited.smoke.test` | global `60s` |
| `override` | `override.smoke.test`, `alias.smoke.test` | override `15s` |

Docker uses container labels. Swarm uses service labels and replicated services
on an attachable overlay network. The setup does not test Swarm global services.
The synthetic backend handles SIGTERM so scale-down can stop it promptly.

Startup discovery can already arm idle timers. Run probes promptly after setup;
if a backend has already stopped, a login should wake it again. Image pulls and
building can take time, but happen before the smoke router starts. The probes
retry startup/wake connections for up to 90 seconds by default.

If setup fails, collect diagnostics and run `down`; it deliberately leaves the
project available for inspection rather than hiding failures through automatic
cleanup. Preserve the exported project name until cleanup finishes.

## Observable pass/fail criteria

Use the same provider argument chosen at setup. Commands below use `docker`;
substitute `swarm` consistently for a Swarm project.

```bash
./smoke.sh probe docker override.smoke.test --hold 25
./smoke.sh state docker
```

The probe must remain connected longer than the 15-second idle delay and exit
successfully, reporting timestamped `CONNECTED` and `DISCONNECTED` records. Logs
are collected when the client container finishes, rather than streamed.
In a second terminal with the same exported project name, inspect state during
that hold: the backend must remain running (Docker) or at one replica (Swarm).
A probe failure or backend shutdown while connected is a **FAIL**.

After disconnect, poll `state` every few seconds. The override must stay running
for approximately 15 seconds, then become stopped (Docker) or desired replicas
zero (Swarm). Router logs must identify its scale-down action. Allow several
seconds for event delivery and reconciliation; a shutdown before the effective
idle delay or no scale-down within 30 seconds is a **FAIL** under an otherwise
healthy, responsive local daemon. Docker's `finished` timestamp and the probe's
UTC timestamps help distinguish the effective delay from polling lag.

Compare inheritance in a separate cycle:

```bash
./smoke.sh probe docker inherited.smoke.test --hold 5
./smoke.sh state docker
```

After its disconnect, `inherited` must still be running after 20 seconds and stop
around the global 60-second delay. Allow up to 75 seconds for the observed stop.
The probe must succeed, and logs must identify the corresponding backend.
Swarm's `state` reports desired/running replica counts: desired zero is the
scale-down decision; terminating tasks can take additional time.

To check shared-hostname activity, run these concurrently in separate terminals
with the same project and provider, then inspect state:

```bash
./smoke.sh probe docker override.smoke.test --hold 20
./smoke.sh probe docker alias.smoke.test --hold 35
```

The shared backend must remain up while either probe is connected. Its 15-second
idle period starts only after the **last** disconnect. The aliases must not result
in independent stop decisions. This first fixture setup does not yet exercise
metadata updates, invalid-value inheritance, default routes, or conflicting
per-route timeouts; those belong to later smoke coverage.

Save commands, provider/daemon versions, probe output, state observations, and
router logs when recording a result. Infrastructure failures are inconclusive,
not a feature pass. A successful syntax/preflight check is not a provider pass.

## Diagnostics and cleanup

```bash
./smoke.sh state docker  # or swarm
# Outer-engine diagnostics if the inner daemon failed:
docker compose -f compose.yaml -p "$SMOKE_PROJECT" logs --tail 100 engine
./smoke.sh down
# Must return no container IDs:
docker ps -aq --filter "label=com.docker.compose.project=$SMOKE_PROJECT"
```

`down` removes the project's engine, its anonymous storage volumes, and the
uniquely named local router image. Removing the engine removes all of its inner
containers, Swarm services, networks, and images. It does not prune unrelated
host images, build caches, networks, or volumes; pulled host base images and build
cache may remain. It is safe to repeat `down` for the same project.

Run cleanup after successful checks, failures, or interruption. No `docker swarm
leave`, cluster deletion, global prune, or removal of existing deployments is
needed. No API is exposed on a host port, and no authentication tokens or real
server data are required.

## Future CI integration

Keep this folder in a separate test-only commit so reverting it does not affect
production code, Go tests, or existing CI. There is no workflow added by this
setup. A future automation commit can add:

1. A bounded verification command that asserts running/replica state and elapsed
   idle time, rather than treating a successful login probe as a complete pass.
2. An opt-in GitHub Actions workflow on a standard Ubuntu VM runner, initially
   using manual dispatch and a Docker/Swarm provider matrix. Manual dispatch
   requires the workflow to exist on the default branch.
3. A unique project name for each run/job, a job timeout, log artifacts, and an
   unconditional cleanup step (`if: always()`).
4. Explicit nonzero results for lifecycle failures. The observed Swarm failure
   must be investigated before presenting that matrix as passing or required.

Use the nested daemon isolation even on CI so the router cannot discover other
job containers. The runner needs privileged-container support. Kubernetes would
need an additional disposable-cluster fixture and is not covered here.
