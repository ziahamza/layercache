package acceptance_test

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"
)

func TestBuildxPlanUsesPersistentBuilderAndNativeRegistryCaches(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.json")
	runLayerCache(t,
		"setup", "--config", configPath,
		"--data-dir", filepath.Join(root, "cache"),
		"--buildkit-builder", "layercache-fixture",
		"--buildkit-team-repository", "registry.test/team/acme/widget",
		"--non-interactive", "--json",
	)
	output := runLayerCache(t,
		"buildx", "plan", "--config", configPath,
		"--platform", "linux/amd64",
		"--team-import", "main",
		"--team-export", "run-123",
		"--load",
		"--", "--file", "Dockerfile", ".",
	)
	var plan struct {
		Command struct {
			Path string   `json:"Path"`
			Args []string `json:"Args"`
		} `json:"Command"`
	}
	if err := json.Unmarshal(output, &plan); err != nil {
		t.Fatalf("decode Buildx plan: %v\n%s", err, output)
	}
	if plan.Command.Path != "docker" {
		t.Fatalf("command path = %q, want docker", plan.Command.Path)
	}
	wantArguments := []string{
		"--builder", "layercache-fixture",
		"--platform", "linux/amd64",
		"--cache-from", "type=registry,ref=registry.test/team/acme/widget/linux-amd64:main",
		"--cache-to", "type=registry,ref=registry.test/team/acme/widget/linux-amd64:build-run-123,mode=max,oci-mediatypes=true,image-manifest=true,ignore-error=true",
		"--load", "--file", "Dockerfile", ".",
	}
	for _, argument := range wantArguments {
		if !slices.Contains(plan.Command.Args, argument) {
			t.Fatalf("Buildx arguments do not contain %q: %v", argument, plan.Command.Args)
		}
	}
}

func TestBuildxBypassForcesLocalRecomputation(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.json")
	runLayerCache(t,
		"setup", "--config", configPath,
		"--data-dir", filepath.Join(root, "cache"),
		"--buildkit-builder", "layercache-bypass-fixture",
		"--buildkit-team-repository", "registry.test/team/acme/widget",
		"--non-interactive", "--json",
	)
	runLayerCache(t, "bypass", "--config", configPath, "--adapter", "buildkit", "--json")
	output := runLayerCache(t,
		"buildx", "plan", "--config", configPath,
		"--platform", "linux/amd64",
		"--team-import", "main",
		"--team-export", "run-123",
		"--load",
		"--", "--file", "Dockerfile", ".",
	)
	var plan struct {
		Command struct {
			Args []string `json:"Args"`
		} `json:"Command"`
	}
	jsonStart := bytes.IndexByte(output, '{')
	if jsonStart < 0 {
		t.Fatalf("bypassed Buildx plan did not contain JSON: %s", output)
	}
	if err := json.Unmarshal(output[jsonStart:], &plan); err != nil {
		t.Fatalf("decode bypassed Buildx plan: %v\n%s", err, output)
	}
	if !slices.Contains(plan.Command.Args, "--no-cache") {
		t.Fatalf("bypassed Buildx arguments do not force recomputation: %v", plan.Command.Args)
	}
	for _, argument := range plan.Command.Args {
		if argument == "--cache-from" || argument == "--cache-to" {
			t.Fatalf("bypassed Buildx arguments still contain %q: %v", argument, plan.Command.Args)
		}
	}
}
