package acceptance_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunInjectsScopedTurboAndActionsConnections(t *testing.T) {
	root := t.TempDir()
	binary := buildLayerCache(t)
	address := availableAddress(t)
	configPath := filepath.Join(root, "config.json")
	runBinary(t, binary,
		"setup", "--config", configPath,
		"--data-dir", filepath.Join(root, "cache"),
		"--listen", address,
		"--project", "github.com/acme/widget",
		"--non-interactive", "--json",
	)
	runBinary(t, binary, "start", "--config", configPath, "--json")
	t.Cleanup(func() {
		command := exec.Command(binary, "stop", "--config", configPath, "--json")
		_ = command.Run()
	})

	environmentFile := filepath.Join(root, "environment.json")
	script := `printf '{"runId":"%s","turboApi":"%s","turboToken":"%s","turboTeam":"%s","actionsUrl":"%s","actionsToken":"%s"}' "$LAYER_CACHE_RUN_ID" "$TURBO_API" "$TURBO_TOKEN" "$TURBO_TEAM" "$ACTIONS_CACHE_URL" "$ACTIONS_RUNTIME_TOKEN" > "$1"`
	runBinary(t, binary, "run", "--config", configPath, "--", "sh", "-c", script, "layercache-run", environmentFile)

	contents, err := os.ReadFile(environmentFile)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		RunID        string `json:"runId"`
		TurboAPI     string `json:"turboApi"`
		TurboToken   string `json:"turboToken"`
		TurboTeam    string `json:"turboTeam"`
		ActionsURL   string `json:"actionsUrl"`
		ActionsToken string `json:"actionsToken"`
	}
	if err := json.Unmarshal(contents, &got); err != nil {
		t.Fatal(err)
	}
	if got.RunID == "" || got.TurboAPI != "http://"+address || got.TurboTeam != "github.com/acme/widget" || got.ActionsURL != "http://"+address+"/" {
		t.Fatalf("injected environment = %+v", got)
	}
	if got.TurboToken != got.ActionsToken || !strings.HasPrefix(got.TurboToken, "lc1.") {
		t.Fatalf("workspace tokens = %q and %q", got.TurboToken, got.ActionsToken)
	}
}
