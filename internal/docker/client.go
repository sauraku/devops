package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sauraku/devops-control/internal/models"
)

type Client struct {
	composeBinary string
}

// Compose can take longer than the default Docker stop grace period while it
// concurrently stops stateful services. Keep deletion and runner teardown
// bounded, but do not kill Compose after it has successfully removed resources
// and before it has returned its final success status.
const composeDownTimeout = 2 * time.Minute

const ContainerReadFileLimit = 10 * 1024 * 1024

var ErrContainerFileTooLarge = errors.New("container file exceeds read limit")

func NewClient() *Client {
	c := &Client{}
	c.detectComposeBinary()
	return c
}

func (c *Client) detectComposeBinary() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "compose", "version").Run(); err == nil {
		c.composeBinary = "docker compose"
	} else {
		c.composeBinary = "docker-compose"
	}
}

func (c *Client) ComposeCommand() []string {
	return strings.Split(c.composeBinary, " ")
}

func (c *Client) InspectContainer(name string) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", "inspect", name)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("inspect container %s: %w", name, err)
	}

	var containers []map[string]any
	if err := json.Unmarshal(output, &containers); err != nil || len(containers) == 0 {
		return nil, fmt.Errorf("parse inspect: %w", err)
	}
	return containers[0], nil
}

func (c *Client) ContainerSummary(name string) *models.ContainerState {
	info, err := c.InspectContainer(name)
	if err != nil {
		return &models.ContainerState{
			Container: name,
			Exists:    false,
			Running:   false,
			State:     "unavailable",
		}
	}

	state, _ := info["State"].(map[string]any)
	running, _ := state["Running"].(bool)
	status, _ := state["Status"].(string)

	var health string
	if h, ok := state["Health"].(map[string]any); ok {
		health, _ = h["Status"].(string)
	}

	netSettings, _ := info["NetworkSettings"].(map[string]any)
	networks, _ := netSettings["Networks"].(map[string]any)
	networkIP := ""
	for _, nw := range networks {
		if n, ok := nw.(map[string]any); ok {
			if ip, ok := n["IPAddress"].(string); ok && ip != "" {
				networkIP = ip
				break
			}
		}
	}

	publishedPorts := make(map[string][]models.PortBinding)
	ports, _ := netSettings["Ports"].(map[string]any)
	for containerPort, bindings := range ports {
		bindingList, ok := bindings.([]any)
		if !ok {
			continue
		}
		port := strings.Split(containerPort, "/")[0]
		for _, b := range bindingList {
			binding, ok := b.(map[string]any)
			if !ok {
				continue
			}
			hostPort, _ := binding["HostPort"].(string)
			hostIP, _ := binding["HostIp"].(string)
			if hostPort == "" {
				continue
			}
			publishedPorts[port] = append(publishedPorts[port], models.PortBinding{
				Host: normalizeHostIP(hostIP),
				Port: hostPort,
			})
		}
	}

	return &models.ContainerState{
		Container:      name,
		Exists:         true,
		Running:        running,
		State:          status,
		Health:         health,
		NetworkIP:      networkIP,
		PublishedPorts: publishedPorts,
	}
}

func (c *Client) RemoveContainer(name string) (bool, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", "rm", "-f", name)
	output, err := cmd.CombinedOutput()
	outputStr := strings.ToLower(string(output))
	if err != nil {
		if strings.Contains(outputStr, "no such container") {
			return true, fmt.Sprintf("Container %s was already absent.", name)
		}
		if strings.Contains(outputStr, "already in progress") {
			return true, fmt.Sprintf("Container %s removal already in progress.", name)
		}
		return false, fmt.Sprintf("Failed to remove container %s: %s", name, strings.TrimSpace(string(output)))
	}
	return true, fmt.Sprintf("Removed container %s.", name)
}

func (c *Client) ContainerLogs(name string, tail int) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", "logs", "--tail", fmt.Sprintf("%d", tail), name)
	output, err := cmd.Output()
	if err != nil {
		return fmt.Sprintf("Unable to read docker logs for %s: %s\n", name, err)
	}
	return string(output)
}

func (c *Client) ContainerLogsSince(name string, tail int, since string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	args := []string{"logs", "--tail", fmt.Sprintf("%d", tail)}
	if since != "" {
		args = append(args, "--since", since)
	}
	args = append(args, name)
	cmd := exec.CommandContext(ctx, "docker", args...)
	output, err := cmd.Output()
	if err != nil {
		return fmt.Sprintf("Unable to read docker logs for %s: %s\n", name, err)
	}
	return string(output)
}

func (c *Client) ContainerReadFile(containerName, filePath string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", "exec", containerName, "cat", "--", filePath)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("open container file stream: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start container file read: %w", err)
	}
	output, readErr := io.ReadAll(io.LimitReader(stdout, ContainerReadFileLimit+1))
	if len(output) > ContainerReadFileLimit {
		cancel()
		_ = cmd.Wait()
		return "", fmt.Errorf("%w (%d bytes)", ErrContainerFileTooLarge, ContainerReadFileLimit)
	}
	if readErr != nil {
		cancel()
		_ = cmd.Wait()
		return "", fmt.Errorf("read container file stream: %w", readErr)
	}
	if err := cmd.Wait(); err != nil {
		return "", fmt.Errorf("read file from container: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return string(output), nil
}

func (c *Client) WaitForRunnerReady(name string, timeout time.Duration) (bool, string, string) {
	deadline := time.Now().Add(timeout)
	failurePatterns := []string{
		"github actions runner registration failed",
		"expired or invalid registration token",
		"http response code: notfound",
	}
	readyPatterns := []string{
		"listening for jobs",
		"running job:",
	}

	var logs, state string
	for time.Now().Before(deadline) {
		summary := c.ContainerSummary(name)
		state = summary.State
		logs = c.ContainerLogs(name, 50)
		logsLower := strings.ToLower(logs)

		for _, p := range failurePatterns {
			if strings.Contains(logsLower, p) {
				return false, state, logs
			}
		}
		for _, p := range readyPatterns {
			if strings.Contains(logsLower, p) {
				return true, state, logs
			}
		}
		if state != "running" && state != "created" && state != "restarting" {
			return false, state, logs
		}
		time.Sleep(1 * time.Second)
	}
	return false, state, logs
}

func (c *Client) ExecHTTPHealth(containerName string, port int, path string) (string, string) {
	script := fmt.Sprintf(`
const http = require('http');
const port = Number(process.argv[1]);
const p = process.argv[2];
const req = http.request({host:'127.0.0.1', port, path:p, timeout:1000}, (res) => {
  res.resume();
  res.on('end', () => process.exit(res.statusCode >= 200 && res.statusCode < 400 ? 0 : 2));
});
req.on('timeout', () => req.destroy(new Error('timeout')));
req.on('error', () => process.exit(3));
req.end();
`)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", "exec", containerName, "node", "-e", script, fmt.Sprintf("%d", port), path)
	err := cmd.Run()
	if err == nil {
		return "healthy", fmt.Sprintf("In-container HTTP %s probe succeeded.", path)
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		if exitErr.ExitCode() == 2 {
			return "unhealthy", fmt.Sprintf("In-container HTTP %s probe returned non-success.", path)
		}
	}
	return "unavailable", fmt.Sprintf("In-container HTTP %s probe failed.", path)
}

func (c *Client) ComposeUp(composeFile, projectName string, envVars map[string]string, services ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cmdArgs := append(c.ComposeCommand(), "-f", composeFile, "-p", projectName)
	cmdArgs = append(cmdArgs, "up", "-d")
	cmdArgs = append(cmdArgs, services...)

	cmd := exec.CommandContext(ctx, cmdArgs[0], cmdArgs[1:]...)
	cmd.Env = dockerCommandEnv(envVars)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("compose up failed: %s: %s", err, strings.TrimSpace(string(output)))
	}

	// Enforce restart: unless-stopped on the exact Compose-owned containers so
	// this remains correct for custom container names and Compose naming changes.
	for _, svc := range services {
		name, err := c.FindComposeContainer(projectName, svc)
		if err != nil {
			return fmt.Errorf("resolve started Compose service %s/%s: %w", projectName, svc, err)
		}
		if name == "" {
			return fmt.Errorf("started Compose service %s/%s was not found", projectName, svc)
		}
		ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
		updateOutput, updateErr := exec.CommandContext(ctx2, "docker", "update", "--restart", "unless-stopped", name).CombinedOutput()
		cancel2()
		if updateErr != nil {
			return fmt.Errorf("set restart policy for Compose service %s/%s: %w: %s", projectName, svc, updateErr, strings.TrimSpace(string(updateOutput)))
		}
	}
	return nil
}

func (c *Client) ComposeDown(composeFile, projectName string, envVars map[string]string) error {
	return c.ComposeDownWithEnvFile(composeFile, projectName, "", envVars)
}

func (c *Client) ComposeDownWithEnvFile(composeFile, projectName, envFile string, envVars map[string]string) error {
	ctx, cancel := context.WithTimeout(context.Background(), composeDownTimeout)
	defer cancel()

	cmdArgs := append(c.ComposeCommand(), "-f", composeFile, "-p", projectName)
	if envFile != "" {
		if _, err := os.Stat(envFile); err != nil {
			return fmt.Errorf("use Compose environment file %s: %w", envFile, err)
		}
		cmdArgs = append(cmdArgs, "--env-file", envFile)
	}
	cmdArgs = append(cmdArgs, "down", "--remove-orphans")
	cmd := exec.CommandContext(ctx, cmdArgs[0], cmdArgs[1:]...)
	cmd.Env = dockerCommandEnv(envVars)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("compose down failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// RemoveComposeVolumes removes only Docker volumes that Compose labelled as
// belonging to projectName. Project deletion calls this after Compose has
// stopped and verified removal of its containers. Using labels rather than
// `compose down --volumes` keeps a changed or malformed Compose file from
// naming an unrelated host volume for removal.
func (c *Client) RemoveComposeVolumes(projectName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	list := exec.CommandContext(ctx, "docker", "volume", "ls",
		"--filter", "label=com.docker.compose.project="+projectName,
		"--format", "{{.Name}}")
	output, err := list.Output()
	if err != nil {
		return fmt.Errorf("list Compose volumes for %s: %w", projectName, err)
	}

	for _, volume := range strings.Fields(string(output)) {
		remove := exec.CommandContext(ctx, "docker", "volume", "rm", volume)
		removeOutput, err := remove.CombinedOutput()
		if err != nil {
			message := strings.TrimSpace(string(removeOutput))
			if strings.Contains(strings.ToLower(message), "no such volume") {
				continue
			}
			return fmt.Errorf("remove Compose volume %s for %s: %w: %s", volume, projectName, err, message)
		}
	}
	return nil
}

func (c *Client) ListContainers(filter string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	args := []string{"ps", "-a", "--format", "{{.Names}}"}
	if filter != "" {
		args = append(args, "--filter", filter)
	}
	cmd := exec.CommandContext(ctx, "docker", args...)
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	names := strings.Fields(string(output))
	return names, nil
}

// FindComposeContainer returns the single container owned by an exact Compose
// project and service. Container names are not an ownership signal.
func (c *Client) FindComposeContainer(projectName, service string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	args := []string{
		"ps", "-a", "--format", "{{.Names}}",
		"--filter", "label=com.docker.compose.project=" + projectName,
		"--filter", "label=com.docker.compose.service=" + service,
	}
	output, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		return "", fmt.Errorf("list Compose container for %s/%s: %w", projectName, service, err)
	}
	names := strings.Fields(string(output))
	switch len(names) {
	case 0:
		return "", nil
	case 1:
		return names[0], nil
	default:
		return "", fmt.Errorf("multiple containers claim Compose ownership %s/%s: %s", projectName, service, strings.Join(names, ", "))
	}
}

func (c *Client) ListComposeContainers(projectName string) ([]string, error) {
	return c.ListContainers("label=com.docker.compose.project=" + projectName)
}

// ListComposeProjects returns the distinct exact Compose project labels whose
// names start with prefix and whose Compose working directory is exactly the
// supplied project directory. The working-directory check prevents a project
// such as "foo" from claiming the historical containers of project "foo-bar".
func (c *Client) ListComposeProjects(prefix, workingDir string) ([]string, error) {
	containers, err := c.ListContainers("label=com.docker.compose.project")
	if err != nil {
		return nil, fmt.Errorf("list Compose projects: %w", err)
	}
	wantedDir := filepath.Clean(workingDir)
	seen := make(map[string]struct{})
	for _, containerName := range containers {
		info, err := c.InspectContainer(containerName)
		if err != nil {
			return nil, fmt.Errorf("inspect Compose project container %s: %w", containerName, err)
		}
		config, _ := info["Config"].(map[string]any)
		labels, _ := config["Labels"].(map[string]any)
		project, _ := labels["com.docker.compose.project"].(string)
		containerWorkingDir, _ := labels["com.docker.compose.project.working_dir"].(string)
		if strings.HasPrefix(project, prefix) && filepath.Clean(containerWorkingDir) == wantedDir {
			seen[project] = struct{}{}
		}
	}
	projects := make([]string, 0, len(seen))
	for project := range seen {
		projects = append(projects, project)
	}
	sort.Strings(projects)
	return projects, nil
}

func (c *Client) VerifyComposeOwnership(containerName, projectName, service string) error {
	info, err := c.InspectContainer(containerName)
	if err != nil {
		return err
	}
	config, _ := info["Config"].(map[string]any)
	labels, _ := config["Labels"].(map[string]any)
	project, _ := labels["com.docker.compose.project"].(string)
	ownedService, _ := labels["com.docker.compose.service"].(string)
	if project != projectName || ownedService != service {
		return fmt.Errorf("container %s is not owned by Compose project %s service %s", containerName, projectName, service)
	}
	return nil
}

func (c *Client) StartContainer(name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", "start", name)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("start container %s: %s: %s", name, err, strings.TrimSpace(string(output)))
	}
	// Ensure restart policy is sticky
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	exec.CommandContext(ctx2, "docker", "update", "--restart", "unless-stopped", name).Run()
	return nil
}

func (c *Client) StopContainer(name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", "stop", name)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("stop container %s: %s: %s", name, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (c *Client) RestartContainer(name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", "restart", name)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("restart container %s: %s: %s", name, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (c *Client) PauseContainer(name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", "pause", name)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("pause container %s: %s: %s", name, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (c *Client) UnpauseContainer(name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", "unpause", name)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("unpause container %s: %s: %s", name, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (c *Client) ContainerAction(action, name string) error {
	switch action {
	case "start":
		return c.StartContainer(name)
	case "stop":
		return c.StopContainer(name)
	case "restart":
		return c.RestartContainer(name)
	case "pause":
		return c.PauseContainer(name)
	case "resume":
		return c.UnpauseContainer(name)
	default:
		return fmt.Errorf("unsupported container action %q", action)
	}
}

func (c *Client) ComposeRecreate(composeFile, projectName, envFile string, service string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cmdArgs := append(c.ComposeCommand(), "-f", composeFile, "-p", projectName)
	if envFile != "" {
		if _, err := os.Stat(envFile); err != nil {
			return fmt.Errorf("env file %s is missing or inaccessible: %w", envFile, err)
		}
		cmdArgs = append(cmdArgs, "--env-file", envFile)
	}
	cmdArgs = append(cmdArgs, "up", "-d", "--force-recreate", service)

	cmd := exec.CommandContext(ctx, cmdArgs[0], cmdArgs[1:]...)
	cmd.Env = dockerCommandEnv(nil)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("compose recreate service %s failed: %w: %s", service, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (c *Client) ValidateComposeConfig(composeFile, projectName, envFile string, allowedRoots []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	args := append(c.ComposeCommand(), "-f", composeFile, "-p", projectName)
	if envFile != "" {
		args = append(args, "--env-file", envFile)
	}
	args = append(args, "config", "--format", "json")
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Env = dockerCommandEnv(nil)
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("render Compose config: %w", err)
	}
	return validateRenderedComposeConfig(output, projectName, allowedRoots)
}

type composeResourceConfig struct {
	Name     string `json:"name"`
	External bool   `json:"external"`
}

func validateRenderedComposeConfig(output []byte, projectName string, allowedRoots []string) error {
	var config struct {
		Services map[string]map[string]any        `json:"services"`
		Volumes  map[string]composeResourceConfig `json:"volumes"`
		Networks map[string]composeResourceConfig `json:"networks"`
	}
	if err := json.Unmarshal(output, &config); err != nil {
		return fmt.Errorf("parse rendered Compose config: %w", err)
	}
	if projectName == "" {
		return fmt.Errorf("Compose policy violation: project name is empty")
	}
	for kind, resources := range map[string]map[string]composeResourceConfig{
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
		}
	}
	cleanRoots := make([]string, 0, len(allowedRoots))
	for _, root := range allowedRoots {
		cleanRoots = append(cleanRoots, resolveExistingPath(root))
	}
	for service, cfg := range config.Services {
		if privileged, _ := cfg["privileged"].(bool); privileged {
			return fmt.Errorf("Compose policy violation: %s uses privileged mode", service)
		}
		for _, field := range []string{"network_mode", "pid", "ipc"} {
			if value, _ := cfg[field].(string); value != "" {
				value = strings.TrimSpace(value)
				if value == "host" {
					return fmt.Errorf("Compose policy violation: %s uses host %s", service, field)
				}
				if strings.HasPrefix(value, "container:") {
					return fmt.Errorf("Compose policy violation: %s uses %s from another container", service, field)
				}
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
		for _, field := range []string{"cap_add", "devices"} {
			if values, ok := cfg[field].([]any); ok && len(values) > 0 {
				return fmt.Errorf("Compose policy violation: %s sets %s", service, field)
			}
		}
		if mounts, ok := cfg["volumes"].([]any); ok {
			for _, raw := range mounts {
				mount, _ := raw.(map[string]any)
				switch mount["type"] {
				case "bind":
					source, _ := mount["source"].(string)
					if !pathWithinAnyRoot(source, cleanRoots) {
						return fmt.Errorf("Compose policy violation: %s bind-mounts %s outside project roots", service, source)
					}
				case "volume":
					source, _ := mount["source"].(string)
					if source != "" {
						if _, ok := config.Volumes[source]; !ok {
							return fmt.Errorf("Compose policy violation: %s uses undeclared named volume %s", service, source)
						}
					}
				}
			}
		}
		if ports, ok := cfg["ports"].([]any); ok {
			for _, raw := range ports {
				port, _ := raw.(map[string]any)
				hostIP, _ := port["host_ip"].(string)
				if hostIP != "127.0.0.1" && hostIP != "::1" {
					return fmt.Errorf("Compose policy violation: %s publishes a port on %q instead of loopback", service, hostIP)
				}
			}
		}
	}
	return nil
}

func pathWithinAnyRoot(path string, roots []string) bool {
	if path == "" {
		return false
	}
	clean := resolveExistingPath(path)
	for _, root := range roots {
		resolvedRoot := resolveExistingPath(root)
		if clean == resolvedRoot || strings.HasPrefix(clean, resolvedRoot+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func resolveExistingPath(path string) string {
	clean := filepath.Clean(path)
	ancestor := clean
	for {
		if resolved, err := filepath.EvalSymlinks(ancestor); err == nil {
			relative, relErr := filepath.Rel(ancestor, clean)
			if relErr == nil {
				return filepath.Clean(filepath.Join(resolved, relative))
			}
			return filepath.Clean(resolved)
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return clean
		}
		ancestor = parent
	}
}

func dockerCommandEnv(overrides map[string]string) []string {
	env := make([]string, 0, len(overrides)+4)
	for _, key := range []string{"PATH", "HOME", "DOCKER_HOST", "DOCKER_CONFIG"} {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	for key, value := range overrides {
		if IsReservedCommandEnvKey(key) {
			continue
		}
		env = append(env, key+"="+value)
	}
	return env
}

// IsReservedCommandEnvKey identifies environment names owned by the controller
// or the Docker CLI. Project configuration must not redirect command lookup,
// Docker access, or controller-managed paths.
func IsReservedCommandEnvKey(key string) bool {
	switch key {
	case "PATH", "HOME", "DOCKER_CONFIG", "DOCKER_HOST", "DOCKER_CONTEXT",
		"BASH_ENV", "ENV", "CDPATH", "IFS", "BASHOPTS", "SHELLOPTS",
		"PROJECT_DIR", "PROJECT_ENV_FILE", "PROJECT_COMPOSE_FILE",
		"PROJECT_STATE_DIR", "PROJECT_RELEASE_DIR", "PROJECT_LOG_DIR",
		"COMPOSE_PROJECT_NAME", "DEPLOY_PROCESS_PID", "DEPLOY_ACTOR":
		return true
	default:
		return false
	}
}

func (c *Client) RegistryLogin(host, username, password string) (bool, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", "login", host, "-u", username, "--password-stdin")
	cmd.Stdin = strings.NewReader(password)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return false, fmt.Sprintf("docker login failed: %s: %s", err, strings.TrimSpace(string(output)))
	}
	return true, "Login Succeeded"
}

func normalizeHostIP(hostIP string) string {
	hostIP = strings.TrimSpace(hostIP)
	if hostIP == "" || hostIP == "0.0.0.0" || hostIP == "::" {
		return "127.0.0.1"
	}
	if strings.Contains(hostIP, ":") && !strings.HasPrefix(hostIP, "[") {
		return fmt.Sprintf("[%s]", hostIP)
	}
	return hostIP
}

// ComposeServiceNames parses a docker-compose.yml and returns the list of service names.
func (c *Client) ComposeServiceNames(composeFile string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	args := append(c.ComposeCommand(), "-f", composeFile, "config", "--services")
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("compose config --services: %w", err)
	}

	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			names = append(names, line)
		}
	}
	return names, nil
}
