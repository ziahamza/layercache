package acceptance_test

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
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
		Promotion *struct {
			Path string   `json:"Path"`
			Args []string `json:"Args"`
		} `json:"Promotion"`
		TeamExportID string `json:"TeamExportID"`
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
		"--load", "--file", "Dockerfile", ".",
	}
	for _, argument := range wantArguments {
		if !slices.Contains(plan.Command.Args, argument) {
			t.Fatalf("Buildx arguments do not contain %q: %v", argument, plan.Command.Args)
		}
	}
	if !regexp.MustCompile(`^run-123-u[0-9a-f]{32}$`).MatchString(plan.TeamExportID) {
		t.Fatalf("immutable Team export ID = %q", plan.TeamExportID)
	}
	cacheTo := "type=registry,ref=registry.test/team/acme/widget/linux-amd64:build-" + plan.TeamExportID + ",mode=max,oci-mediatypes=true,image-manifest=true,ignore-error=true"
	if !slices.Contains(plan.Command.Args, cacheTo) {
		t.Fatalf("Buildx arguments do not contain randomized immutable export %q: %v", cacheTo, plan.Command.Args)
	}
	if plan.Promotion == nil || plan.Promotion.Path != "docker" {
		t.Fatalf("Buildx plan promotion = %#v", plan.Promotion)
	}
	promotion := strings.Join(plan.Promotion.Args, "\x00")
	for _, value := range []string{
		"registry.test/team/acme/widget/linux-amd64:main",
		"registry.test/team/acme/widget/linux-amd64:build-" + plan.TeamExportID,
	} {
		if !strings.Contains(promotion, value) {
			t.Fatalf("Buildx promotion does not contain %q: %v", value, plan.Promotion.Args)
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
