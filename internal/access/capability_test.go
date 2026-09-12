package access_test

import (
	"errors"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/access"
)

func TestCapabilityTokenRoundTripAndAuthority(t *testing.T) {
	now := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	token, err := access.MintCapabilityToken("secret", access.Claims{
		Subject: "github:42", RunID: "run-1", Project: "project-1", Compatibility: "linux-amd64-schema1",
		Repository: "acme/widgets", Ref: "refs/pull/9/merge", DefaultRef: "refs/heads/main",
		Capabilities: []access.Capability{access.CapabilityWrite}, ExpiresAt: now.Add(time.Hour),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := access.ParseCapabilityToken("secret", token, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !claims.Allows(access.CapabilityRead) || !claims.Allows(access.CapabilityWrite) || claims.Allows(access.CapabilityAdmin) {
		t.Fatalf("unexpected authority: %#v", claims.Capabilities)
	}
	if claims.Repository != "acme/widgets" || claims.Ref != "refs/pull/9/merge" {
		t.Fatalf("scope was not preserved: %#v", claims)
	}
}

func TestCapabilityTokenRejectsTamperingAndExpiry(t *testing.T) {
	now := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	token, err := access.MintCapabilityToken("secret", access.Claims{
		Subject: "worker", Project: "project-1", Capabilities: []access.Capability{access.CapabilityRead}, ExpiresAt: now.Add(time.Minute),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := access.ParseCapabilityToken("wrong", token, now); !errors.Is(err, access.ErrInvalidToken) {
		t.Fatalf("wrong key error = %v", err)
	}
	if _, err := access.ParseCapabilityToken("secret", token, now.Add(2*time.Minute)); !errors.Is(err, access.ErrInvalidToken) && !errors.Is(err, access.ErrExpiredToken) {
		t.Fatalf("expired token error = %v", err)
	}
}
