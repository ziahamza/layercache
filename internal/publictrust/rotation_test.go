package publictrust_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/publictrust"
)

func TestSignedTrustRotationBindsServiceAndRejectsRollback(t *testing.T) {
	old, private, _ := ed25519.GenerateKey(rand.Reader)
	newKey, _, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	rotation := publictrust.Rotation{Version: 1, Endpoint: "https://cache.example.com", Sequence: 1,
		PreviousKey: publictrust.EncodePublicKey(old), NextKey: publictrust.EncodePublicKey(newKey), IssuedAt: now, ExpiresAt: now.Add(time.Hour)}
	envelope, err := publictrust.SignRotation(private, rotation)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := publictrust.VerifyRotation(old, envelope, rotation.Endpoint, 0, now)
	if err != nil || verified != rotation {
		t.Fatalf("rotation: %+v, %v", verified, err)
	}
	for _, test := range []struct {
		name, endpoint string
		sequence       uint64
		at             time.Time
		key            ed25519.PublicKey
	}{
		{"wrong service", "https://other.example.com", 0, now, old},
		{"replay", rotation.Endpoint, 1, now, old},
		{"rollback", rotation.Endpoint, 2, now, old},
		{"expired", rotation.Endpoint, 0, now.Add(time.Hour), old},
		{"future", rotation.Endpoint, 0, now.Add(-time.Second), old},
		{"wrong signer", rotation.Endpoint, 0, now, newKey},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := publictrust.VerifyRotation(test.key, envelope, test.endpoint, test.sequence, test.at); err == nil {
				t.Fatal("untrusted rotation accepted")
			}
		})
	}
	rotation.Endpoint = "http://cache.example.com"
	if _, err := publictrust.SignRotation(private, rotation); err == nil {
		t.Fatal("insecure service identity accepted")
	}
}
