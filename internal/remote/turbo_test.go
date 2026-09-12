package remote_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

func TestTurboClientUsesProgressIdleDeadlineInsteadOfWholeTransferDeadline(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		flusher := writer.(http.Flusher)
		for _, chunk := range []string{"a", "b", "c", "d"} {
			_, _ = writer.Write([]byte(chunk))
			flusher.Flush()
			time.Sleep(15 * time.Millisecond)
		}
	}))
	defer server.Close()

	client, err := remote.NewTurboClientForCompatibilityWithTimeouts(
		server.URL, "team-token", "linux-amd64-schema1",
		remote.Timeouts{Metadata: 25 * time.Millisecond, TransferIdle: 40 * time.Millisecond},
	)
	if err != nil {
		t.Fatal(err)
	}
	download, err := client.Get(context.Background(), "task-hash")
	if err != nil {
		t.Fatal(err)
	}
	defer download.Body.Close()
	body, err := io.ReadAll(download.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "abcd" {
		t.Fatalf("body = %q, want abcd", body)
	}
}

func TestTurboClientStopsStalledTransferAtIdleDeadline(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("a"))
		writer.(http.Flusher).Flush()
		time.Sleep(200 * time.Millisecond)
		_, _ = writer.Write([]byte("b"))
	}))
	defer server.Close()

	client, err := remote.NewTurboClientForCompatibilityWithTimeouts(
		server.URL, "team-token", "linux-amd64-schema1",
		remote.Timeouts{Metadata: 25 * time.Millisecond, TransferIdle: 30 * time.Millisecond},
	)
	if err != nil {
		t.Fatal(err)
	}
	download, err := client.Get(context.Background(), "task-hash")
	if err != nil {
		t.Fatal(err)
	}
	defer download.Body.Close()
	if _, err := io.ReadAll(download.Body); !errors.Is(err, remote.ErrTransferIdle) {
		t.Fatalf("stalled transfer error = %v, want ErrTransferIdle", err)
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
