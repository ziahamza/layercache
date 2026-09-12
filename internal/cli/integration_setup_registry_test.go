package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildkitRegistryHost(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"ghcr.io/acme/cache":        "ghcr.io",
		"localhost:5000/acme/cache": "localhost:5000",
		"acme/cache":                "docker.io",
		"library/cache":             "docker.io",
	}
	for repository, expected := range tests {
		repository, expected := repository, expected
		t.Run(repository, func(t *testing.T) {
			t.Parallel()
			actual, err := buildkitRegistryHost(repository)
			if err != nil {
				t.Fatal(err)
			}
			if actual != expected {
				t.Fatalf("registry host = %q, want %q", actual, expected)
			}
		})
	}

	for _, repository := range []string{"", "https://ghcr.io/acme/cache", "ghcr.io/acme/cache@sha256:abc", "ghcr.io/acme/cache,mode=max"} {
		if _, err := buildkitRegistryHost(repository); err == nil {
			t.Fatalf("expected unsafe repository %q to fail", repository)
		}
	}
}

func TestLoginBuildkitRegistryUsesPasswordStdinWithoutLeakingFailure(t *testing.T) {
	directory := t.TempDir()
	argumentsPath := filepath.Join(directory, "arguments")
	passwordPath := filepath.Join(directory, "password")
	commandPath := filepath.Join(directory, "docker")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > \"$QA_ARGUMENTS\"\n" +
		"cat > \"$QA_PASSWORD\"\n"
	if err := os.WriteFile(commandPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QA_ARGUMENTS", argumentsPath)
	t.Setenv("QA_PASSWORD", passwordPath)
	t.Setenv("DOCKER_CONFIG", filepath.Join(directory, "missing-docker-config"))

	const secret = "registry-secret"
	report, err := loginBuildkitRegistry(context.Background(), commandPath, "ghcr.io", "octocat", strings.NewReader(secret+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if report.Storage != "unverified" || report.Warning != dockerConfigCredentialWarning {
		t.Fatalf("credential storage report = %#v", report)
	}
	arguments, err := os.ReadFile(argumentsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(arguments) != "login\nghcr.io\n--username\noctocat\n--password-stdin\n" {
		t.Fatalf("unexpected Docker arguments: %q", arguments)
	}
	password, err := os.ReadFile(passwordPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(password) != secret+"\n" {
		t.Fatalf("password stdin = %q", password)
	}
	if strings.Contains(string(arguments), secret) {
		t.Fatal("registry secret appeared in Docker arguments")
	}

	failurePath := filepath.Join(directory, "failing-docker")
	failureScript := "#!/bin/sh\npassword=$(cat)\nprintf '%s' \"$password\" >&2\nexit 1\n"
	if err := os.WriteFile(failurePath, []byte(failureScript), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err = loginBuildkitRegistry(context.Background(), failurePath, "ghcr.io", "octocat", strings.NewReader(secret))
	if err == nil {
		t.Fatal("expected Docker login failure")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("registry secret appeared in the returned error")
	}
}

func TestInspectDockerCredentialStorage(t *testing.T) {
	tests := []struct {
		name            string
		registry        string
		config          string
		writeConfig     bool
		expectedStorage string
		expectedWarning string
	}{
		{
			name:            "global external helper",
			registry:        "ghcr.io",
			config:          `{"credsStore":"secretservice"}`,
			writeConfig:     true,
			expectedStorage: "external-helper",
		},
		{
			name:            "registry external helper",
			registry:        "ghcr.io",
			config:          `{"credHelpers":{"ghcr.io":"pass"}}`,
			writeConfig:     true,
			expectedStorage: "external-helper",
		},
		{
			name:            "Docker Hub legacy helper name",
			registry:        "docker.io",
			config:          `{"credHelpers":{"https://index.docker.io/v1/":"osxkeychain"}}`,
			writeConfig:     true,
			expectedStorage: "external-helper",
		},
		{
			name:            "reversible config auth",
			registry:        "ghcr.io",
			config:          `{"auths":{"ghcr.io":{"auth":"registry-secret"}}}`,
			writeConfig:     true,
			expectedStorage: "config.json",
			expectedWarning: dockerConfigCredentialWarning,
		},
		{
			name:            "different registry helper",
			registry:        "ghcr.io",
			config:          `{"credHelpers":{"registry.example.com":"pass"}}`,
			writeConfig:     true,
			expectedStorage: "unverified",
			expectedWarning: dockerConfigCredentialWarning,
		},
		{
			name:            "malformed config",
			registry:        "ghcr.io",
			config:          `{`,
			writeConfig:     true,
			expectedStorage: "unverified",
			expectedWarning: dockerConfigCredentialWarning,
		},
		{
			name:            "missing config",
			registry:        "ghcr.io",
			expectedStorage: "unverified",
			expectedWarning: dockerConfigCredentialWarning,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			t.Setenv("DOCKER_CONFIG", directory)
			if test.writeConfig {
				if err := os.WriteFile(filepath.Join(directory, "config.json"), []byte(test.config), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			report := inspectDockerCredentialStorage(test.registry)
			if report.Storage != test.expectedStorage || report.Warning != test.expectedWarning {
				t.Fatalf("credential storage report = %#v, want storage %q and warning %q", report, test.expectedStorage, test.expectedWarning)
			}
			if strings.Contains(report.Warning, "registry-secret") {
				t.Fatal("Docker config secret appeared in the warning")
			}
		})
	}
}
