# Opt-in autoscaling smoke fixtures

These fixtures exercise real Docker Engine and single-node Swarm lifecycle APIs
in disposable nested daemons. The backend echoes TCP bytes; a probe sends a Java
login handshake through mc-router, verifies the echoed login, holds the
connection, then disconnects. No Minecraft download or EULA acceptance is needed.
`go test`, Make targets, and existing CI do not start provider infrastructure.

## Requirements and isolation

Use a Linux Docker host that supports privileged containers, Docker Compose,
Bash, and host Python 3 for automated verification. The repository Dockerfile
builds the router from the current checkout. Registry access is required for
build images and fixture images. The engine and Python image defaults are pinned
by digest; `SMOKE_ENGINE_IMAGE` and `SMOKE_BACKEND_IMAGE` can override them for
compatibility testing. Actual Docker versions and the backend image reference
are recorded in each run's report.

Docker discovery scans the entire daemon, so a Compose network alone would not
isolate scaling. Only a disposable `docker:28-dind` engine runs on the host;
router, backends, probes, and optional Swarm live inside it. The router receives
only the inner socket. No ports are published. Swarm is initialized only inside
the disposable engine, leaving host Swarm and existing deployments untouched.

Kubernetes is not included. It needs its own tooling and disposable-cluster
fixture. The verifier explicitly records unavailable and unverified scenarios;
it does not label missing provider coverage as a pass.

## Automated verification

From this directory:

```bash
./smoke.sh check
./smoke.sh verify docker --artifacts /tmp/mc-router-smoke-docker
./smoke.sh verify swarm --artifacts /tmp/mc-router-smoke-swarm
```

`verify` chooses a unique project automatically unless `SMOKE_PROJECT` is set.
It refuses an existing project/image before taking ownership. Use different
projects and artifact directories for parallel provider runs. The default
verification budget is 600 seconds, including a maximum 240-second setup command;
change it with `--timeout SECONDS`. Each Docker command is bounded. Diagnostics
and cleanup have a separate 90-second budget.

| Check | Assertions |
| --- | --- |
| Global inheritance | `inherited.smoke.test` remains active during login and scales down after the global 60s delay. |
| Resource override | `override.smoke.test` uses 15s instead of the global delay. |
| Shared hostnames | Concurrent override/alias logins keep one target active; its delay starts after the final disconnect. |
| Metadata update | Changing the override to 8s changes the next idle cycle. |
| Metadata removal | Removing the setting restores the global 60s delay. |
| Pending update (Swarm) | Changing service labels while a 15s timer is pending preserves that deadline; the next cycle uses 8s. |

Docker container labels cannot be changed in place: metadata tests replace only
the disposable override container and verify its identity changes. Swarm tests
update service labels in place and verify the service ID stays the same.
Consequently, Docker's metadata check does not claim in-place pending-timer
coverage. Both aliases are preserved through changes.

The verifier checks probe exit codes, samples backend running/desired-replica
state while players are active, measures idle intervals, and checks matching
router timer/scale-down logs. Timing assertions allow 0.75s early measurement
slack and 12s reconciliation/polling lag. The configured timer duration must also
appear in router debug logs. A successful probe alone is not a lifecycle pass.

Every run writes `report.json`, setup/cleanup logs, router/engine diagnostics,
and per-cycle probe logs under the artifact directory. Reports identify completed
and failed checks, metadata changes, observed delays, sampled state, and coverage
limitations. They are local/CI artifacts, not tracked `RESULTS.md` files.
A failed setup, failed assertion, timeout, or cleanup error returns nonzero.

Once the verifier takes ownership, it collects diagnostics and attempts cleanup
on success, failure, or Ctrl-C. Hard termination or a daemon outage may prevent
cleanup; retain the printed project name and run `down` manually in that case.

## Manual fixture commands

```bash
export SMOKE_PROJECT="mc-router-smoke-$(date +%s)-${RANDOM}"
./smoke.sh up docker                 # or swarm
./smoke.sh probe docker override.smoke.test --hold 25
./smoke.sh state docker
./smoke.sh down
```

`up` creates an inherited backend (60s) and an overridden backend (15s) with
`override.smoke.test` and `alias.smoke.test` sharing its scaling target. Startup
can arm timers; a later login should wake a stopped backend. The synthetic
backend handles SIGTERM so provider scale-down completes promptly.

`probe` waits for the named client container to finish, prints its UTC connection
records, and returns its actual exit code. Its default overall limit is 180s
(`SMOKE_PROBE_TIMEOUT` overrides it); startup/wake retries default to 90s.
While a probe holds a connection, use `state` in another terminal with the same
project and provider. The backend must stay running/at one desired replica.
Manual `up` leaves a failed setup available for diagnosis; run `down` afterward.

Manual commands are intentionally lower-level than `verify`: `state` prints
observations and does not itself assert lifecycle behavior.

## Cleanup and diagnostics

```bash
./smoke.sh state docker              # or swarm
# If the inner daemon fails:
docker compose -f compose.yaml -p "$SMOKE_PROJECT" logs --tail 100 engine
./smoke.sh down
# Must show no remaining containers:
docker ps -aq --filter "label=com.docker.compose.project=$SMOKE_PROJECT"
```

`down` removes this project's engine, anonymous storage, and uniquely named
router image, thereby removing all inner containers/services/networks/images.
It does not prune unrelated host resources, build cache, or downloaded base
images. No host `docker swarm leave`, global prune, or cluster deletion is needed.
The verifier checks for remaining project-labelled containers, networks, and
volumes after cleanup.

## Coverage limits and future GitHub Actions integration

Not covered: Kubernetes, real Minecraft servers, Swarm VIP/multi-node networking,
Swarm global services, default routes, live invalid-value fallback, and conflicting
per-route settings on the same physical target. Unit tests cover several of
these policy cases, but that is distinct from real-provider verification.

No workflow is added here. A later optional workflow can call `verify` on a
standard disposable Ubuntu VM with a Docker/Swarm matrix, unique project names,
read-only repository permissions, uploaded artifacts, and unconditional cleanup.
Allow a job timeout longer than verification plus cleanup. `workflow_dispatch`
requires the workflow to exist on the default branch. Avoid persistent shared
runners for this privileged fixture. Keep infrastructure tests separate from the
ordinary Go test job and retain this folder as independently removable test code.
