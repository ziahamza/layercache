package server

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/layercache/layercache/internal/actionscache"
	"github.com/layercache/layercache/internal/publictrust"
	"github.com/layercache/layercache/internal/remote"
)

func TestActionsPublicErrorClassificationPreservesTrustFailures(t *testing.T) {
	t.Parallel()

	for _, trustFailure := range []error{
		publictrust.ErrSignature,
		publictrust.ErrIdentity,
		publictrust.ErrExpired,
		publictrust.ErrRevoked,
		publictrust.ErrAmbiguous,
	} {
		classified := classifyActionsPublicError(context.Background(), trustFailure)
		if !errors.Is(classified, trustFailure) {
			t.Fatalf("classification of %v = %v", trustFailure, classified)
		}
		if errors.Is(classified, actionscache.ErrPublicOffline) {
			t.Fatalf("trust failure %v was classified as offline", trustFailure)
		}
	}
}

func TestActionsPublicErrorClassificationSeparatesMissesAndAvailability(t *testing.T) {
	t.Parallel()

	miss := classifyActionsPublicError(context.Background(), remote.ErrMiss)
	if !errors.Is(miss, publictrust.ErrNotFound) || errors.Is(miss, actionscache.ErrPublicOffline) {
		t.Fatalf("remote miss classification = %v", miss)
	}

	for _, unavailable := range []error{
		&net.DNSError{Err: "temporary failure", Name: "cache.example", IsTemporary: true},
		errors.New("Public Cache resolve returned HTTP 503"),
		errors.New("Public Cache artifact returned HTTP 429"),
	} {
		classified := classifyActionsPublicError(context.Background(), unavailable)
		if !errors.Is(classified, actionscache.ErrPublicOffline) {
			t.Fatalf("availability failure %v classified as %v", unavailable, classified)
		}
	}

	protocolFailure := errors.New("Public Cache returned an invalid artifact URL")
	if classified := classifyActionsPublicError(context.Background(), protocolFailure); errors.Is(classified, actionscache.ErrPublicOffline) {
		t.Fatalf("protocol failure classified as offline: %v", classified)
	}
}

func TestActionsPublicErrorClassificationHonorsCallerCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	classified := classifyActionsPublicError(ctx, &net.DNSError{Err: "cancelled", Name: "cache.example", IsTemporary: true})
	if !errors.Is(classified, context.Canceled) || errors.Is(classified, actionscache.ErrPublicOffline) {
		t.Fatalf("cancelled request classification = %v", classified)
	}
}
