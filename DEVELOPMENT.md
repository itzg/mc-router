## Developing Docker discovery on non-Linux

This works best with the included devcontaner setup, which includes attaching the host's docker socket to the dev container at `/var/run/docker.sock`.

On Windows, can create the devcontainer using:

![image.png](docs/create-dev-container.png)

Within the devcontainer, start the vanilla example server with:

```shell
docker compose -f examples/docker-discovery/compose.yml run vanilla
```

Start mc-router directly in the devcontainer.

## Using skaffold

Verified with skaffold v2.23.0

```
skaffold dev --kube-context=docker-desktop --default-repo=gcr.io/YOURS
```

- `YOURS` with your github username or the repo entirely

Also be sure to kubectl apply the minecraft deployment such as `docs/k8s-mc-with-default.yaml`

## Manual test cases

### Docker discovery with auto-scaling

1. Build the image given `Dockerfile` and tag it as `itzg/mc-router`
2. Start examples/docker-autoscale-multi/compose.yml
3. Wait for initial scale down of vanilla container
4. With Minecraft client, do a ping/list and ensure asleep MOTD comes back
   - Nothing happens with scaling or timer
5. Connect
   - Watch for scaling up of vanilla container
   - Client should fully connect after vanilla container is up
6. Stay connected longer than the 1 minute scale down timer
   - Ensure vanilla container is still up
7. Disconnect
8. Server list refresh to cause ping
   - ensure scale-down timer started again
9. Wait for scale down of vanilla container