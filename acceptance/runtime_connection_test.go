package acceptance_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type runtimeConnections struct {
	TurboAPI     string
	TurboToken   string
	TurboTeam    string
	ActionsURL   string
	ActionsToken string
}

// captureRuntimeConnections exercises the installed layercache run command and
// lets its child hand the short-lived credentials back through a protected
// temporary file. Integration previews intentionally never print credentials.
func captureRuntimeConnections(t *testing.T, binary, configPath string) runtimeConnections {
	t.Helper()
	directory := t.TempDir()
	path := filepath.Join(directory, "connections")
	script := `umask 077
printf '%s\n%s\n%s\n%s\n%s\n' "$TURBO_API" "$TURBO_TOKEN" "$TURBO_TEAM" "$ACTIONS_CACHE_URL" "$ACTIONS_RUNTIME_TOKEN" > "$1"`
	command := exec.Command(
		binary, "run", "--config", configPath, "--",
		"sh", "-c", script, "layercache-acceptance-connection", path,
	)
	// Keep project discovery from inheriting the layercache repository that is
	// running the acceptance suite. Each fixture's saved config is authoritative.
	command.Dir = directory
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("capture installed runtime connections: %v\n%s", err, output)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("runtime connection handoff mode = %s, want an owner-only regular file", info.Mode())
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(contents), "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("runtime connection handoff has %d fields, want 5", len(lines))
	}
	connections := runtimeConnections{
		TurboAPI: lines[0], TurboToken: lines[1], TurboTeam: lines[2],
		ActionsURL: lines[3], ActionsToken: lines[4],
	}
	if connections.TurboAPI == "" || connections.TurboToken == "" || connections.TurboTeam == "" ||
		connections.ActionsURL == "" || connections.ActionsToken == "" {
		t.Fatal("installed runtime connection handoff omitted a required field")
	}
	if connections.TurboToken == connections.ActionsToken ||
		!strings.HasPrefix(connections.TurboToken, "lc2.") ||
		!strings.HasPrefix(connections.ActionsToken, "lc2.") {
		t.Fatal("installed runtime did not issue distinct scoped integration credentials")
	}
	return connections
}

func captureTurboConnection(t *testing.T, binary, configPath string) turboConnection {
	t.Helper()
	connections := captureRuntimeConnections(t, binary, configPath)
	return turboConnection{APIURL: connections.TurboAPI, Token: connections.TurboToken, Team: connections.TurboTeam}
}

func captureActionsConnection(t *testing.T, binary, configPath string) actionsConnectionResult {
	t.Helper()
	connections := captureRuntimeConnections(t, binary, configPath)
	return actionsConnectionResult{CacheURL: connections.ActionsURL, Token: connections.ActionsToken}
}
