package access_test

import (
	"errors"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/access"
)

func TestWorkspaceTokenBindsRunAndExpiry(t *testing.T) {
	now := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	token, err := access.MintWorkspaceToken("host-secret", "run-123", now, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	runID, err := access.ParseWorkspaceToken("host-secret", token, now.Add(5*time.Minute))
	if err != nil || runID != "run-123" {
		t.Fatalf("parse = %q, %v", runID, err)
	}
	if _, err := access.ParseWorkspaceToken("host-secret", token, now.Add(11*time.Minute)); !errors.Is(err, access.ErrExpiredToken) {
		t.Fatalf("expired error = %v, want ErrExpiredToken", err)
	}
	if _, err := access.ParseWorkspaceToken("different-secret", token, now); !errors.Is(err, access.ErrInvalidToken) {
		t.Fatalf("wrong-secret error = %v, want ErrInvalidToken", err)
	}
	tampered := token[:len(token)-1] + "A"
	if _, err := access.ParseWorkspaceToken("host-secret", tampered, now); !errors.Is(err, access.ErrInvalidToken) {
		t.Fatalf("tampered error = %v, want ErrInvalidToken", err)
	}
}
