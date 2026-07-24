package services

import (
	"strings"
	"testing"
)

func environmentMap(t *testing.T, entries []string) map[string]string {
	t.Helper()
	values := make(map[string]string, len(entries))
	for _, entry := range entries {
		parts := strings.SplitN(entry, "=", 2)
		if len(parts) != 2 {
			t.Fatalf("invalid environment entry %q", entry)
		}
		values[parts[0]] = parts[1]
	}
	return values
}

func TestOperationProcessEnvironmentsPreserveTrustedDockerSettings(t *testing.T) {
	t.Setenv("PATH", "/controller/bin")
	t.Setenv("HOME", "/controller/home")
	t.Setenv("DOCKER_CONFIG", "/controller/docker-config")
	t.Setenv("DOCKER_HOST", "tcp://controller.example:2376")
	t.Setenv("DOCKER_CONTEXT", "controller-context")
	t.Setenv("DOCKER_TLS", "1")
	t.Setenv("DOCKER_TLS_VERIFY", "1")
	t.Setenv("DOCKER_CERT_PATH", "/controller/docker-certs")
	t.Setenv("DOCKER_API_VERSION", "1.46")

	tests := []struct {
		name        string
		build       func(map[string]string) []string
		explicit    map[string]string
		want        map[string]string
		mustNotHave []string
	}{
		{
			name:  "deploy",
			build: deploymentProcessEnv,
			explicit: map[string]string{
				"DEPLOY_ID":                     "deploy-1",
				"DOCKER_CONFIG":                 "/operation/docker-config",
				"AUTHENTICATED_GHCR_REPOSITORY": "ghcr.io/sauraku/medusa",
				"DOCKER_HOST":                   "tcp://project.example:2375",
				"COMPOSE_FILE":                  "/project/compose.yml",
				"BUILDKIT_HOST":                 "tcp://project.example:1234",
				"LD_PRELOAD":                    "/project/evil.so",
				"PROJECT_ENV_FILE":              "/project/host.env",
				projectEnvOverridesFDEnv:        "99",
			},
			want: map[string]string{
				"DEPLOY_ID":                     "deploy-1",
				"DOCKER_CONFIG":                 "/operation/docker-config",
				"AUTHENTICATED_GHCR_REPOSITORY": "ghcr.io/sauraku/medusa",
			},
			mustNotHave: []string{
				"COMPOSE_FILE", "BUILDKIT_HOST", "LD_PRELOAD",
				"PROJECT_ENV_FILE", projectEnvOverridesFDEnv,
			},
		},
		{
			name:  "backup",
			build: backupProcessEnv,
			explicit: map[string]string{
				"DEPLOY_ID":            "backup-1",
				"COMPOSE_PROJECT_NAME": "medusa-main",
				"DOCKER_HOST":          "tcp://project.example:2375",
				"COMPOSE_FILE":         "/project/compose.yml",
				"BUILDX_CONFIG":        "/project/buildx",
				"BASH_ENV":             "/project/bash-env",
			},
			want: map[string]string{
				"DEPLOY_ID":            "backup-1",
				"COMPOSE_PROJECT_NAME": "medusa-main",
				"DOCKER_CONFIG":        "/controller/docker-config",
			},
			mustNotHave: []string{"COMPOSE_FILE", "BUILDX_CONFIG", "BASH_ENV"},
		},
		{
			name:  "restore",
			build: restoreProcessEnv,
			explicit: map[string]string{
				"DEPLOY_ID":            "restore-1",
				"COMPOSE_PROJECT_NAME": "medusa-main",
				"COMPOSE_FILE":         "/controller/compose.yml",
				"DOCKER_CONTEXT":       "project-context",
				"COMPOSE_PROFILES":     "debug",
				"BUILDKIT_HOST":        "tcp://project.example:1234",
			},
			want: map[string]string{
				"DEPLOY_ID":            "restore-1",
				"COMPOSE_PROJECT_NAME": "medusa-main",
				"COMPOSE_FILE":         "/controller/compose.yml",
				"DOCKER_CONFIG":        "/controller/docker-config",
			},
			mustNotHave: []string{"COMPOSE_PROFILES", "BUILDKIT_HOST"},
		},
		{
			name:  "rollback",
			build: rollbackProcessEnv,
			explicit: map[string]string{
				"DEPLOY_ID":        "rollback-1",
				"DOCKER_CONFIG":    "/operation/docker-config",
				"DOCKER_CERT_PATH": "/project/docker-certs",
				"COMPOSE_FILE":     "/project/compose.yml",
				"BUILDX_CONFIG":    "/project/buildx",
			},
			want: map[string]string{
				"DEPLOY_ID":     "rollback-1",
				"DOCKER_CONFIG": "/operation/docker-config",
			},
			mustNotHave: []string{"COMPOSE_FILE", "BUILDX_CONFIG"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			values := environmentMap(t, tc.build(tc.explicit))
			for key, want := range map[string]string{
				"PATH":               "/controller/bin",
				"HOME":               "/controller/home",
				"DOCKER_HOST":        "tcp://controller.example:2376",
				"DOCKER_TLS":         "1",
				"DOCKER_TLS_VERIFY":  "1",
				"DOCKER_CERT_PATH":   "/controller/docker-certs",
				"DOCKER_API_VERSION": "1.46",
			} {
				if got := values[key]; got != want {
					t.Fatalf("%s = %q, want trusted controller value %q", key, got, want)
				}
			}
			if _, found := values["DOCKER_CONTEXT"]; found {
				t.Fatalf("DOCKER_CONTEXT overrode the explicit controller DOCKER_HOST in %s environment", tc.name)
			}
			for key, want := range tc.want {
				if got := values[key]; got != want {
					t.Fatalf("%s = %q, want %q", key, got, want)
				}
			}
			for _, key := range tc.mustNotHave {
				if _, found := values[key]; found {
					t.Fatalf("untrusted command-control key %s reached %s environment", key, tc.name)
				}
			}
		})
	}

	t.Run("context is preserved without explicit host", func(t *testing.T) {
		t.Setenv("DOCKER_HOST", "")
		t.Setenv("DOCKER_CONTEXT", "controller-context")
		values := environmentMap(t, deploymentProcessEnv(nil))
		if got := values["DOCKER_CONTEXT"]; got != "controller-context" {
			t.Fatalf("DOCKER_CONTEXT = %q, want controller-context", got)
		}
	})
}
