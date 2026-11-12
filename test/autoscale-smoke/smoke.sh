#!/usr/bin/env bash
# Opt-in manual fixture management. Never called by go test or CI.
set -euo pipefail
fixture_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_dir=$(cd -- "$fixture_dir/../.." && pwd)
action=${1:-help}
provider=${2:-docker}

usage() {
  cat <<'HELP'
Usage: SMOKE_PROJECT=mc-router-smoke-<unique> ./smoke.sh <action> [docker|swarm] [probe arguments]
  check                 Read Docker/Compose availability; creates nothing
  up docker|swarm       Build this checkout and create an isolated nested engine
  state docker|swarm    Show backend running/replica state and router logs
  probe docker|swarm HOST [--hold SECONDS] [--wait SECONDS]
                        Send a real login through the router; hold, then disconnect
  down                  Remove the project's engine, storage, and local router image
Docker and Swarm backends: inherited.smoke.test, override.smoke.test,
                         alias.smoke.test (shares override's scaling target).
HELP
}

compose() { docker compose -f "$fixture_dir/compose.yaml" -p "$SMOKE_PROJECT" "$@"; }
inner() { compose exec -T engine docker --host=unix:///var/run/docker.sock "$@"; }

if [[ "$action" == help ]]; then usage; exit 0; fi
if [[ "$action" == check ]]; then
  command -v docker
  docker compose version
  docker info --format 'Host daemon: {{.ServerVersion}}; Swarm={{.Swarm.LocalNodeState}} (unchanged by these fixtures)'
  echo 'Nested-engine and image availability require an explicit up; not verified by check.'
  exit 0
fi
if [[ ! ${SMOKE_PROJECT:-} =~ ^mc-router-smoke-[a-z0-9-]+$ ]]; then
  echo 'Set SMOKE_PROJECT to a unique mc-router-smoke-<lowercase suffix> name.' >&2
  exit 2
fi
if [[ "$provider" != docker && "$provider" != swarm ]]; then usage; exit 2; fi
router_image="${SMOKE_PROJECT}-router:local"

case "$action" in
  up)
    # Refuse to repurpose an existing project. A failed setup is removed by down.
    if [[ -n $(compose ps -a -q) ]]; then
      echo 'This project already exists; inspect it or run down before another up.' >&2
      exit 1
    fi
    echo "Project: $SMOKE_PROJECT; provider: $provider"
    echo 'On failure, collect state and run: ./smoke.sh down'
    docker build -t "$router_image" -f "$repo_dir/Dockerfile" "$repo_dir"
    compose up -d --wait --wait-timeout 180
    compose exec -T engine mkdir -p /fixtures
    engine_id=$(compose ps -q engine)
    docker cp "$fixture_dir/backend.py" "$engine_id:/fixtures/backend.py"
    docker cp "$fixture_dir/probe.py" "$engine_id:/fixtures/probe.py"
    docker image save "$router_image" | inner image load
    inner image pull python:3.13-alpine
    if [[ "$provider" == docker ]]; then
      inner network create backends
      for backend in inherited override; do
        labels=(--label "mc-router.host=${backend}.smoke.test")
        if [[ "$backend" == override ]]; then
          labels=(--label 'mc-router.host=override.smoke.test,alias.smoke.test' --label 'mc-router.auto-scale-down-after=15s')
        fi
        inner run -d --name "$backend" --network backends "${labels[@]}" \
          --mount type=bind,source=/fixtures/backend.py,target=/backend.py,readonly \
          python:3.13-alpine python -u /backend.py
      done
      inner run -d --name router --network backends --user 0 \
        --mount type=bind,source=/var/run/docker.sock,target=/var/run/docker.sock \
        -e IN_DOCKER=true -e AUTO_SCALE_UP=true -e AUTO_SCALE_DOWN=true \
        -e AUTO_SCALE_DOWN_AFTER=60s "$router_image"
    else
      # This changes only the freshly-created inner daemon, never the host swarm.
      inner swarm init --advertise-addr eth0
      inner network create --driver overlay --attachable backends
      for backend in inherited override; do
        labels=(--label "mc-router.host=${backend}.smoke.test")
        if [[ "$backend" == override ]]; then
          labels=(--label 'mc-router.host=override.smoke.test,alias.smoke.test' --label 'mc-router.auto-scale-down-after=15s')
        fi
        inner service create --no-resolve-image --name "$backend" --network backends --replicas 1 \
          --endpoint-mode dnsrr "${labels[@]}" \
          --mount type=bind,source=/fixtures/backend.py,target=/backend.py,readonly \
          python:3.13-alpine python -u /backend.py
      done
      inner service create --no-resolve-image --name router --network backends --user 0 --replicas 1 \
        --constraint node.role==manager \
        --mount type=bind,source=/var/run/docker.sock,target=/var/run/docker.sock \
        --env IN_DOCKER_SWARM=true --env AUTO_SCALE_UP=true --env AUTO_SCALE_DOWN=true \
        --env AUTO_SCALE_DOWN_AFTER=60s "$router_image"
    fi
    echo 'Fixtures started. Run probe immediately; see README.md for state checks and cleanup.'
    ;;
  probe)
    shift 2
    probe_name="probe-$(date +%s)-${RANDOM}"
    probe_timeout=${SMOKE_PROBE_TIMEOUT:-180}
    if [[ ! "$probe_timeout" =~ ^[1-9][0-9]*$ ]]; then
      echo 'SMOKE_PROBE_TIMEOUT must be a positive integer.' >&2
      exit 2
    fi
    trap 'inner rm -f "$probe_name" >/dev/null 2>&1 || true' EXIT
    inner run -d --name "$probe_name" --network backends \
      --mount type=bind,source=/fixtures/probe.py,target=/probe.py,readonly \
      python:3.13-alpine python -u /probe.py "$@" >/dev/null
    deadline=$((SECONDS + probe_timeout))
    while (( SECONDS < deadline )); do
      status=$(inner inspect --format '{{.State.Running}} {{.State.ExitCode}}' "$probe_name" 2>/dev/null || true)
      if [[ "$status" == false\ * ]]; then
        inner logs "$probe_name"
        exit "${status#false }"
      fi
      sleep 1
    done
    inner logs "$probe_name"
    echo "FAIL: probe exceeded ${probe_timeout}s" >&2
    exit 1
    ;;
  state)
    if [[ "$provider" == docker ]]; then
      inner inspect --format '{{.Name}} running={{.State.Running}} finished={{.State.FinishedAt}}' inherited override
      inner logs --tail 40 router
    else
      inner service ls --format '{{.Name}} replicas={{.Replicas}}'
      inner service logs --tail 40 router
    fi
    ;;
  down)
    compose down --volumes --remove-orphans
    if docker image inspect "$router_image" >/dev/null 2>&1; then
      docker image rm "$router_image"
    fi
    ;;
  *) usage; exit 2 ;;
esac
