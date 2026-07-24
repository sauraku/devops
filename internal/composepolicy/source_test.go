package composepolicy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCompose(t *testing.T, root, content string) string {
	t.Helper()
	path := filepath.Join(root, "docker-compose.yml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestValidateSourceAcceptsImageOnlyCompose(t *testing.T) {
	root := t.TempDir()
	path := writeCompose(t, root, `
services:
  backend:
    image: ghcr.io/sauraku/medusa-backend:main
    environment:
      SMTP_HOST: ${SMTP_HOST}
    volumes:
      - postgres_data:/data
volumes:
  postgres_data:
`)
	if err := ValidateSource(path, root); err != nil {
		t.Fatal(err)
	}
}

func TestValidateSourceAcceptsMedusaProductionFeatures(t *testing.T) {
	root := t.TempDir()
	path := writeCompose(t, root, `
services:
  opensearch-init:
    image: opensearchproject/opensearch:2.19.6
    user: "0:0"
    read_only: true
    security_opt:
      - no-new-privileges:true
    tmpfs:
      - /tmp:mode=700
    volumes:
      - opensearch_config:/usr/share/opensearch/config
  backend:
    image: ghcr.io/sauraku/medusa-backend:${IMAGE_TAG:-latest}
    depends_on:
      opensearch-init:
        condition: service_completed_successfully
    ports:
      - "127.0.0.1:${BACKEND_PORT:-9001}:9000"
    volumes:
      - opensearch_config:/run/opensearch-config:ro
    command: ["npx", "medusa", "start"]
    networks:
      - medusa-net
networks:
  medusa-net:
    driver: bridge
volumes:
  opensearch_config:
    driver: local
`)
	if err := ValidateSource(path, root); err != nil {
		t.Fatalf("Medusa production features were rejected: %v", err)
	}
}

func TestValidateSourceRejectsPreRenderFileReadsAndBuilds(t *testing.T) {
	tests := map[string]string{
		"env file":      "services:\n  app:\n    image: example/app\n    env_file: /opt/devops-control/.env.prod\n",
		"include":       "include:\n  - /opt/devops-control/compose.yml\nservices: {}\n",
		"extends":       "services:\n  app:\n    extends:\n      file: /opt/devops-control/compose.yml\n      service: app\n",
		"build":         "services:\n  app:\n    build: /opt/devops-control\n",
		"secret":        "secrets:\n  controller:\n    file: /opt/devops-control/.env.prod\nservices: {}\n",
		"config":        "configs:\n  controller:\n    file: /opt/devops-control/.env.prod\nservices: {}\n",
		"label file":    "services:\n  app:\n    image: example/app\n    label_file: /opt/devops-control/labels\n",
		"volume driver": "services:\n  app:\n    image: example/app\nvolumes:\n  data:\n    driver_opts:\n      type: none\n      device: /opt/devops-control\n      o: bind\n",
	}
	for name, source := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			err := ValidateSource(writeCompose(t, root, source), root)
			if err == nil || !strings.Contains(err.Error(), "Compose policy violation") {
				t.Fatalf("source was not rejected: %v", err)
			}
		})
	}
}

func TestValidateSourceRejectsHostPrivilegeDirectives(t *testing.T) {
	tests := map[string]string{
		"device cgroup":  "device_cgroup_rules:\n      - 'b *:* rwm'",
		"lifecycle hook": "post_start:\n      - command: id\n        privileged: true",
		"API socket":     "use_api_socket: true",
		"deploy devices": "deploy:\n      resources:\n        reservations:\n          devices:\n            - capabilities: [gpu]",
		"user namespace": "userns_mode: host",
		"UTS namespace":  "uts: host",
		"cgroup":         "cgroup: host",
		"runtime":        "runtime: runc",
		"GPU":            "gpus: all",
		"external link":  "external_links:\n      - other-project-db",
	}
	for name, directive := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			source := "services:\n  app:\n    image: example/app\n    " + directive + "\n"
			if err := ValidateSource(writeCompose(t, root, source), root); err == nil {
				t.Fatalf("unsafe directive was accepted:\n%s", source)
			}
		})
	}
}

func TestValidateSourceAllowsOnlyNoNewPrivilegesSecurityOpt(t *testing.T) {
	root := t.TempDir()
	path := writeCompose(t, root, "services:\n  app:\n    image: example/app\n    security_opt:\n      - seccomp=unconfined\n")
	if err := ValidateSource(path, root); err == nil {
		t.Fatal("unsafe security_opt was accepted")
	}
}

func TestValidateSourceRejectsSymlinkOutsideProjectAndDuplicateKeys(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	outsideCompose := writeCompose(t, outside, "services: {}\n")
	link := filepath.Join(root, "docker-compose.yml")
	if err := os.Symlink(outsideCompose, link); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSource(link, root); err == nil {
		t.Fatal("Compose symlink outside project root was accepted")
	}

	duplicateRoot := t.TempDir()
	duplicate := writeCompose(t, duplicateRoot, "services: {}\nservices: {}\n")
	if err := ValidateSource(duplicate, duplicateRoot); err == nil || !strings.Contains(err.Error(), "duplicate key") {
		t.Fatalf("duplicate key was not rejected: %v", err)
	}
}

func TestValidateSourceRejectsAliasesMergeKeysAndExcessiveDepth(t *testing.T) {
	tests := map[string]string{
		"alias": "x-service: &service\n  image: example/app\nservices:\n  app: *service\n",
		"merge": "x-service: &service\n  image: example/app\nservices:\n  app:\n    <<: *service\n",
		"depth": "services:\n  app:\n    image: example/app\n    environment:\n      VALUE: " +
			strings.Repeat("[", maxComposeDepth+2) + "value" + strings.Repeat("]", maxComposeDepth+2) + "\n",
	}
	for name, source := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if err := ValidateSource(writeCompose(t, root, source), root); err == nil {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
}
