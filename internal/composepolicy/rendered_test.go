package composepolicy

import (
	"strings"
	"testing"
)

func TestValidateRenderedAcceptsMedusaProductionFeatures(t *testing.T) {
	config := []byte(`{
		"networks":{"medusa-net":{"name":"medusa-main_medusa-net","driver":"bridge"}},
		"volumes":{"opensearch_config":{"name":"medusa-main_opensearch_config","driver":"local"}},
		"services":{
			"opensearch-init":{"security_opt":["no-new-privileges:true"],"volumes":[{"type":"volume","source":"opensearch_config"}]},
			"backend":{"ports":[{"host_ip":"127.0.0.1"}],"volumes":[{"type":"volume","source":"opensearch_config"}]}
		}
	}`)
	if err := ValidateRendered(config, "medusa-main"); err != nil {
		t.Fatalf("Medusa production model was rejected: %v", err)
	}
}

func TestValidateRenderedRejectsHostPrivilegeDirectivesAndBindMounts(t *testing.T) {
	tests := []string{
		`"device_cgroup_rules":["b *:* rwm"]`,
		`"post_start":[{"command":"id","privileged":true}]`,
		`"use_api_socket":true`,
		`"deploy":{"resources":{"reservations":{"devices":[{"capabilities":["gpu"]}]}}}`,
		`"security_opt":["seccomp=unconfined"]`,
		`"volumes":[{"type":"bind","source":"/opt/devops-control"}]`,
	}
	for _, field := range tests {
		config := []byte(`{"services":{"app":{` + field + `}}}`)
		if err := ValidateRendered(config, "project-main"); err == nil {
			t.Fatalf("unsafe rendered field was accepted: %s", field)
		}
	}
}

func TestValidateRenderedRejectsUnsafeResourceDrivers(t *testing.T) {
	for _, resource := range []string{
		`"driver":"local","driver_opts":{"device":"/"}`,
		`"driver":"evil-plugin"`,
	} {
		config := []byte(`{"volumes":{"data":{"name":"project-main_data",` + resource + `}},"services":{"app":{"volumes":[{"type":"volume","source":"data"}]}}}`)
		err := ValidateRendered(config, "project-main")
		if err == nil || !strings.Contains(err.Error(), "driver") {
			t.Fatalf("unsafe resource driver was accepted: %v", err)
		}
	}
}
