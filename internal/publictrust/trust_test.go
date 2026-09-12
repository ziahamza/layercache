package publictrust_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
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
		Integration:        "turbo",
		Project:            "github.com/acme/widget",
		Compatibility:      "linux-amd64-schema1",
		NativeKey:          "turbo-hash",
		Repository:         publication.Repository,
		Commit:             publication.Commit,
		RecipeDigest:       publication.RecipeDigest,
		Target:             publication.Target,
		Platform:           publication.Platform,
		Inputs:             publication.Inputs,
		Toolchain:          publication.Toolchain,
		Builder:            publication.Builder,
		BuilderImageDigest: publication.BuilderImageDigest,
		BuildID:            publication.BuildID,
		PublicIdentity:     publication.Identity(),
		Digest:             publication.Digest,
	}, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if verified.Commit != publication.Commit || verified.BuildID != publication.BuildID {
		t.Fatalf("verified publication = %+v, want commit and build ID from %+v", verified, publication)
	}
	changedImage := publication
	changedImage.BuilderImageDigest = "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	if changedImage.Identity() == publication.Identity() {
		t.Fatal("builder image digest did not affect signed publication identity")
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

	wrongSource := publictrust.Expected{
		Integration: publication.Integration, Project: publication.Project,
		Compatibility: publication.Compatibility, NativeKey: publication.NativeKey,
		Repository: publication.Repository, Commit: publication.Commit,
		RecipeDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}
	if _, err := publictrust.Verify(publicKey, envelope, wrongSource, now.Add(time.Hour)); !errors.Is(err, publictrust.ErrIdentity) {
		t.Fatalf("wrong recipe error = %v, want ErrIdentity", err)
	}
	wrongInputs := publictrust.Expected{
		Integration: publication.Integration, Project: publication.Project,
		Compatibility: publication.Compatibility, NativeKey: publication.NativeKey,
		Inputs: []publictrust.DeclaredInput{{Name: "compatibility", Value: "linux-amd64-node@22"}},
	}
	if _, err := publictrust.Verify(publicKey, envelope, wrongInputs, now.Add(time.Hour)); !errors.Is(err, publictrust.ErrIdentity) {
		t.Fatalf("wrong inputs error = %v, want ErrIdentity", err)
	}
	wrongBuilderImage := publictrust.Expected{
		Integration: publication.Integration, Project: publication.Project,
		Compatibility: publication.Compatibility, NativeKey: publication.NativeKey,
		BuilderImageDigest: "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
	}
	if _, err := publictrust.Verify(publicKey, envelope, wrongBuilderImage, now.Add(time.Hour)); !errors.Is(err, publictrust.ErrIdentity) {
		t.Fatalf("wrong builder image error = %v, want ErrIdentity", err)
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
	buildIdentity := publictrust.BuildIdentity{
		Repository: first.Repository, Commit: first.Commit, Integration: first.Integration,
		Target: first.Target, RecipeDigest: first.RecipeDigest, Platform: first.Platform, Inputs: first.Inputs,
	}
	if found, err := registry.FindBuild(ctx, buildIdentity); err != nil || found.Identity() != first.Identity() {
		t.Fatalf("find build by declared inputs = %#v, %v", found, err)
	}
	buildIdentity.Inputs = []publictrust.DeclaredInput{{Name: "compatibility", Value: "linux-amd64-node@22"}}
	if _, err := registry.FindBuild(ctx, buildIdentity); !errors.Is(err, publictrust.ErrNotFound) {
		t.Fatalf("find build with wrong inputs error = %v, want ErrNotFound", err)
	}
	if err := registry.Publish(ctx, first); err != nil {
		t.Fatalf("idempotent publication failed: %v", err)
	}
	metadataConflict := first
	metadataConflict.Builder = "another-builder@sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	if err := registry.Publish(ctx, metadataConflict); !errors.Is(err, publictrust.ErrIdentity) {
		t.Fatalf("same bytes under different provenance error = %v, want ErrIdentity", err)
	}
	divergent := first
	divergent.Digest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if err := registry.Publish(ctx, divergent); !errors.Is(err, publictrust.ErrAmbiguous) {
		t.Fatalf("divergent publication error = %v, want ErrAmbiguous", err)
	}
	if _, err := registry.Resolve(ctx, first.CacheIdentity()); !errors.Is(err, publictrust.ErrAmbiguous) {
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

func TestRegistryExpiresAfterIdleRetentionAndKeepsTombstone(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	clock := now
	registry, err := publictrust.OpenRegistryWithOptions(ctx, t.TempDir(), publictrust.RegistryOptions{
		Retention: 2 * time.Hour,
		Now:       func() time.Time { return clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()

	publication := fixturePublication(now)
	if err := registry.Publish(ctx, publication); err != nil {
		t.Fatal(err)
	}
	clock = now.Add(time.Hour)
	if _, err := registry.Resolve(ctx, publication.CacheIdentity()); err != nil {
		t.Fatalf("resolve before idle expiry: %v", err)
	}
	clock = now.Add(2*time.Hour + 30*time.Minute)
	if _, err := registry.Resolve(ctx, publication.Identity()); err != nil {
		t.Fatalf("access did not extend idle retention: %v", err)
	}
	clock = now.Add(5 * time.Hour)
	if _, err := registry.Resolve(ctx, publication.CacheIdentity()); !errors.Is(err, publictrust.ErrExpired) {
		t.Fatalf("expired resolve error = %v, want ErrExpired", err)
	}
	if _, err := registry.Resolve(ctx, publication.CacheIdentity()); !errors.Is(err, publictrust.ErrExpired) {
		t.Fatalf("expired tombstone resolve error = %v, want ErrExpired", err)
	}
	replacement := publication
	replacement.BuildID = "build-replacement"
	replacement.IssuedAt = clock
	replacement.ExpiresAt = clock.Add(24 * time.Hour)
	if err := registry.Publish(ctx, replacement); err != nil {
		t.Fatalf("replace expired publication: %v", err)
	}
	resolved, err := registry.Resolve(ctx, replacement.Identity())
	if err != nil {
		t.Fatalf("resolve replacement publication: %v", err)
	}
	if resolved.BuildID != replacement.BuildID {
		t.Fatalf("replacement build = %q, want %q", resolved.BuildID, replacement.BuildID)
	}
}

func TestActivePublicationsReturnsOnlyLiveEntriesWithoutExtendingRetention(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	clock := now
	registry, err := publictrust.OpenRegistryWithOptions(ctx, t.TempDir(), publictrust.RegistryOptions{
		Retention: 2 * time.Hour,
		Now:       func() time.Time { return clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	publication := fixturePublication(now)
	if err := registry.Publish(ctx, publication); err != nil {
		t.Fatal(err)
	}

	clock = now.Add(time.Hour)
	active, err := registry.ActivePublications(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].Identity() != publication.Identity() {
		t.Fatalf("active publications = %#v", active)
	}

	clock = now.Add(2*time.Hour + time.Second)
	if active, err := registry.ActivePublications(ctx); err != nil || len(active) != 0 {
		t.Fatalf("expired active publications = %#v, error = %v", active, err)
	}
	if _, err := registry.Resolve(ctx, publication.CacheIdentity()); !errors.Is(err, publictrust.ErrExpired) {
		t.Fatalf("active snapshot extended retention: %v", err)
	}
}

func TestRegistryRetiresOnlyTheExactUnusablePublicationForReplacement(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	registry, err := publictrust.OpenRegistry(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	first := fixturePublication(now)
	if err := registry.Publish(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := registry.Retire(
		ctx, first.CacheIdentity(), strings.Repeat("f", 64), "stale repair", now,
	); !errors.Is(err, publictrust.ErrIdentity) {
		t.Fatalf("retire wrong public identity = %v, want ErrIdentity", err)
	}
	if _, err := registry.Resolve(ctx, first.CacheIdentity()); err != nil {
		t.Fatalf("wrong retirement changed the active publication: %v", err)
	}
	if err := registry.Retire(
		ctx, first.CacheIdentity(), first.Identity(), "artifact unavailable", now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Resolve(ctx, first.CacheIdentity()); !errors.Is(err, publictrust.ErrExpired) {
		t.Fatalf("retired publication resolve = %v, want ErrExpired", err)
	}

	replacement := first
	replacement.BuildID = "build-repair"
	replacement.IssuedAt = now.Add(time.Minute)
	replacement.ExpiresAt = now.Add(24*time.Hour + time.Minute)
	if err := registry.Publish(ctx, replacement); err != nil {
		t.Fatalf("publish exact-byte repair: %v", err)
	}
	resolved, err := registry.Resolve(ctx, replacement.CacheIdentity())
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Identity() != replacement.Identity() || resolved.BuildID != replacement.BuildID {
		t.Fatalf("replacement publication = %#v", resolved)
	}
	if err := registry.Retire(
		ctx, first.CacheIdentity(), first.Identity(), "stale retirement", now,
	); !errors.Is(err, publictrust.ErrIdentity) {
		t.Fatalf("stale retirement after replacement = %v, want ErrIdentity", err)
	}
}

func fixturePublication(now time.Time) publictrust.Publication {
	return publictrust.Publication{
		Integration:        "turbo",
		Project:            "github.com/acme/widget",
		Compatibility:      "linux-amd64-schema1",
		NativeKey:          "turbo-hash",
		Repository:         "https://github.com/acme/widget",
		Commit:             "0123456789abcdef0123456789abcdef01234567",
		RecipeDigest:       "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		Target:             "test",
		Platform:           "linux/amd64",
		Inputs:             []publictrust.DeclaredInput{{Name: "compatibility", Value: "linux-amd64-schema1"}},
		Toolchain:          "turbo@2.10.9",
		Builder:            "layercache-public-builder@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		BuilderImageDigest: "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
		Digest:             "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Size:               1234,
		DurationMS:         9500,
		BuildID:            "build-123",
		IssuedAt:           now,
		ExpiresAt:          now.Add(24 * time.Hour),
	}
}
