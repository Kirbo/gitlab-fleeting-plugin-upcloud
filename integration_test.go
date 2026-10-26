//go:build integration

package main

import (
	"os"
	"testing"
	"time"

	"gitlab.com/gitlab-org/fleeting/fleeting/integration"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

// TestIntegration runs fleeting's provider conformance test against real
// UpCloud servers: it scales a fresh group up to 3 servers, runs a command on
// one over SSH, and deletes them all. The servers are billed while it runs.
//
//	UPCLOUD_TOKEN=ucat_... UPCLOUD_ZONE=fi-hel1 UPCLOUD_TEMPLATE=<template UUID> \
//	UPCLOUD_SSH_KEY=~/.ssh/fleeting-upcloud mise run test-integration
//
// UPCLOUD_PLAN (default 1xCPU-2GB) and UPCLOUD_SSH_USER (default root) are optional.
func TestIntegration(t *testing.T) {
	env := map[string]string{}
	for _, name := range []string{"UPCLOUD_TOKEN", "UPCLOUD_ZONE", "UPCLOUD_TEMPLATE", "UPCLOUD_SSH_KEY"} {
		if env[name] = os.Getenv(name); env[name] == "" {
			t.Skipf("%s not set; skipping integration test against real UpCloud servers", name)
		}
	}

	key, err := os.ReadFile(env["UPCLOUD_SSH_KEY"])
	if err != nil {
		t.Fatalf("reading UPCLOUD_SSH_KEY: %v", err)
	}

	username := os.Getenv("UPCLOUD_SSH_USER")
	if username == "" {
		username = "root"
	}

	pluginConfig := map[string]any{
		"token":    env["UPCLOUD_TOKEN"],
		"zone":     env["UPCLOUD_ZONE"],
		"template": env["UPCLOUD_TEMPLATE"],
		// A unique group, so the test never touches a real runner's servers.
		"name":        "fleeting-integration-" + randomSuffix(6),
		"name_prefix": "fleeting-it",
	}
	if plan := os.Getenv("UPCLOUD_PLAN"); plan != "" {
		pluginConfig["plan"] = plan
	}

	plugin := integration.BuildPluginBinary(t, ".", "fleeting-plugin-upcloud")
	integration.TestProvisioning(t, plugin, integration.Config{
		PluginConfig: pluginConfig,
		ConnectorConfig: provider.ConnectorConfig{
			OS:       "linux",
			Arch:     "amd64",
			Protocol: provider.ProtocolSSH,
			Username: username,
			Key:      key,
			Timeout:  10 * time.Minute,
		},
		MaxInstances:    3,
		UseExternalAddr: true,
	})
}
