package composepolicy

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

const maxRenderedComposeSize = 8 << 20

type renderedResourceConfig struct {
	Name       string         `json:"name"`
	External   bool           `json:"external"`
	Driver     string         `json:"driver"`
	DriverOpts map[string]any `json:"driver_opts"`
}

func ValidateRenderedFile(path, projectName string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open rendered Compose config: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxRenderedComposeSize+1))
	if err != nil {
		return fmt.Errorf("read rendered Compose config: %w", err)
	}
	return ValidateRendered(data, projectName)
}

// ValidateRendered validates the normalized JSON emitted by Docker Compose.
// Source validation runs first to stop pre-render file reads; this second layer
// catches interpolation-dependent values and daemon-facing normalization.
func ValidateRendered(output []byte, projectName string) error {
	if len(output) > maxRenderedComposeSize {
		return fmt.Errorf("Compose policy violation: rendered Compose model exceeds %d bytes", maxRenderedComposeSize)
	}
	var config struct {
		Services map[string]map[string]any         `json:"services"`
		Volumes  map[string]renderedResourceConfig `json:"volumes"`
		Networks map[string]renderedResourceConfig `json:"networks"`
	}
	if err := json.Unmarshal(output, &config); err != nil {
		return fmt.Errorf("parse rendered Compose config: %w", err)
	}
	if projectName == "" {
		return fmt.Errorf("Compose policy violation: project name is empty")
	}
	if len(config.Services) == 0 {
		return fmt.Errorf("Compose policy violation: no services are declared")
	}

	for kind, resources := range map[string]map[string]renderedResourceConfig{
		"volume":  config.Volumes,
		"network": config.Networks,
	} {
		for logicalName, resource := range resources {
			if resource.External {
				return fmt.Errorf("Compose policy violation: %s %s is external", kind, logicalName)
			}
			if !strings.HasPrefix(resource.Name, projectName+"_") {
				return fmt.Errorf("Compose policy violation: %s %s resolves to %q outside Compose project %s", kind, logicalName, resource.Name, projectName)
			}
			driver := strings.TrimSpace(resource.Driver)
			safeDriver := driver == "" ||
				(kind == "volume" && driver == "local") ||
				(kind == "network" && driver == "bridge")
			if !safeDriver {
				return fmt.Errorf("Compose policy violation: %s %s uses forbidden driver %q", kind, logicalName, driver)
			}
			if len(resource.DriverOpts) != 0 {
				return fmt.Errorf("Compose policy violation: %s %s sets driver_opts", kind, logicalName)
			}
		}
	}

	for service, cfg := range config.Services {
		for _, field := range []string{"network_mode", "pid", "ipc"} {
			if value, _ := cfg[field].(string); strings.HasPrefix(strings.TrimSpace(value), "container:") {
				return fmt.Errorf("Compose policy violation: %s uses %s from another container", service, field)
			}
		}
		for field, reason := range forbiddenServiceKeys {
			if value, present := cfg[field]; present && composeValueIsSet(value) {
				return fmt.Errorf("Compose policy violation: %s sets %s (%s)", service, field, reason)
			}
		}
		if value, present := cfg["security_opt"]; present {
			values, ok := value.([]any)
			if !ok || len(values) != 1 || values[0] != "no-new-privileges:true" {
				return fmt.Errorf("Compose policy violation: %s security_opt may contain only no-new-privileges:true", service)
			}
		}
		if values, ok := cfg["volumes_from"].([]any); ok {
			for _, raw := range values {
				reference, _ := raw.(string)
				if strings.HasPrefix(reference, "container:") {
					return fmt.Errorf("Compose policy violation: %s uses volumes_from from another container", service)
				}
				referencedService := strings.SplitN(reference, ":", 2)[0]
				if _, ok := config.Services[referencedService]; !ok {
					return fmt.Errorf("Compose policy violation: %s uses volumes_from from undeclared service %s", service, referencedService)
				}
			}
		}
		if mounts, ok := cfg["volumes"].([]any); ok {
			for _, raw := range mounts {
				mount, ok := raw.(map[string]any)
				if !ok {
					return fmt.Errorf("Compose policy violation: %s has an invalid volume mount", service)
				}
				mountType, _ := mount["type"].(string)
				source, _ := mount["source"].(string)
				switch mountType {
				case "bind":
					return fmt.Errorf("Compose policy violation: %s uses forbidden bind mount %s", service, source)
				case "volume":
					if source != "" {
						if _, ok := config.Volumes[source]; !ok {
							return fmt.Errorf("Compose policy violation: %s uses undeclared named volume %s", service, source)
						}
					}
				case "tmpfs":
					// tmpfs storage is private to the container and cannot read
					// controller or host filesystem content.
				default:
					return fmt.Errorf("Compose policy violation: %s uses unsupported mount type %q", service, mountType)
				}
			}
		}
		if ports, ok := cfg["ports"].([]any); ok {
			for _, raw := range ports {
				port, ok := raw.(map[string]any)
				if !ok {
					return fmt.Errorf("Compose policy violation: %s has an invalid published port", service)
				}
				hostIP, _ := port["host_ip"].(string)
				if hostIP != "127.0.0.1" && hostIP != "::1" {
					return fmt.Errorf("Compose policy violation: %s publishes a port on %q instead of loopback", service, hostIP)
				}
			}
		}
	}
	return nil
}

func composeValueIsSet(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return strings.TrimSpace(typed) != ""
	case []any:
		return len(typed) != 0
	case map[string]any:
		return len(typed) != 0
	default:
		return true
	}
}
