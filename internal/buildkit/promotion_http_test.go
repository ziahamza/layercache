package buildkit_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/buildkit"
)

func TestHTTPPromotionCoordinatorAcquiresAndIdempotentlyReleasesLease(t *testing.T) {
	t.Parallel()

	var acquireCalls atomic.Int32
	var releaseCalls atomic.Int32
	reference := "registry.example/team/widget/linux-amd64:main"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer team-token" {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		var body struct {
			Reference  string `json:"reference"`
			LeaseToken string `json:"leaseToken"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(writer, "bad body", http.StatusBadRequest)
			return
		}
		if body.Reference != reference {
			http.Error(writer, "wrong reference", http.StatusBadRequest)
			return
		}
		switch request.URL.Path {
		case "/v1/buildkit/promotion-leases/acquire":
			if acquireCalls.Add(1) == 1 {
				writer.WriteHeader(http.StatusConflict)
				return
			}
			writer.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"leaseToken": "lease-123", "expiresAt": time.Now().UTC().Add(2 * time.Minute),
			})
		case "/v1/buildkit/promotion-leases/release":
			if body.LeaseToken != "lease-123" {
				http.Error(writer, "wrong lease", http.StatusConflict)
				return
			}
			releaseCalls.Add(1)
			writer.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	coordinator, err := buildkit.NewHTTPPromotionCoordinator(server.URL, "team-token")
	if err != nil {
		t.Fatalf("new coordinator: %v", err)
	}
	lease, err := coordinator.Acquire(context.Background(), reference)
	if err != nil {
		t.Fatalf("acquire lease: %v", err)
	}
	if err := lease.Release(context.Background()); err != nil {
		t.Fatalf("release lease: %v", err)
	}
	if err := lease.Release(context.Background()); err != nil {
		t.Fatalf("idempotent release: %v", err)
	}
	if acquireCalls.Load() != 2 || releaseCalls.Load() != 1 {
		t.Fatalf("acquire calls = %d, release calls = %d", acquireCalls.Load(), releaseCalls.Load())
	}
}

func TestHTTPPromotionCoordinatorStopsLeaseContentionOnCancellation(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusConflict)
	}))
	defer server.Close()
	coordinator, err := buildkit.NewHTTPPromotionCoordinator(server.URL, "team-token")
	if err != nil {
		t.Fatalf("new coordinator: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = coordinator.Acquire(ctx, "registry.example/team/widget/linux-amd64:main")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("acquire error = %v, want context deadline", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("cancelled acquire took %v", elapsed)
	}
}

func TestHTTPPromotionLeaseRenewsUntilPromotionFinishes(t *testing.T) {
	t.Parallel()

	var renewCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer team-token" {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		var body struct {
			Reference  string `json:"reference"`
			LeaseToken string `json:"leaseToken"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(writer, "bad body", http.StatusBadRequest)
			return
		}
		switch request.URL.Path {
		case "/v1/buildkit/promotion-leases/acquire":
			writer.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"leaseToken": "lease-renew", "expiresAt": time.Now().UTC().Add(80 * time.Millisecond),
			})
		case "/v1/buildkit/promotion-leases/renew":
			if body.LeaseToken != "lease-renew" {
				http.Error(writer, "wrong lease", http.StatusConflict)
				return
			}
			renewCalls.Add(1)
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"leaseToken": "lease-renew", "expiresAt": time.Now().UTC().Add(80 * time.Millisecond),
			})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	coordinator, err := buildkit.NewHTTPPromotionCoordinator(server.URL, "team-token")
	if err != nil {
		t.Fatalf("new coordinator: %v", err)
	}
	lease, err := coordinator.Acquire(context.Background(), "registry.example/team/widget/linux-amd64:main")
	if err != nil {
		t.Fatalf("acquire lease: %v", err)
	}
	maintained, ok := lease.(interface{ Maintain(context.Context) error })
	if !ok {
		t.Fatal("HTTP promotion lease has no renewal loop")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 230*time.Millisecond)
	defer cancel()
	if err := maintained.Maintain(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("maintain lease error = %v, want context deadline", err)
	}
	if renewCalls.Load() < 3 {
		t.Fatalf("promotion lease renewals = %d, want at least 3", renewCalls.Load())
	}
}
