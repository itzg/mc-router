Routes Minecraft client connections to backend servers based upon the requested server address.

## Usage

```text
Flags:
  --help                     Show context-sensitive help (also try --help-long
                             and --help-man).
  --port=25565               The port bound to listen for Minecraft client
                             connections
  --api-binding=API-BINDING  The host:port bound for servicing API requests
  --mapping=MAPPING ...      Mapping of external hostname to internal server
                             host:port
```

## REST API

* `GET /routes`
   Retrieves the currently configured routes
* `POST /routes`
   Registers a route given a JSON body structured like:
```json
{
  "serverAddress": "CLIENT REQUESTED SERVER ADDRESS",
  "backend": "HOST:PORT"
}
```

* `DELETE /routes/{serverAddress}`
  Deletes an existing route for the given `serverAddress`
  
## Example kubernetes deployment

[This example deployment](docs/k8s-example-auto.yaml) 
* Declares an `mc-router` service that exposes a node port 25565
* Declares a service account with access to watch and list services
* Declares `--in-kube-cluster` in the `mc-router` container arguments
* Two "backend" Minecraft servers are declared each with an 
  `"mc-router.itzg.me/externalServerName"` annotation that declares their external server name

```bash
kubectl apply -f https://raw.githubusercontent.com/itzg/mc-router/master/docs/k8s-example-auto.yaml
```

![](docs/example-deployment-auto.drawio.png)

#### Notes
* This deployment assumes two persistent volume claims: `mc-stable` and `mc-snapshot`
* I extended the allowed node port range by adding `--service-node-port-range=25000-32767` 
  to `/etc/kubernetes/manifests/kube-apiserver.yaml` 
