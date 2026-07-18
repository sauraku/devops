package models

type Config struct {
	Token               string
	Host                string
	Port                int
	BaseDir             string
	DataDir             string
	RunDir              string
	SocketPath          string
	CookieSecret        string
	AllowedRepoPrefixes []string
	DefaultProjectID    string
	EnableRestore       bool
	BackupSchedule      string
	BackupRetention     int
	ProjectRoot         string
	GithubToken         string
	GithubUser          string
	RunnerImage         string
	RunnerControlURL    string
	RunnerNetwork       string
	CookieSecure        bool
	EnableTerminal      bool
	SSHKnownHosts       string
}

type ContainerState struct {
	Container      string                   `json:"container"`
	Exists         bool                     `json:"exists"`
	Running        bool                     `json:"running"`
	State          string                   `json:"state"`
	Health         string                   `json:"docker_health"`
	NetworkIP      string                   `json:"network_ip"`
	PublishedPorts map[string][]PortBinding `json:"published_ports"`
}

type PortBinding struct {
	Host string `json:"host"`
	Port string `json:"port"`
}

type ServiceHealth struct {
	Service        string `json:"service"`
	Container      string `json:"container"`
	ContainerState string `json:"container_state"`
	Status         string `json:"status"`
	Detail         string `json:"detail"`
}

type ProjectStatus struct {
	Project      *Project                     `json:"project"`
	State        map[string]any               `json:"state"`
	Lock         *DeployLock                  `json:"lock"`
	Runner       map[string]string            `json:"runner"`
	Containers   map[string]map[string]string `json:"containers"`
	Health       map[string]*ServiceHealth    `json:"service_health"`
	Deployments  []*Deployment                `json:"recent_deployments"`
	Backups      []*Backup                    `json:"recent_backups"`
	Capabilities map[string]bool              `json:"capabilities"`
	LogDir       string                       `json:"log_dir"`
	ServerTime   string                       `json:"server_time"`
}
