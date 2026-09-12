package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/layercache/layercache/internal/config"
)

func TestLoadProjectGatewayProtectedConfiguration(t *testing.T) {
	root := t.TempDir()
	project, err := config.Defaults()
	if err != nil {
		t.Fatal(err)
	}
	project.Role = "team"
	projectPath := filepath.Join(root, "project.json")
	if err := config.Save(projectPath, project); err != nil {
		t.Fatal(err)
	}
	file := projectGatewayFile{Listen: "127.0.0.1:7437", Origin: "https://cache.example", Projects: map[string]string{"alpha": projectPath}}
	data, _ := json.MarshalIndent(file, "", "  ")
	path := filepath.Join(root, "gateway.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	_, cfg, err := loadProjectGateway(path)
	if err != nil || cfg.Projects["alpha"].ProjectID != project.ProjectID {
		t.Fatalf("load gateway: %v", err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadProjectGateway(path); err == nil {
		t.Fatal("accepted world-readable gateway config")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadProjectGateway(link); err == nil {
		t.Fatal("accepted symlink gateway config")
	}
}

func TestLoadProjectGatewayRejectsPublicListener(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.json")
	if err := os.WriteFile(path, []byte(`{"listen":"0.0.0.0:7437","origin":"https://cache.example","projects":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadProjectGateway(path); err == nil {
		t.Fatal("accepted public cleartext listener")
	}
}
