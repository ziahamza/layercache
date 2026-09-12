package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/config"
	"github.com/layercache/layercache/internal/publictrust"
)

func TestPublicTrustUpdatePreviewsAppliesAndRejectsReplay(t *testing.T) {
	path, _ := setupLocalConfig(t)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	old, private, _ := ed25519.GenerateKey(rand.Reader)
	next, _, _ := ed25519.GenerateKey(rand.Reader)
	cfg.PublicURL, cfg.PublicTrustKey = "https://public.example.com", publictrust.EncodePublicKey(old)
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Second)
	envelope, err := publictrust.SignRotation(private, publictrust.Rotation{Version: 1, Endpoint: cfg.PublicURL, Sequence: 1,
		PreviousKey: cfg.PublicTrustKey, NextKey: publictrust.EncodePublicKey(next), IssuedAt: now, ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	rotation := filepath.Join(t.TempDir(), "rotation.json")
	if err := os.WriteFile(rotation, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := callCLI("public", "trust-update", "--config", path, "--rotation", rotation, "--json"); err != nil {
		t.Fatal(err)
	}
	preview, _ := config.Load(path)
	if preview.PublicTrustKey != cfg.PublicTrustKey || preview.PublicTrustSequence != 0 {
		t.Fatal("preview modified trust")
	}
	output, _, err := callCLI("public", "trust-update", "--config", path, "--rotation", rotation, "--apply", "--json")
	if err != nil || !strings.Contains(output, `"applied":true`) {
		t.Fatalf("apply: %s, %v", output, err)
	}
	updated, _ := config.Load(path)
	if updated.PublicTrustKey != publictrust.EncodePublicKey(next) || updated.PublicTrustSequence != 1 {
		t.Fatal("new trust root not persisted")
	}
	if _, _, err := callCLI("public", "trust-update", "--config", path, "--rotation", rotation, "--apply"); err == nil {
		t.Fatal("rotation replay accepted")
	}
}
