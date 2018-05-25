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

[These deployments](docs/k8s-example.yaml) declare an `mc-router` that exposes a node port service 
on the standard Minecraft server port 25565. Two "backend" Minecraft servers are declared as example
where users can choose stable/vanilla or snapshot simply based on the hostname they used.

```bash
kubectl apply -f https://raw.githubusercontent.com/itzg/mc-router/master/docs/k8s-example.yaml
```

![](docs/example-deployment.drawio.png)

**Note**: this deployment assumes two persistent volume claims: `mc-stable` and `mc-snapshot`

## Coming Soon

* Make `mc-router` kubernetes service aware. It would watch for backend instances with well known annotations
  and dynamically create/remove routes accordingly