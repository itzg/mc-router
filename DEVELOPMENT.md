## Developing Docker discovery on non-Linux

This works best with the included devcontaner setup, which includes attaching the host's docker socket to the dev container at `/var/run/docker.sock`.

On Windows, can create the devcontainer using:

![image.png](docs/create-dev-container.png)

Within the devcontainer, start the vanilla example server with:

```shell
docker compose -f examples/docker-discovery/compose.yml run vanilla
```

Start mc-router directly in the devcontainer.