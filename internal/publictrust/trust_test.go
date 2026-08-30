package publictrust_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/publictrust"
)

func TestSignedPublicationVerifiesOnlyForExactIdentityAndValidity(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	publication := fixturePublication(now)

	envelope, err := publictrust.Sign(privateKey, publication)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := publictrust.Verify(publicKey, envelope, publictrust.Expected{
		Integration:   "turbo",
		Project:       "github.com/acme/widget",
		Compatibility: "linux-amd64-schema1",
		NativeKey:     "turbo-hash",
		Digest:        publication.Digest,
	}, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if verified.Commit != publication.Commit || verified.BuildID != publication.BuildID {
		t.Fatalf("verified publication = %+v, want commit and build ID from %+v", verified, publication)
	}

	_, err = publictrust.Verify(publicKey, envelope, publictrust.Expected{
		Integration:   "turbo",
		Project:       "github.com/acme/other",
		Compatibility: "linux-amd64-schema1",
		NativeKey:     "turbo-hash",
		Digest:        publication.Digest,
	}, now.Add(time.Hour))
	if !errors.Is(err, publictrust.ErrIdentity) {
		t.Fatalf("wrong project error = %v, want ErrIdentity", err)
	}

	_, err = publictrust.Verify(publicKey, envelope, publictrust.Expected{
		Integration:   "turbo",
		Project:       "github.com/acme/widget",
		Compatibility: "linux-amd64-schema1",
		NativeKey:     "turbo-hash",
		Digest:        publication.Digest,
	}, publication.ExpiresAt.Add(time.Nanosecond))
	if !errors.Is(err, publictrust.ErrExpired) {
		t.Fatalf("expired publication error = %v, want ErrExpired", err)
	}
}

func TestRegistryMakesDivergentPublicationAmbiguousAndRevocationSticky(t *testing.T) {
	ctx := context.Background()
	registry, err := publictrust.OpenRegistry(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()

	now := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	first := fixturePublication(now)
	if err := registry.Publish(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := registry.Publish(ctx, first); err != nil {
		t.Fatalf("idempotent publication failed: %v", err)
	}

	divergent := first
	divergent.Digest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if err := registry.Publish(ctx, divergent); !errors.Is(err, publictrust.ErrAmbiguous) {
		t.Fatalf("divergent publication error = %v, want ErrAmbiguous", err)
	}
	if _, err := registry.Resolve(ctx, first.Identity()); !errors.Is(err, publictrust.ErrAmbiguous) {
		t.Fatalf("ambiguous resolve error = %v, want ErrAmbiguous", err)
	}

	second := fixturePublication(now)
	second.NativeKey = "another-hash"
	if err := registry.Publish(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err := registry.Revoke(ctx, second.Identity(), "bad builder image", now); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Resolve(ctx, second.Identity()); !errors.Is(err, publictrust.ErrRevoked) {
		t.Fatalf("revoked resolve error = %v, want ErrRevoked", err)
	}
	if err := registry.Publish(ctx, second); !errors.Is(err, publictrust.ErrRevoked) {
		t.Fatalf("republish revoked identity error = %v, want ErrRevoked", err)
	}
}

func fixturePublication(now time.Time) publictrust.Publication {
	return publictrust.Publication{
		Integration:   "turbo",
		Project:       "github.com/acme/widget",
		Compatibility: "linux-amd64-schema1",
		NativeKey:     "turbo-hash",
		Repository:    "https://github.com/acme/widget",
		Commit:        "0123456789abcdef0123456789abcdef01234567",
		RecipeDigest:  "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		Platform:      "linux/amd64",
		Toolchain:     "turbo@2.10.9",
		Builder:       "layercache-public-builder@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		Digest:        "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Size:          1234,
		DurationMS:    9500,
		BuildID:       "build-123",
		IssuedAt:      now,
		ExpiresAt:     now.Add(24 * time.Hour),
	}
}
