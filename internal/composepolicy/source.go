// Package composepolicy validates repository-controlled Compose input before
// Docker Compose is allowed to parse it with controller privileges.
package composepolicy

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const maxComposeSourceSize = 2 << 20

const (
	maxComposeNodes = 100_000
	maxComposeDepth = 128
)

// Keys in this set can make Compose read another host file, invoke a build, or
// delegate lifecycle control before the rendered-model policy can inspect the
// result. Image-only deployments do not need any of them.
var forbiddenSourceKeys = map[string]string{
	"build":           "image-only deployments cannot build images",
	"configs":         "Compose configs can import host or external data",
	"credential_spec": "credential specs can read host files",
	"develop":         "development watches can read host paths",
	"env_file":        "environment files can read host data before validation",
	"extends":         "extends can import another Compose file",
	"include":         "include can import another Compose file",
	"label_file":      "label files can read host data before validation",
	"provider":        "provider delegates lifecycle control outside Compose",
	"secrets":         "Compose secrets can import host or external data",
}

// These service keys grant host namespaces, devices, engine access, alternate
// runtimes, or lifecycle privileges. The deployment controller deliberately
// supports image-only application services rather than the entire Compose
// orchestration surface.
var forbiddenServiceKeys = map[string]string{
	"cap_add":             "additional Linux capabilities are forbidden",
	"cgroup":              "host cgroup namespace access is forbidden",
	"cgroup_parent":       "host cgroup placement is forbidden",
	"deploy":              "deploy device reservations and daemon policy are forbidden",
	"device_cgroup_rules": "device cgroup access is forbidden",
	"devices":             "host device access is forbidden",
	"external_links":      "cross-project container links are forbidden",
	"gpus":                "host GPU access is forbidden",
	"ipc":                 "IPC namespace sharing is forbidden",
	"isolation":           "alternate container isolation is forbidden",
	"network_mode":        "network namespace sharing is forbidden",
	"pid":                 "PID namespace sharing is forbidden",
	"post_start":          "privileged lifecycle hooks are forbidden",
	"pre_stop":            "privileged lifecycle hooks are forbidden",
	"privileged":          "privileged containers are forbidden",
	"runtime":             "alternate container runtimes are forbidden",
	"sysctls":             "container sysctl changes are forbidden",
	"use_api_socket":      "container engine API socket access is forbidden",
	"userns_mode":         "host user namespace access is forbidden",
	"uts":                 "host UTS namespace access is forbidden",
}

// ValidateSource parses one Compose YAML document without invoking Docker
// Compose. composePath itself must resolve inside projectRoot. The validator is
// deliberately conservative because the file comes from a project image while
// Compose runs against the host Docker daemon.
func ValidateSource(composePath, projectRoot string) error {
	resolvedRoot, err := filepath.EvalSymlinks(projectRoot)
	if err != nil {
		return fmt.Errorf("resolve project root: %w", err)
	}
	resolvedCompose, err := filepath.EvalSymlinks(composePath)
	if err != nil {
		return fmt.Errorf("resolve Compose file: %w", err)
	}
	relative, err := filepath.Rel(resolvedRoot, resolvedCompose)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return fmt.Errorf("Compose policy violation: Compose file resolves outside the project root")
	}

	file, err := os.Open(resolvedCompose)
	if err != nil {
		return fmt.Errorf("open Compose file: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxComposeSourceSize+1))
	if err != nil {
		return fmt.Errorf("read Compose file: %w", err)
	}
	if len(data) > maxComposeSourceSize {
		return fmt.Errorf("Compose policy violation: Compose source exceeds %d bytes", maxComposeSourceSize)
	}

	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return fmt.Errorf("parse Compose source: %w", err)
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("Compose policy violation: Compose source must contain one mapping document")
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return fmt.Errorf("parse additional Compose document: %w", err)
		}
		return fmt.Errorf("Compose policy violation: multiple YAML documents are forbidden")
	}

	budget := &sourceTraversalBudget{}
	if err := rejectForbiddenKeys(document.Content[0], nil, budget, 0); err != nil {
		return err
	}
	if err := validateServices(document.Content[0]); err != nil {
		return err
	}
	return validateResourceDrivers(document.Content[0])
}

type sourceTraversalBudget struct {
	nodes int
}

func rejectForbiddenKeys(node *yaml.Node, path []string, budget *sourceTraversalBudget, depth int) error {
	if node == nil {
		return fmt.Errorf("Compose policy violation: invalid empty YAML node")
	}
	budget.nodes++
	if budget.nodes > maxComposeNodes {
		return fmt.Errorf("Compose policy violation: Compose source exceeds %d YAML nodes", maxComposeNodes)
	}
	if depth > maxComposeDepth {
		return fmt.Errorf("Compose policy violation: Compose source exceeds YAML depth %d", maxComposeDepth)
	}

	switch node.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, child := range node.Content {
			if err := rejectForbiddenKeys(child, path, budget, depth+1); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		if len(node.Content)%2 != 0 {
			return fmt.Errorf("Compose policy violation: invalid mapping")
		}
		seen := make(map[string]struct{}, len(node.Content)/2)
		for i := 0; i < len(node.Content); i += 2 {
			keyNode, valueNode := node.Content[i], node.Content[i+1]
			if keyNode.Kind != yaml.ScalarNode {
				return fmt.Errorf("Compose policy violation: mapping keys must be strings")
			}
			key := keyNode.Value
			if key == "<<" || keyNode.Tag == "!!merge" {
				return fmt.Errorf("Compose policy violation: YAML merge keys are forbidden")
			}
			if keyNode.Tag != "!!str" {
				return fmt.Errorf("Compose policy violation: mapping keys must be strings")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("Compose policy violation: duplicate key %q", strings.Join(append(path, key), "."))
			}
			seen[key] = struct{}{}
			if reason, forbidden := forbiddenSourceKeys[key]; forbidden {
				return fmt.Errorf("Compose policy violation: %s (%s)", strings.Join(append(path, key), "."), reason)
			}
			if err := rejectForbiddenKeys(valueNode, append(path, key), budget, depth+1); err != nil {
				return err
			}
		}
	case yaml.AliasNode:
		return fmt.Errorf("Compose policy violation: YAML aliases are forbidden")
	}
	return nil
}

func validateServices(root *yaml.Node) error {
	services := mappingValue(root, "services")
	if services == nil {
		return fmt.Errorf("Compose policy violation: services must be declared")
	}
	if services.Kind != yaml.MappingNode {
		return fmt.Errorf("Compose policy violation: services must be a mapping")
	}
	for i := 0; i < len(services.Content); i += 2 {
		nameNode, service := services.Content[i], services.Content[i+1]
		if nameNode.Kind != yaml.ScalarNode || nameNode.Tag != "!!str" {
			return fmt.Errorf("Compose policy violation: service names must be strings")
		}
		if service.Kind != yaml.MappingNode {
			return fmt.Errorf("Compose policy violation: services.%s must be a mapping", nameNode.Value)
		}
		for j := 0; j < len(service.Content); j += 2 {
			keyNode, valueNode := service.Content[j], service.Content[j+1]
			if reason, forbidden := forbiddenServiceKeys[keyNode.Value]; forbidden {
				return fmt.Errorf("Compose policy violation: services.%s.%s (%s)", nameNode.Value, keyNode.Value, reason)
			}
			if keyNode.Value == "security_opt" {
				if err := validateSecurityOptSource(nameNode.Value, valueNode); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validateSecurityOptSource(service string, node *yaml.Node) error {
	if node.Kind != yaml.SequenceNode || len(node.Content) != 1 {
		return fmt.Errorf("Compose policy violation: services.%s.security_opt may contain only no-new-privileges:true", service)
	}
	value := node.Content[0]
	if value.Kind != yaml.ScalarNode || value.Tag != "!!str" || strings.TrimSpace(value.Value) != "no-new-privileges:true" {
		return fmt.Errorf("Compose policy violation: services.%s.security_opt may contain only no-new-privileges:true", service)
	}
	return nil
}

func validateResourceDrivers(root *yaml.Node) error {
	for _, sectionName := range []string{"volumes", "networks"} {
		section := mappingValue(root, sectionName)
		if section == nil {
			continue
		}
		if section.Kind != yaml.MappingNode {
			return fmt.Errorf("Compose policy violation: %s must be a mapping", sectionName)
		}
		for i := 0; i < len(section.Content); i += 2 {
			nameNode, resource := section.Content[i], section.Content[i+1]
			if resource.Kind == yaml.ScalarNode && resource.Tag == "!!null" {
				continue
			}
			if resource.Kind != yaml.MappingNode {
				return fmt.Errorf("Compose policy violation: %s.%s must be a mapping", sectionName, nameNode.Value)
			}
			if value := mappingValue(resource, "driver"); value != nil && !isEmptyScalar(value) {
				driver := strings.TrimSpace(value.Value)
				safeDriver := (sectionName == "volumes" && driver == "local") ||
					(sectionName == "networks" && driver == "bridge")
				if !safeDriver {
					return fmt.Errorf("Compose policy violation: %s.%s.driver %q is forbidden", sectionName, nameNode.Value, driver)
				}
			}
			if value := mappingValue(resource, "driver_opts"); value != nil && !isEmptyMapping(value) {
				return fmt.Errorf("Compose policy violation: %s.%s.driver_opts is forbidden", sectionName, nameNode.Value)
			}
		}
	}
	return nil
}

func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

func isEmptyScalar(node *yaml.Node) bool {
	return node.Kind == yaml.ScalarNode && (node.Tag == "!!null" || strings.TrimSpace(node.Value) == "")
}

func isEmptyMapping(node *yaml.Node) bool {
	return (node.Kind == yaml.ScalarNode && node.Tag == "!!null") ||
		(node.Kind == yaml.MappingNode && len(node.Content) == 0)
}
