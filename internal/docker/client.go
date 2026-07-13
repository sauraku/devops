package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/sauraku/devops-control/internal/models"
)

type Client struct {
	composeBinary string
}

func NewClient() *Client {
	c := &Client{}
	c.detectComposeBinary()
	return c
}

func (c *Client) detectComposeBinary() {
	if err := exec.Command("docker", "compose", "version").Run(); err == nil {
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

	cmd := exec.CommandContext(ctx, "docker", "exec", containerName, "cat", filePath)
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("read file %s from container %s: %w", filePath, containerName, err)
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
	cmd.Env = os.Environ()
	for k, v := range envVars {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("compose up failed: %s: %s", err, strings.TrimSpace(string(output)))
	}

	// Enforce restart: unless-stopped on started containers so they survive host reboots
	for _, svc := range services {
		name := fmt.Sprintf("%s-%s-1", projectName, svc)
		ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
		exec.CommandContext(ctx2, "docker", "update", "--restart", "unless-stopped", name).Run()
		cancel2()
	}
	return nil
}

func (c *Client) ComposeDown(composeFile, projectName string, envVars map[string]string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmdArgs := append(c.ComposeCommand(), "-f", composeFile, "-p", projectName, "down", "--remove-orphans")
	cmd := exec.CommandContext(ctx, cmdArgs[0], cmdArgs[1:]...)
	cmd.Env = os.Environ()
	for k, v := range envVars {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}
	return cmd.Run()
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

func (c *Client) ComposeRecreate(composeFile, projectName, envFile string, service string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cmdArgs := append(c.ComposeCommand(), "-f", composeFile, "-p", projectName)
	if envFile != "" {
		if _, err := os.Stat(envFile); err == nil {
			cmdArgs = append(cmdArgs, "--env-file", envFile)
		}
	}
	cmdArgs = append(cmdArgs, "up", "-d", "--force-recreate", service)

	cmd := exec.CommandContext(ctx, cmdArgs[0], cmdArgs[1:]...)
	cmd.Env = os.Environ()
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("compose recreate service %s failed: %w: %s", service, err, strings.TrimSpace(string(output)))
	}
	return nil
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


