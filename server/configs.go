package server

type WebhookConfig struct {
	Url         string `usage:"If set, a POST request that contains connection status notifications will be sent to this HTTP address"`
	RequireUser bool   `default:"false" usage:"Indicates if the webhook will only be called if a user is connecting rather than just server list/ping"`
}

type AutoScale struct {
	Up         bool   `usage:"Scale from zero on access. For Kubernetes, increases StatefulSet replicas from 0 to 1. For Docker, starts or unpauses the container when accessed"`
	Down       bool   `default:"false" usage:"Scale to zero after idle. For Kubernetes, decreases StatefulSet replicas from 1 to 0. For Docker, gracefully stops the container when there are no connections"`
	DownAfter  string `default:"10m" usage:"Server scale down delay after there are no connections"`
	AllowDeny  string `usage:"Path to config for server allowlists and denylists. If a global/server entry is specified, only players allowed to connect to the server will be able to trigger a scale up when -auto-scale-up is enabled or cancel active down scalers when -auto-scale-down is enabled"`
	AsleepMOTD string `usage:"MOTD to display when auto-scaled down servers are accessed; if empty, no status will be served"`
}

type RoutesConfig struct {
	Config      string `usage:"Name or full [path] to routes config file"`
	ConfigWatch bool   `usage:"Watch for config file changes"`
}

type NgrokConfig struct {
	Token      string `usage:"If set, an ngrok tunnel will be established. It is HIGHLY recommended to pass as an environment variable."`
	RemoteAddr string `usage:"If set, the TCP address to request for this edge"`
}

type Config struct {
	Port                  int               `default:"25565" usage:"The [port] bound to listen for Minecraft client connections"`
	Default               string            `usage:"host:port of a default Minecraft server to use when mapping not found"`
	Mapping               map[string]string `usage:"Comma or newline delimited or repeated mappings of externalHostname=host:port"`
	ApiBinding            string            `usage:"The [host:port] bound for servicing API requests"`
	CpuProfile            string            `usage:"Enables CPU profiling and writes to given path"`
	ConnectionRateLimit   int               `default:"1" usage:"Max number of connections to allow per second"`
	InKubeCluster         bool              `usage:"Use in-cluster Kubernetes config"`
	KubeConfig            string            `usage:"The path to a Kubernetes configuration file"`
	KubeNamespace         string            `usage:"The namespace to watch or blank for all, which is the default"`
	InDocker              bool              `usage:"Use Docker service discovery"`
	InDockerSwarm         bool              `usage:"Use Docker Swarm service discovery"`
	DockerSocket          string            `default:"unix:///var/run/docker.sock" usage:"Path to Docker socket to use"`
	DockerTimeout         int               `default:"0" usage:"Timeout configuration in seconds for the Docker integrations"`
	DockerRefreshInterval int               `default:"15" usage:"Refresh interval in seconds for the Docker integrations"`
	DockerApiVersion      string            `usage:"Instead of auto-negotiating, use specific Docker API version"`
	MetricsBackend        string            `default:"discard" usage:"Backend to use for metrics exposure/publishing: discard,expvar,influxdb,prometheus"`
	MetricsBackendConfig  MetricsBackendConfig
	UseProxyProtocol      bool     `default:"false" usage:"Send PROXY protocol to backend servers"`
	ReceiveProxyProtocol  bool     `default:"false" usage:"Receive PROXY protocol from backend servers, by default trusts every proxy header that it receives, combine with -trusted-proxies to specify a list of trusted proxies"`
	TrustedProxies        []string `usage:"Comma delimited list of CIDR notation IP blocks to trust when receiving PROXY protocol"`
	RecordLogins          bool     `default:"false" usage:"Log and generate metrics on player logins. Metrics only supported with influxdb or prometheus backend"`
	Routes                RoutesConfig
	Ngrok                 NgrokConfig
	AutoScale             AutoScale

	ClientsToAllow []string `usage:"Zero or more client IP addresses or CIDRs to allow. Takes precedence over deny."`
	ClientsToDeny  []string `usage:"Zero or more client IP addresses or CIDRs to deny. Ignored if any configured to allow"`

	SimplifySRV bool `default:"false" usage:"Simplify fully qualified SRV records for mapping"`

	Webhook WebhookConfig `usage:"Webhook configuration"`
}
