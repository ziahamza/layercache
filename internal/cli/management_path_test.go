package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/layercache/layercache/internal/config"
)

func TestOwnershipAcceptsParentDirectoryAliasWithoutChangingInstallation(t *testing.T) {
	root := t.TempDir()
	realRoot := filepath.Join(root, "real")
	aliasRoot := filepath.Join(root, "alias")
	if err := os.Mkdir(realRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realRoot, aliasRoot); err != nil {
		t.Fatal(err)
	}
	aliasConfig := filepath.Join(aliasRoot, "config.json")
	runCLI(t, "setup", "--config", aliasConfig, "--data-dir", filepath.Join(aliasRoot, "cache"), "--non-interactive", "--json")
	cfg, err := config.Load(aliasConfig)
	if err != nil {
		t.Fatal(err)
	}
	canonicalConfig, err := filepath.EvalSymlinks(aliasConfig)
	if err != nil {
		t.Fatal(err)
	}
	// Same configuration, installation ID, and data directory. Only the spelling
	// of the configuration path changes between these two checks.
	if err := verifyOwnershipMarker(cfg, canonicalConfig); err != nil {
		t.Fatalf("canonical ownership check: %v", err)
	}
	if err := verifyOwnershipMarker(cfg, aliasConfig); err != nil {
		t.Fatalf("alias ownership check: %v", err)
	}
}
