package docker

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPathWithinAnyRootResolvesSymlinkAncestors(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if pathWithinAnyRoot(filepath.Join(root, "safe", "file"), []string{root}) != true {
		t.Fatal("safe project path was rejected")
	}
	if pathWithinAnyRoot(filepath.Join(root, "escape", "new-file"), []string{root}) {
		t.Fatal("path through an escaping symlink was accepted")
	}
}

func TestDockerCommandEnvDoesNotAllowOverrideOfControllerEnvironment(t *testing.T) {
	t.Setenv("PATH", "/trusted/bin")
	t.Setenv("HOME", "/trusted/home")
	t.Setenv("DOCKER_HOST", "unix:///trusted/docker.sock")
	t.Setenv("DOCKER_CONFIG", "/trusted/docker-config")
	t.Setenv("DOCKER_CONTEXT", "trusted-context")
	t.Setenv("DOCKER_TLS", "1")
	t.Setenv("DOCKER_TLS_VERIFY", "1")
	t.Setenv("DOCKER_CERT_PATH", "/trusted/docker-certificates")
	t.Setenv("DOCKER_API_VERSION", "1.46")
	got := strings.Join(dockerCommandEnv(map[string]string{
		"PATH":               "/untrusted/bin",
		"HOME":               "/untrusted/home",
		"DOCKER_HOST":        "tcp://untrusted:2375",
		"DOCKER_CONFIG":      "/untrusted/config",
		"DOCKER_CONTEXT":     "untrusted-context",
		"DOCKER_TLS":         "0",
		"DOCKER_TLS_VERIFY":  "0",
		"DOCKER_CERT_PATH":   "/untrusted/certificates",
		"DOCKER_API_VERSION": "1.24",
		"COMPOSE_FILE":       "/untrusted/compose.yml",
		"COMPOSE_ENV_FILES":  "/untrusted/compose.env",
		"COMPOSE_PROFILES":   "untrusted-profile",
		"BUILDKIT_HOST":      "tcp://untrusted:1234",
		"BUILDX_CONFIG":      "/untrusted/buildx",
		"PROJECT_DIR":        "/untrusted/project",
		"APP_SETTING":        "allowed",
		"APP_DOCKER_MODE":    "allowed-app-docker",
		"MY_COMPOSE_VALUE":   "allowed-app-compose",
	}), "\n")
	for _, forbidden := range []string{
		"/untrusted/bin", "/untrusted/home", "tcp://untrusted:2375",
		"/untrusted/config", "untrusted-context", "DOCKER_TLS=0", "DOCKER_TLS_VERIFY=0",
		"/untrusted/certificates", "1.24",
		"/untrusted/compose.yml", "/untrusted/compose.env", "untrusted-profile",
		"tcp://untrusted:1234", "/untrusted/buildx", "/untrusted/project",
	} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("reserved value leaked into Docker command environment: %q", got)
		}
	}
	for _, expected := range []string{
		"PATH=/trusted/bin", "HOME=/trusted/home", "DOCKER_HOST=unix:///trusted/docker.sock",
		"DOCKER_CONFIG=/trusted/docker-config", "DOCKER_CONTEXT=trusted-context", "APP_SETTING=allowed",
		"DOCKER_TLS=1", "DOCKER_TLS_VERIFY=1", "DOCKER_CERT_PATH=/trusted/docker-certificates", "DOCKER_API_VERSION=1.46",
		"APP_DOCKER_MODE=allowed-app-docker", "MY_COMPOSE_VALUE=allowed-app-compose",
	} {
		if !strings.Contains(got, expected) {
			t.Fatalf("Docker command environment missing %q: %q", expected, got)
		}
	}
}

func TestConcurrentRegistryLoginsUseDistinctIsolatedConfigs(t *testing.T) {
	dir := t.TempDir()
	loginLog := filepath.Join(dir, "logins")
	dockerStub := filepath.Join(dir, "docker")
	stub := `#!/bin/sh
set -eu
[ "$1" = login ]
password="$(cat)"
mode="$(stat -c '%a' "$DOCKER_CONFIG" 2>/dev/null || stat -f '%Lp' "$DOCKER_CONFIG")"
printf '%s|%s|%s\n' "$DOCKER_CONFIG" "$password" "$mode" >> ` + strconv.Quote(loginLog) + `
printf '%s\n' '{"auths":{}}' > "$DOCKER_CONFIG/config.json"
`
	if err := os.WriteFile(dockerStub, []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOCKER_HOST", "unix:///explicit-test-docker.sock")

	type result struct {
		auth *RegistryAuth
		ok   bool
		msg  string
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for _, password := range []string{"project-one-password", "project-two-password"} {
		password := password
		wg.Add(1)
		go func() {
			defer wg.Done()
			auth, ok, message := (&Client{}).RegistryLoginIsolated("registry.example", "operator", password)
			results <- result{auth: auth, ok: ok, msg: message}
		}()
	}
	wg.Wait()
	close(results)

	var auths []*RegistryAuth
	for result := range results {
		if !result.ok || result.auth == nil {
			t.Fatalf("isolated registry login failed: %s", result.msg)
		}
		auths = append(auths, result.auth)
	}
	if auths[0].ConfigDir() == auths[1].ConfigDir() {
		t.Fatalf("concurrent registry logins shared %s", auths[0].ConfigDir())
	}
	for _, auth := range auths {
		info, err := os.Stat(auth.ConfigDir())
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("isolated Docker config mode = %o, want 700", info.Mode().Perm())
		}
		if err := auth.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(auth.ConfigDir()); !os.IsNotExist(err) {
			t.Fatalf("isolated Docker config still exists after close: %v", err)
		}
	}

	data, err := os.ReadFile(loginLog)
	if err != nil {
		t.Fatal(err)
	}
	logged := string(data)
	for _, password := range []string{"project-one-password", "project-two-password"} {
		if !strings.Contains(logged, "|"+password+"|700") {
			t.Fatalf("login did not use a private mode-0700 config: %q", logged)
		}
	}
}

func TestRegistryLoginIsolatedImportsActiveContextWithoutGlobalAuth(t *testing.T) {
	root := t.TempDir()
	globalConfigDir := filepath.Join(root, "global-docker")
	if err := os.MkdirAll(globalConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const globalSecret = "global-auth-must-not-be-copied"
	globalConfig := `{"currentContext":"colima","auths":{"registry.example":{"auth":"` + globalSecret + `"}}}`
	if err := os.WriteFile(filepath.Join(globalConfigDir, "config.json"), []byte(globalConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	logPath := filepath.Join(root, "docker-log")
	dockerStub := filepath.Join(root, "docker")
	stub := `#!/bin/sh
set -eu
case "${1:-}:${2:-}" in
  context:export)
    [ "${3:-}" = "--" ]
    [ "${4:-}" = "colima" ]
    [ "${5:-}" = "-" ]
    [ "$DOCKER_CONFIG" = ` + strconv.Quote(globalConfigDir) + ` ]
    printf 'export|%s\n' "$DOCKER_CONFIG" >> ` + strconv.Quote(logPath) + `
    printf 'colima-context-archive'
    ;;
  context:import)
    [ "${3:-}" = "--" ]
    [ "${4:-}" = "colima" ]
    [ "${5:-}" = "-" ]
    [ "$DOCKER_CONFIG" != ` + strconv.Quote(globalConfigDir) + ` ]
    [ ! -e "$DOCKER_CONFIG/config.json" ]
    [ "$(cat)" = "colima-context-archive" ]
    : > "$DOCKER_CONFIG/imported-colima"
    printf 'import|%s\n' "$DOCKER_CONFIG" >> ` + strconv.Quote(logPath) + `
    ;;
  context:use)
    [ "${3:-}" = "--" ]
    [ "${4:-}" = "colima" ]
    [ -f "$DOCKER_CONFIG/imported-colima" ]
    printf '%s\n' '{"currentContext":"colima"}' > "$DOCKER_CONFIG/config.json"
    printf 'use|%s\n' "$DOCKER_CONFIG" >> ` + strconv.Quote(logPath) + `
    ;;
  login:registry.example)
    [ -f "$DOCKER_CONFIG/imported-colima" ]
    grep -q '"currentContext":"colima"' "$DOCKER_CONFIG/config.json"
    password="$(cat)"
    [ "$password" = "project-registry-password" ]
    printf '%s\n' '{"currentContext":"colima","auths":{"registry.example":{"auth":"isolated-auth"}}}' > "$DOCKER_CONFIG/config.json"
    printf 'login|%s\n' "$DOCKER_CONFIG" >> ` + strconv.Quote(logPath) + `
    ;;
  version:)
    [ -f "$DOCKER_CONFIG/imported-colima" ]
    grep -q '"currentContext":"colima"' "$DOCKER_CONFIG/config.json"
    ! grep -q ` + strconv.Quote(globalSecret) + ` "$DOCKER_CONFIG/config.json"
    printf 'version|%s\n' "$DOCKER_CONFIG" >> ` + strconv.Quote(logPath) + `
    ;;
  *)
    exit 97
    ;;
esac
`
	if err := os.WriteFile(dockerStub, []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", root)
	t.Setenv("DOCKER_CONFIG", globalConfigDir)
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")

	auth, ok, message := (&Client{}).RegistryLoginIsolated("registry.example", "operator", "project-registry-password")
	if !ok || auth == nil {
		t.Fatalf("isolated registry login failed: %s", message)
	}
	t.Cleanup(func() { _ = auth.Close() })
	if auth.ConfigDir() == globalConfigDir {
		t.Fatal("registry login reused the global Docker config")
	}

	command := exec.Command("docker", "version")
	command.Env = CommandEnvForConfig(auth.ConfigDir())
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("subsequent Docker command did not use imported context: %v: %s", err, output)
	}

	isolatedConfig, err := os.ReadFile(filepath.Join(auth.ConfigDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(isolatedConfig), globalSecret) {
		t.Fatal("global registry auth was copied into the isolated Docker config")
	}
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(logged)), "\n")
	if len(lines) != 5 {
		t.Fatalf("Docker command sequence = %q", logged)
	}
	isolatedDir := auth.ConfigDir()
	for index, prefix := range []string{"export|" + globalConfigDir, "import|" + isolatedDir, "use|" + isolatedDir, "login|" + isolatedDir, "version|" + isolatedDir} {
		if lines[index] != prefix {
			t.Fatalf("Docker command %d = %q, want %q", index, lines[index], prefix)
		}
	}
}

func TestRegistryLoginIsolatedKeepsExplicitDockerHostAuthoritative(t *testing.T) {
	root := t.TempDir()
	globalConfigDir := filepath.Join(root, "global-docker")
	if err := os.MkdirAll(globalConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalConfigDir, "config.json"), []byte(`{"currentContext":"colima"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(root, "docker-log")
	dockerStub := filepath.Join(root, "docker")
	const explicitHost = "unix:///explicit/docker.sock"
	stub := `#!/bin/sh
set -eu
[ "${DOCKER_HOST:-}" = ` + strconv.Quote(explicitHost) + ` ]
[ -z "${DOCKER_CONTEXT+x}" ]
case "${1:-}" in
  context)
    exit 96
    ;;
  login)
    password="$(cat)"
    [ "$password" = "project-registry-password" ]
    printf '%s\n' '{"auths":{"registry.example":{"auth":"isolated-auth"}}}' > "$DOCKER_CONFIG/config.json"
    printf 'login\n' >> ` + strconv.Quote(logPath) + `
    ;;
  version)
    printf 'version\n' >> ` + strconv.Quote(logPath) + `
    ;;
  *)
    exit 97
    ;;
esac
`
	if err := os.WriteFile(dockerStub, []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", root)
	t.Setenv("DOCKER_CONFIG", globalConfigDir)
	t.Setenv("DOCKER_HOST", explicitHost)
	t.Setenv("DOCKER_CONTEXT", "colima")

	auth, ok, message := (&Client{}).RegistryLoginIsolated("registry.example", "operator", "project-registry-password")
	if !ok || auth == nil {
		t.Fatalf("isolated registry login failed: %s", message)
	}
	t.Cleanup(func() { _ = auth.Close() })

	command := exec.Command("docker", "version")
	command.Env = CommandEnvForConfig(auth.ConfigDir())
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("subsequent Docker command did not preserve DOCKER_HOST: %v: %s", err, output)
	}
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(logged) != "login\nversion\n" {
		t.Fatalf("unexpected Docker commands: %q", logged)
	}
}

func TestRegistryLoginIsolatedRejectsOptionLikeDockerContextName(t *testing.T) {
	root := t.TempDir()
	unexpectedPath := filepath.Join(root, "unexpected-docker-command")
	dockerStub := filepath.Join(root, "docker")
	stub := "#!/bin/sh\n: > " + strconv.Quote(unexpectedPath) + "\nexit 98\n"
	if err := os.WriteFile(dockerStub, []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", root)
	t.Setenv("DOCKER_CONFIG", "")
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "--help")

	auth, ok, message := (&Client{}).RegistryLoginIsolated("registry.example", "operator", "project-registry-password")
	if ok || auth != nil {
		if auth != nil {
			_ = auth.Close()
		}
		t.Fatal("option-like Docker context name was accepted")
	}
	if !strings.Contains(message, "active Docker context name is invalid") {
		t.Fatalf("unexpected failure: %s", message)
	}
	if _, err := os.Stat(unexpectedPath); !os.IsNotExist(err) {
		t.Fatalf("Docker CLI ran with an option-like context name: %v", err)
	}
}

func TestRegistryLoginIsolatedRejectsOversizedContextExport(t *testing.T) {
	root := t.TempDir()
	globalConfigDir := filepath.Join(root, "global-docker")
	if err := os.MkdirAll(globalConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalConfigDir, "config.json"), []byte(`{"currentContext":"colima"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	unexpectedPath := filepath.Join(root, "unexpected-command")
	dockerStub := filepath.Join(root, "docker")
	stub := `#!/bin/sh
set -eu
if [ "${1:-}" = context ] && [ "${2:-}" = export ]; then
  head -c ` + strconv.Itoa(maxDockerContextArchiveSize+1) + ` /dev/zero
  exit 0
fi
: > ` + strconv.Quote(unexpectedPath) + `
exit 98
`
	if err := os.WriteFile(dockerStub, []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", root)
	t.Setenv("DOCKER_CONFIG", globalConfigDir)
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")

	auth, ok, message := (&Client{}).RegistryLoginIsolated("registry.example", "operator", "project-registry-password")
	if ok || auth != nil {
		if auth != nil {
			_ = auth.Close()
		}
		t.Fatal("oversized Docker context export was accepted")
	}
	if !strings.Contains(message, "context export exceeds size limit") {
		t.Fatalf("unexpected failure: %s", message)
	}
	if _, err := os.Stat(unexpectedPath); !os.IsNotExist(err) {
		t.Fatalf("context import or registry login ran after oversized export: %v", err)
	}
}

func TestContainerReadFileRejectsOversizedOutput(t *testing.T) {
	dir := t.TempDir()
	dockerStub := filepath.Join(dir, "docker")
	stub := "#!/bin/sh\nhead -c " + strconv.Itoa(ContainerReadFileLimit+1) + " /dev/zero\n"
	if err := os.WriteFile(dockerStub, []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := (&Client{}).ContainerReadFile("medusa-main-backend", "/app/logs/app.log")
	if !errors.Is(err, ErrContainerFileTooLarge) {
		t.Fatalf("ContainerReadFile error = %v, want size-limit error", err)
	}
}

func TestComposeDownUsesProjectEnvironmentFile(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	composeStub := filepath.Join(dir, "compose-stub")
	stub := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + argsFile + "\"\n"
	if err := os.WriteFile(composeStub, []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	composeFile := filepath.Join(dir, "docker-compose.yml")
	envFile := filepath.Join(dir, ".env.main")
	for _, path := range []string{composeFile, envFile} {
		if err := os.WriteFile(path, []byte("TEST=value\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	client := &Client{composeBinary: composeStub}
	if err := client.ComposeDownWithEnvFile(composeFile, "project-main", envFile, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	args := string(data)
	if !strings.Contains(args, "--env-file\n"+envFile+"\ndown\n--remove-orphans\n") {
		t.Fatalf("compose arguments did not include the environment file before down: %q", args)
	}
}

func TestComposeDownTimeoutCoversStatefulServiceGracePeriod(t *testing.T) {
	// Docker's default graceful-stop period is 10 seconds, and Compose must wait
	// for several stateful services to finish their shutdown before returning.
	// Keep a substantial budget so a successful teardown cannot be killed just
	// before Compose emits its final success status.
	if composeDownTimeout < 2*time.Minute {
		t.Fatalf("compose down timeout = %s, want at least %s", composeDownTimeout, 2*time.Minute)
	}
}

func TestRemoveComposeVolumesOnlyRemovesVolumesWithProjectLabel(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	dockerStub := filepath.Join(dir, "docker")
	stub := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" >> \"" + argsFile + "\"\n" +
		"if [ \"$1\" = volume ] && [ \"$2\" = ls ]; then printf '%s\\n' project-main_postgres_data project-main_redis_data; fi\n"
	if err := os.WriteFile(dockerStub, []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if err := (&Client{}).RemoveComposeVolumes("project-main"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	args := string(data)
	if !strings.Contains(args, "volume\nls\n--filter\nlabel=com.docker.compose.project=project-main\n--format\n{{.Name}}\n") {
		t.Fatalf("volume listing did not use the exact project label: %q", args)
	}
	for _, volume := range []string{"project-main_postgres_data", "project-main_redis_data"} {
		if !strings.Contains(args, "volume\nrm\n"+volume+"\n") {
			t.Fatalf("project volume %s was not removed: %q", volume, args)
		}
	}
}

func TestValidateRenderedComposeConfigAllowsProjectNamedVolumes(t *testing.T) {
	config := []byte(`{
		"networks": {"medusa-net": {"name": "medusa-main_medusa-net"}},
		"volumes": {
			"postgres_data": {"name": "medusa-main_postgres_data"},
			"redis_data": {"name": "medusa-main_redis_data"}
		},
		"services": {
			"postgres": {"volumes": [{"type": "volume", "source": "postgres_data"}]},
			"redis": {"volumes": [{"type": "volume", "source": "redis_data"}]},
			"backup": {"volumes_from": ["postgres:ro"]}
		}
	}`)
	if err := validateRenderedComposeConfig(config, "medusa-main", []string{t.TempDir()}); err != nil {
		t.Fatalf("project-owned named volumes were rejected: %v", err)
	}
}

func TestValidateRenderedComposeConfigRejectsExternalResources(t *testing.T) {
	tests := map[string]string{
		"volume":  `{"volumes":{"data":{"name":"other_data","external":true}},"services":{"app":{"volumes":[{"type":"volume","source":"data"}]}}}`,
		"network": `{"networks":{"shared":{"name":"other_shared","external":true}},"services":{"app":{"networks":{"shared":null}}}}`,
	}
	for name, config := range tests {
		t.Run(name, func(t *testing.T) {
			err := validateRenderedComposeConfig([]byte(config), "project-main", nil)
			if err == nil || !strings.Contains(err.Error(), "is external") {
				t.Fatalf("external %s was not rejected: %v", name, err)
			}
		})
	}
}

func TestValidateRenderedComposeConfigRejectsCrossProjectResourceNames(t *testing.T) {
	tests := map[string]string{
		"volume":  `{"volumes":{"data":{"name":"other-main_data"}},"services":{"app":{"volumes":[{"type":"volume","source":"data"}]}}}`,
		"network": `{"networks":{"shared":{"name":"other-main_shared"}},"services":{"app":{"networks":{"shared":null}}}}`,
	}
	for name, config := range tests {
		t.Run(name, func(t *testing.T) {
			err := validateRenderedComposeConfig([]byte(config), "project-main", nil)
			if err == nil || !strings.Contains(err.Error(), "outside Compose project project-main") {
				t.Fatalf("cross-project %s name was not rejected: %v", name, err)
			}
		})
	}
}

func TestValidateRenderedComposeConfigRejectsContainerNamespaceSharing(t *testing.T) {
	for _, field := range []string{"network_mode", "pid", "ipc"} {
		t.Run(field, func(t *testing.T) {
			config := `{"services":{"app":{"` + field + `":"container:other-project-app"}}}`
			err := validateRenderedComposeConfig([]byte(config), "project-main", nil)
			if err == nil || !strings.Contains(err.Error(), "from another container") {
				t.Fatalf("container-scoped %s was not rejected: %v", field, err)
			}
		})
	}
}

func TestValidateRenderedComposeConfigRejectsContainerVolumesFrom(t *testing.T) {
	config := []byte(`{"services":{"app":{"volumes_from":["container:other-project-app:ro"]}}}`)
	err := validateRenderedComposeConfig(config, "project-main", nil)
	if err == nil || !strings.Contains(err.Error(), "volumes_from from another container") {
		t.Fatalf("volumes_from was not rejected: %v", err)
	}
}
