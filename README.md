[![GitHub issues](https://img.shields.io/github/issues/itzg/mc-router.svg)](https://github.com/itzg/mc-router/issues)
[![Docker Pulls](https://img.shields.io/docker/pulls/itzg/mc-router.svg)](https://cloud.docker.com/u/itzg/repository/docker/itzg/mc-router)
[![test](https://github.com/itzg/mc-router/actions/workflows/test.yml/badge.svg)](https://github.com/itzg/mc-router/actions/workflows/test.yml)
[![GitHub release](https://img.shields.io/github/release/itzg/mc-router.svg)](https://github.com/itzg/mc-router/releases)
[![Discord](https://img.shields.io/discord/660567679458869252?label=discord)](https://discord.gg/JK2v3rJ9ec)
[![Buy me a coffee](https://img.shields.io/badge/Donate-Buy%20me%20a%20coffee-orange.svg)](https://www.buymeacoffee.com/itzg)

Routes Minecraft client connections to backend servers based upon the requested server address.

## Usage

```text
  -api-binding host:port
        The host:port bound for servicing API requests (env API_BINDING)
  -auto-scale-up
        Increase Kubernetes StatefulSet Replicas (only) from 0 to 1 on respective backend servers when accessed (env AUTO_SCALE_UP)
  -connection-rate-limit int
        Max number of connections to allow per second (env CONNECTION_RATE_LIMIT) (default 1)
  -cpu-profile string
        Enables CPU profiling and writes to given path (env CPU_PROFILE)
  -debug
        Enable debug logs (env DEBUG)
  -default string
        host:port of a default Minecraft server to use when mapping not found (env DEFAULT)
  -docker-socket
        Path to Docker socket to use (env DOCKER_SOCKET) (default "unix:///var/run/docker.sock")
  -docker-refresh-interval int
        Refresh interval in seconds for the Docker Swarm integration (env DOCKER_REFRESH_INTERVAL) (default 15)
  -docker-timeout int
        Timeout configuration in seconds for the Docker Swarm integration (env DOCKER_TIMEOUT)
  -in-docker
        Use Docker service discovery (env IN_DOCKER)
  -in-docker-swarm
        Use Docker Swarm service discovery (env IN_DOCKER_SWARM)
  -in-kube-cluster
        Use in-cluster Kubernetes config (env IN_KUBE_CLUSTER)
  -kube-config string
        The path to a Kubernetes configuration file (env KUBE_CONFIG)
  -mapping value
        Comma-separated or repeated mappings of externalHostname=host:port (env MAPPING)
  -metrics-backend string
        Backend to use for metrics exposure/publishing: discard,expvar,influxdb (env METRICS_BACKEND) (default "discard")
  -metrics-backend-config-influxdb-addr string
         (env METRICS_BACKEND_CONFIG_INFLUXDB_ADDR)
  -metrics-backend-config-influxdb-database string
         (env METRICS_BACKEND_CONFIG_INFLUXDB_DATABASE)
  -metrics-backend-config-influxdb-interval duration
         (env METRICS_BACKEND_CONFIG_INFLUXDB_INTERVAL) (default 1m0s)
  -metrics-backend-config-influxdb-password string
         (env METRICS_BACKEND_CONFIG_INFLUXDB_PASSWORD)
  -metrics-backend-config-influxdb-retention-policy string
         (env METRICS_BACKEND_CONFIG_INFLUXDB_RETENTION_POLICY)
  -metrics-backend-config-influxdb-tags value
        any extra tags to be included with all reported metrics (env METRICS_BACKEND_CONFIG_INFLUXDB_TAGS)
  -metrics-backend-config-influxdb-username string
         (env METRICS_BACKEND_CONFIG_INFLUXDB_USERNAME)
  -ngrok-token string
        If set, an ngrok tunnel will be established. It is HIGHLY recommended to pass as an environment variable. (env NGROK_TOKEN)
  -port port
        The port bound to listen for Minecraft client connections (env PORT) (default 25565)
  -receive-proxy-protocol
        Receive PROXY protocol from backend servers, by default trusts every proxy header that it receives, combine with -trusted-proxies to specify a list of trusted proxies (env RECEIVE_PROXY_PROTOCOL)
  -routes-config string
        Name or full path to routes config file (env ROUTES_CONFIG)
  -simplify-srv
        Simplify fully qualified SRV records for mapping (env SIMPLIFY_SRV)
  -trusted-proxies value
        Comma delimited list of CIDR notation IP blocks to trust when receiving PROXY protocol (env TRUSTED_PROXIES)
  -use-proxy-protocol
        Send PROXY protocol to backend servers (env USE_PROXY_PROTOCOL)
  -version
        Output version and exit (env VERSION)
```


## Docker Multi-Architecture Image

The [multi-architecture image published at Docker Hub](https://hub.docker.com/repository/docker/itzg/mc-router) supports amd64, arm64, and arm32v6 (i.e. RaspberryPi).

## Docker Compose Usage

The diagram below shows how this `docker-compose.yml` configures two Minecraft server services named `vanilla` and `forge`, which also become the internal network aliases. _Notice those services don't need their ports exposed since the internal networking allows for the inter-container access._

```yaml
version: "3.8"

services:
  vanilla:
    image: itzg/minecraft-server
    environment:
      EULA: "TRUE"
  forge:
    image: itzg/minecraft-server
    environment:
      EULA: "TRUE"
      TYPE: FORGE
  router:
    image: ${MC_ROUTER_IMAGE:-itzg/mc-router}
    depends_on:
      - forge
      - vanilla
    environment:
      MAPPING: |
        vanilla.example.com=vanilla:25565
        forge.example.com=forge:25565
    ports:
      - "25565:25565"
```

The `router` service is only one of the services that needs to exposed on the external network. The `MAPPING` declares how the hostname users will enter into their Minecraft client will map to the internal services.

![](docs/compose-diagram.png)

To test out this example, add these two entries to my "hosts" file:

```
127.0.0.1 vanilla.example.com
127.0.0.1 forge.example.com
```

### Using Docker auto-discovery

When running `mc-router` in a Docker environment you can pass the `--in-docker` or `--in-docker-swarm` 
command-line argument and it will poll the Docker API periodically to find all the running
containers/services for Minecraft instances. To enable discovery you have to set the `mc-router.host`
label on the container. These are the labels scanned:

- `mc-router.host`: Used to configure the hostname the Minecraft clients would use to 
  connect to the server. The container/service endpoint will be used as the routed backend. You can 
  use more than one hostname by splitting it with a comma.
- `mc-router.port`: This value must be set to the port the Minecraft server is listening on.
  The default value is 25565.
- `mc-router.default`: Set this to a truthy value to make this server the default backend.
  Please note that `mc-router.host` is still required to be set.
- `mc-router.network`: Specify the network you are using for the router if multiple are 
  present in the container/service. You can either use the network ID, it's full name or an alias.

#### Example Docker deployment

Refer to [this example docker-compose.yml](docs/sd-docker.docker-compose.yml) to see how to
configure two different Minecraft servers and a `mc-router` instance for use with Docker service discovery.

#### Example Docker Swarm deployment

Refer to [this example docker-compose.yml](docs/swarm.docker-compose.yml) to see how to
configure two different Minecraft servers and a `mc-router` instance for use with Docker Swarm service discovery.

## Routing Configuration

The routing configuration allows routing via a config file rather than a command. 
You need to set `-routes-config` or `ROUTES_CONFIG` env variable.
The following shows a JSON file for routes config, where `default-server` can also be `null` or omitted:

```json
{
  "default-server": "vanilla:25565",
  "mappings": {
    "vanilla.example.com": "vanilla:25565",
    "forge.example.com": "forge:25565"
  }
}
```

## Kubernetes Usage

### Using Kubernetes Service auto-discovery

When running `mc-router` as a Kubernetes Pod and you pass the `--in-kube-cluster` command-line argument, then it will automatically watch for any services annotated with
- `mc-router.itzg.me/externalServerName` : The value of the annotation will be registered as the external hostname Minecraft clients would used to connect to the routed service. The service's clusterIP and target port are used as the routed backend. You can use more hostnames by splitting them with comma.
- `mc-router.itzg.me/defaultServer` : The service's clusterIP and target port are used as the default if no other `externalServiceName` annotations applies.

For example, start `mc-router`'s container spec with

```yaml
image: itzg/mc-router
name: mc-router
args: ["--in-kube-cluster"]
```

and configure the backend minecraft server's service with the annotation:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: mc-forge
  annotations:
    "mc-router.itzg.me/externalServerName": "external.host.name"
```

you can use multiple host names:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: mc-forge
  annotations:
    "mc-router.itzg.me/externalServerName": "external.host.name,other.host.name"
```

mc-router will pick the service port named either `minecraft` or `mc-router`. If neither port names exist, it will use port value 25565.

### Example Kubernetes deployment

[This example deployment](docs/k8s-example-auto.yaml)
* Declares an `mc-router` service that exposes a node port 25565
* Declares a service account with access to watch and list services
* Declares `--in-kube-cluster` in the `mc-router` container arguments
* Two "backend" Minecraft servers are declared each with an
  `"mc-router.itzg.me/externalServerName"` annotation that declares their external server name(s)

```bash
kubectl apply -f https://raw.githubusercontent.com/itzg/mc-router/master/docs/k8s-example-auto.yaml
```

![](docs/example-deployment-auto.drawio.png)

##### Notes
* This deployment assumes two persistent volume claims: `mc-stable` and `mc-snapshot`
* I extended the allowed node port range by adding `--service-node-port-range=25000-32767`
  to `/etc/kubernetes/manifests/kube-apiserver.yaml`

##### Auto Scale Up

The `-auto-scale-up` flag argument makes the router "wake up" any stopped backend servers, by changing `replicas: 0` to `replicas: 1`.

This requires using `kind: StatefulSet` instead of `kind: Service` for the Minecraft backend servers.

It also requires the `ClusterRole` to permit `get` + `update` for `statefulsets` & `statefulsets/scale`,
e.g. like this (or some equivalent more fine-grained one to only watch/list services+statefulsets, and only get+update scale):

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: services-watcher
rules:
- apiGroups: [""]
  resources: ["services"]
  verbs: ["watch","list"]
- apiGroups: ["apps"]
  resources: ["statefulsets", "statefulsets/scale"]
  verbs: ["watch","list","get","update"]
```

## REST API

* `GET /routes` (with `Accept: application/json`)

  Retrieves the currently configured routes

* `POST /routes` (with `Content-Type: application/json`)

  Registers a route given a JSON body structured like:
  ```json
  {
    "serverAddress": "CLIENT REQUESTED SERVER ADDRESS",
    "backend": "HOST:PORT"
  }
  ```

* `POST /defaultRoute` (with `Content-Type: application/json`)

  Registers a default route to the given backend. JSON body is structured as:
  ```json
  {
    "backend": "HOST:PORT"
  }
  ```

* `DELETE /routes/{serverAddress}`

  Deletes an existing route for the given `serverAddress`

## ngrok

mc-router has built-in support to run as an [ngrok agent](https://ngrok.com/docs/secure-tunnels/ngrok-agent/). To enable this support, pass [an ngrok authtoken](https://ngrok.com/docs/secure-tunnels/ngrok-agent/tunnel-authtokens/#per-agent-authtokens) to the command-line argument or environment variable, [shown above](#usage).

### Ngrok Quick Start

Create/access an ngrok account and [allocate an agent authtoken from the dashboard](https://dashboard.ngrok.com/tunnels/authtokens).

In a new directory, create a file called `.env` with the allocated token

```dotenv
NGROK_TOKEN=...
```

In the same directory, create the following compose file:

```yaml
version: "3.8"

services:
  mc:
    image: itzg/minecraft-server
    environment:
      EULA: true
    volumes:
      - mc-data:/data
    # No port mapping since mc-router connects over compose network
  router:
    image: itzg/mc-router
    environment:
      DEFAULT: mc:25565
      NGROK_TOKEN: ${NGROK_TOKEN}
    # No port mapping needed since it routes through ngrok tunnel

volumes:
  mc-data: {}
```

Start the compose project:

```shell
docker compose up -d
```

Grab the mc-router logs using:

```shell
docker compose logs router
```

From those logs, locate the `ngrokUrl` parameter from the "Listening" info log message, such as `tcp://8.tcp.ngrok.io:99999`.

In the Minecraft client, the server address will be the part after the "tcp://" prefix, such as `8.tcp.ngrok.io:99999`.

## Development

### Building locally with Docker

```bash
docker build -t mc-router .
```

### Build locally without Docker

After [installing Go](https://go.dev/doc/install) and doing a `go mod download` to install all required prerequisites, just like the [Dockerfile](Dockerfile) does, you can:

```bash
make test # go test -v ./...
go build ./cmd/mc-router/
```

### Skaffold

For "in-cluster development" it's convenient to use https://skaffold.dev. Any changes to Go source code
will trigger a go build, new container image pushed to registry with a new tag, and refresh in Kubernetes
with the image tag used in the deployment transparently updated to the new tag and thus new pod created pulling new images,
as configured by [skaffold.yaml](skaffold.yaml):

    skaffold dev

When using Google Cloud (GCP), first create a _Docker Artifact Registry_,
then add the _Artifact Registry Reader_ Role to the _Compute Engine default service account_ of your _GKE `clusterService` Account_ (to avoid error like "container mc-router is waiting to start: ...-docker.pkg.dev/... can't be pulled"),
then use e.g. `gcloud auth configure-docker europe-docker.pkg.dev` or equivalent one time (to create a `~/.docker/config.json`),
and then use e.g. `--default-repo=europe-docker.pkg.dev/YOUR-PROJECT/YOUR-ARTIFACT-REGISTRY` option for `skaffold dev`.

### Performing snapshot release with Docker

```bash
docker run -it --rm \
  -v ${PWD}:/build -w /build \
  -v /var/run/docker.sock:/var/run/docker.sock \
  goreleaser/goreleaser \
  release --snapshot --rm-dist
```

## Related Projects

* https://github.com/haveachin/infrared
