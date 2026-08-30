package remote_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/layercache/layercache/internal/remote"
)

func TestTurboClientRequiresHTTPSExceptOnLoopback(t *testing.T) {
	t.Parallel()

	for _, endpoint := range []string{
		"http://cache.example.com",
		"http://token@cache.example.com",
		"https://cache.example.com?token=secret",
		"https://cache.example.com#fragment",
	} {
		if _, err := remote.NewTurboClient(endpoint, "test-token"); err == nil {
			t.Fatalf("NewTurboClient(%q) succeeded, want an error", endpoint)
		}
	}

	for _, endpoint := range []string{
		"http://127.0.0.1:7437",
		"http://[::1]:7437",
		"https://cache.example.com",
	} {
		if _, err := remote.NewTurboClient(endpoint, "test-token"); err != nil {
			t.Fatalf("NewTurboClient(%q): %v", endpoint, err)
		}
	}
}

func TestTurboClientRejectsCrossOriginRedirectWithoutSendingCredential(t *testing.T) {
	t.Parallel()

	var targetRequests atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		targetRequests.Add(1)
		response.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	source := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, target.URL+"/artifact", http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	client, err := remote.NewTurboClient(source.URL, "sensitive-token")
	if err != nil {
		t.Fatalf("new Turbo client: %v", err)
	}
	_, err = client.Get(context.Background(), "task-hash")
	if err == nil || !strings.Contains(err.Error(), "changed origin") {
		t.Fatalf("cross-origin redirect error = %v", err)
	}
	if got := targetRequests.Load(); got != 0 {
		t.Fatalf("cross-origin target received %d requests, want none", got)
	}
}

func TestTurboClientSendsValidatedCompatibilityOnlyToItsOrigin(t *testing.T) {
	t.Parallel()

	const identity = "darwin-arm64-schema1-node@24"
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("X-LayerCache-Compatibility"); got != identity {
			t.Errorf("compatibility header = %q, want %q", got, identity)
		}
		response.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client, err := remote.NewTurboClientForCompatibility(server.URL, "team-token", identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Get(context.Background(), "task-hash"); err != remote.ErrMiss {
		t.Fatalf("Get = %v, want miss", err)
	}
	for _, invalid := range []string{"", "Darwin-arm64", "darwin arm64"} {
		if _, err := remote.NewTurboClientForCompatibility(server.URL, "team-token", invalid); err == nil {
			t.Errorf("compatibility %q was accepted", invalid)
		}
	}
}
