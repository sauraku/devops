package docker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
