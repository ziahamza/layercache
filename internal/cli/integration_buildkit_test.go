package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/layercache/layercache/internal/config"
)

func TestInspectOwnedBuildkitConfigurationReportsFileDrift(t *testing.T) {
	t.Parallel()

	configPath, _ := setupLocalConfig(t)
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	path, err := resolveThroughExistingAncestor(filepath.Join(cfg.DataDir, "buildkit", "buildkitd.toml"))
	if err != nil {
		t.Fatal(err)
	}
	desired, _, err := buildkitGCConfiguration(cfg, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureRealDirectory(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writePrivateFile(path, desired); err != nil {
		t.Fatal(err)
	}
	record := integrationRecord{
		State: "active", Path: path, Digest: contentDigest(desired), OwnershipCaptured: true,
		Builder: cfg.BuildkitBuilder, BuilderNode: buildkitNodeName(cfg),
	}
	if got := inspectOwnedBuildkitConfiguration(cfg, record); got != "current" {
		t.Fatalf("configuration state = %q, want current", got)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got := inspectOwnedBuildkitConfiguration(cfg, record); got != "configuration-missing" {
		t.Fatalf("missing configuration state = %q", got)
	}
	if err := writePrivateFile(path, []byte("# user edit\n")); err != nil {
		t.Fatal(err)
	}
	if got := inspectOwnedBuildkitConfiguration(cfg, record); got != "configuration-drifted" {
		t.Fatalf("edited configuration state = %q", got)
	}

	if err := writePrivateFile(path, desired); err != nil {
		t.Fatal(err)
	}
	stalePolicy := cfg
	stalePolicy.BuildkitGCBytes--
	if got := inspectOwnedBuildkitConfiguration(stalePolicy, record); got != "configuration-policy-stale" {
		t.Fatalf("stale policy state = %q", got)
	}
	record.Path = filepath.Join(cfg.DataDir, "other-buildkitd.toml")
	if got := inspectOwnedBuildkitConfiguration(cfg, record); got != "configuration-path-drifted" {
		t.Fatalf("path drift state = %q", got)
	}
}

func TestBuildkitApplyRepairsMissingOwnedConfigurationByRecreatingBuilder(t *testing.T) {
	configPath, _ := setupLocalConfig(t)
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	path, err := resolveThroughExistingAncestor(filepath.Join(cfg.DataDir, "buildkit", "buildkitd.toml"))
	if err != nil {
		t.Fatal(err)
	}
	desired, _, err := buildkitGCConfiguration(cfg, path)
	if err != nil {
		t.Fatal(err)
	}
	nodeName := buildkitNodeName(cfg)
	record := integrationRecord{
		Name: "buildkit", State: "active", Path: path, Digest: contentDigest(desired),
		OwnershipCaptured: true, Builder: cfg.BuildkitBuilder, BuilderNode: nodeName,
		Driver: "docker-container",
	}
	state, err := loadIntegrationState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	state.Records["buildkit"] = record
	if err := saveIntegrationState(cfg, state); err != nil {
		t.Fatal(err)
	}
	status := inspectOperationalStatus(context.Background(), cfg)
	if got := status.Integrations["buildkit"]; got.State != "configuration-missing" || got.Active {
		t.Fatalf("missing BuildKit configuration status = %#v", got)
	}
	if health := inspectIntegrationHealth(status); health.OK || !strings.Contains(health.Detail, "buildkit=configuration-missing") {
		t.Fatalf("doctor integration health = %#v", health)
	}

	directory := t.TempDir()
	commandLog := filepath.Join(directory, "commands.log")
	dockerCommand := filepath.Join(directory, "docker")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$LC_BUILDKIT_COMMAND_LOG"
if [ "$1" = buildx ] && [ "$2" = ls ]; then
  printf '{"Name":"%s","Driver":"docker-container","Current":true,"Nodes":[{"Name":"%s","Status":"running"}]}\n' "$LC_BUILDER_NAME" "$LC_BUILDER_NODE"
fi
`
	if err := os.WriteFile(dockerCommand, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LC_BUILDKIT_COMMAND_LOG", commandLog)
	t.Setenv("LC_BUILDER_NAME", cfg.BuildkitBuilder)
	t.Setenv("LC_BUILDER_NODE", nodeName)

	var stdout, stderr bytes.Buffer
	if err := runBuildkitIntegration(context.Background(), []string{
		"--config", configPath, "--docker-command", dockerCommand, "--apply", "--json",
	}, &stdout, &stderr); err != nil {
		t.Fatalf("repair missing BuildKit configuration: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(current, desired) {
		t.Fatalf("repaired configuration = %q, want %q", current, desired)
	}
	commands, err := os.ReadFile(commandLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"buildx rm --keep-state " + cfg.BuildkitBuilder,
		"buildx create --name " + cfg.BuildkitBuilder + " --driver docker-container --node " + nodeName + " --buildkitd-config " + path,
		"buildx use --global " + cfg.BuildkitBuilder,
	} {
		if !strings.Contains(string(commands), expected) {
			t.Fatalf("BuildKit repair commands do not contain %q:\n%s", expected, commands)
		}
	}
	repairedState, err := loadIntegrationState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := repairedState.Records["buildkit"]; got.State != "active" || got.Digest != contentDigest(desired) {
		t.Fatalf("repaired ownership state = %#v", got)
	}

	const edited = "# operator-owned edit\n"
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(commandLog, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	err = runBuildkitIntegration(context.Background(), []string{
		"--config", configPath, "--docker-command", dockerCommand, "--apply", "--json",
	}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "changed outside Layer Cache") {
		t.Fatalf("edited configuration apply error = %v", err)
	}
	unchanged, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(unchanged) != edited {
		t.Fatalf("operator edit was replaced: %q", unchanged)
	}
	commands, err = os.ReadFile(commandLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(commands), "buildx rm") || strings.Contains(string(commands), "buildx create") {
		t.Fatalf("edited configuration triggered builder mutation:\n%s", commands)
	}
}

func TestBuildkitApplyRejectsOwnedBuilderRetarget(t *testing.T) {
	configPath, _ := setupLocalConfig(t)
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	originalBuilder := cfg.BuildkitBuilder
	state, err := loadIntegrationState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	state.Records["buildkit"] = integrationRecord{
		Name: "buildkit", State: "active", OwnershipCaptured: true,
		Path:   filepath.Join(cfg.DataDir, "buildkit", "buildkitd.toml"),
		Digest: "sha256:" + strings.Repeat("a", 64), Builder: originalBuilder,
		BuilderNode: buildkitNodeName(cfg), Driver: "docker-container",
	}
	if err := saveIntegrationState(cfg, state); err != nil {
		t.Fatal(err)
	}
	cfg.BuildkitBuilder = "layercache-retargeted"
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	commandLog := filepath.Join(directory, "commands.log")
	dockerCommand := filepath.Join(directory, "docker")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$LC_BUILDKIT_COMMAND_LOG\"\nprintf '[]\\n'\n"
	if err := os.WriteFile(dockerCommand, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LC_BUILDKIT_COMMAND_LOG", commandLog)

	var stdout, stderr bytes.Buffer
	err = runBuildkitIntegration(context.Background(), []string{
		"--config", configPath, "--docker-command", dockerCommand, "--apply", "--json",
	}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), originalBuilder) ||
		!strings.Contains(err.Error(), cfg.BuildkitBuilder) || !strings.Contains(err.Error(), "uninstall") {
		t.Fatalf("BuildKit builder retarget error = %v", err)
	}
	if _, err := os.Stat(commandLog); !os.IsNotExist(err) {
		t.Fatalf("rejected BuildKit retarget invoked Docker: %v", err)
	}
	unchanged, err := loadIntegrationState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Records["buildkit"].Builder != originalBuilder {
		t.Fatalf("rejected BuildKit retarget changed ownership to %#v", unchanged.Records["buildkit"])
	}
}
