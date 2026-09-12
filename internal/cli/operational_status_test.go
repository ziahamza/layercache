package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/config"
)

func TestTeamRemoteProbeRequiresAuthenticatedStatus(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/status" {
			t.Errorf("probe path = %q, want /v1/status", request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		if request.Header.Get("Authorization") != "Bearer valid-team-token" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"running":true}`))
	}))
	t.Cleanup(server.Close)

	wrong := probeTeamRemote(context.Background(), server.URL, "revoked-team-token")
	if wrong.Reachable || wrong.State != "unauthorized" {
		t.Fatalf("revoked Team credential probe = %#v", wrong)
	}
	valid := probeTeamRemote(context.Background(), server.URL, "valid-team-token")
	if !valid.Reachable || valid.State != "reachable" {
		t.Fatalf("valid Team credential probe = %#v", valid)
	}
}

func TestPublicBuildCapabilityProbeIsAuthenticatedAndNonMutating(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/v1/public-builds/public-build-0" {
			t.Errorf("probe = %s %s", request.Method, request.URL.Path)
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		if request.Header.Get("Authorization") != "Bearer valid-public-token" {
			writer.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(writer).Encode(map[string]string{"error": "unauthorized"})
			return
		}
		writer.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(writer).Encode(map[string]string{"error": "Public Build not found"})
	}))
	t.Cleanup(server.Close)

	wrong := probePublicBuildCapability(context.Background(), server.URL, "wrong-public-token")
	if wrong.Reachable || wrong.State != "unauthorized" {
		t.Fatalf("wrong Public Build capability probe = %#v", wrong)
	}
	valid := probePublicBuildCapability(context.Background(), server.URL, "valid-public-token")
	if !valid.Reachable || valid.State != "reachable" {
		t.Fatalf("valid Public Build capability probe = %#v", valid)
	}
}

func TestPublicCacheEndpointDoesNotImplicitlyEnablePublicBuild(t *testing.T) {
	t.Parallel()

	cfg := config.Config{Role: "local", PublicURL: "https://public.example"}
	token, expiry, required := publicBuildStatusCredential(cfg)
	if token != "" || !expiry.IsZero() || required {
		t.Fatalf("Public Cache-only credential = token %q, expiry %v, required %t", token, expiry, required)
	}
	condition := inspectPublicBuildStatus(
		context.Background(), cfg,
		remoteCondition{Configured: true, Reachable: true, State: "reachable"}, nil,
		token, expiry, required,
	)
	if !condition.EndpointConfigured || condition.Enabled || condition.CredentialRequired || condition.State != "not-configured" {
		t.Fatalf("Public Cache-only Public Build condition = %#v", condition)
	}
}

func TestPublicBuildDoesNotReuseTeamCredentialAcrossEndpoints(t *testing.T) {
	t.Parallel()

	cfg := config.Config{
		Role: "local", TeamURL: "https://team.example", TeamToken: "team-secret",
		PublicURL: "https://public.example",
	}
	if token := publicBuildClientToken(cfg); token != "" {
		t.Fatalf("distinct Public Build endpoint selected Team credential %q", token)
	}
	if _, _, required := publicBuildStatusCredential(cfg); required {
		t.Fatal("distinct Public Cache endpoint without a Public Build credential was marked credential-required")
	}

	cfg.PublicURL = cfg.TeamURL
	token, _, required := publicBuildStatusCredential(cfg)
	if token != cfg.TeamToken || !required || publicBuildClientToken(cfg) != cfg.TeamToken {
		t.Fatalf("shared endpoint credential = token %q, required %t", token, required)
	}
}

func TestOperationalStatusHumanOutputShowsCredentialStateAndExpiry(t *testing.T) {
	t.Parallel()

	expiresAt := time.Date(2026, time.September, 1, 12, 30, 0, 0, time.UTC)
	status := operationalStatus{
		Role: "local", ProjectID: "github.com/acme/widgets", MaxBytes: 1 << 30,
		RemoteReachability: map[string]remoteCondition{},
		Credentials: map[string]credentialCondition{
			"team": {
				Configured: true, State: "valid", ExpiresAt: &expiresAt,
			},
			"publicBuild": {Configured: true, State: "missing"},
		},
		PublicBuild: publicBuildCondition{State: "unavailable"},
	}
	var output bytes.Buffer
	if err := printOperationalStatus(&output, status); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Team Cache credential: valid; expires 2026-09-01T12:30:00Z",
		"Public Build credential: missing",
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("status output lacks %q:\n%s", want, output.String())
		}
	}
}
