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