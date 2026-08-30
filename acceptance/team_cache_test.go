package acceptance_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func TestTurboTeamCacheWarmsFreshHost(t *testing.T) {
	root := t.TempDir()
	binary := buildLayerCache(t)
	teamAddress := availableAddress(t)
	teamConfig := filepath.Join(root, "team.json")
	runLayerCache(t,
		"setup", "--config", teamConfig,
		"--data-dir", filepath.Join(root, "team-cache"),
		"--listen", teamAddress,
		"--role", "team",
		"--project", "project-1",
		"--local-token", "team-secret",
		"--max-size", "10485760",
		"--non-interactive", "--json",
	)
	team := startLayerCache(t, binary, teamConfig, teamAddress)
	defer team.stop(t)

	hostAAddress := availableAddress(t)
	hostAConfig := filepath.Join(root, "host-a.json")
	runLayerCache(t,
		"setup", "--config", hostAConfig,
		"--data-dir", filepath.Join(root, "host-a-cache"),
		"--listen", hostAAddress,
		"--project", "project-1",
		"--team-url", "http://"+teamAddress,
		"--team-token", "team-secret",
		"--max-size", "10485760",
		"--non-interactive", "--json",
	)
	var hostA turboConnection
	if err := json.Unmarshal(runLayerCache(t, "integration", "turbo", "--config", hostAConfig, "--json"), &hostA); err != nil {
		t.Fatal(err)
	}
	hostAServer := startLayerCache(t, binary, hostAConfig, hostAAddress)

	want := []byte("artifact-built-on-host-a")
	artifactPath := "/v8/artifacts/team-shared-hash"
	request, err := http.NewRequest(http.MethodPut, hostA.APIURL+artifactPath, bytes.NewReader(want))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+hostA.Token)
	request.Header.Set("x-artifact-duration", "9000")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("host A PUT status = %d, want 200", response.StatusCode)
	}
	hostAServer.stop(t)

	hostBAddress := availableAddress(t)
	hostBConfig := filepath.Join(root, "host-b.json")
	runLayerCache(t,
		"setup", "--config", hostBConfig,
		"--data-dir", filepath.Join(root, "host-b-cache"),
		"--listen", hostBAddress,
		"--project", "project-1",
		"--team-url", "http://"+teamAddress,
		"--team-token", "team-secret",
		"--max-size", "10485760",
		"--non-interactive", "--json",
	)
	var hostB turboConnection
	if err := json.Unmarshal(runLayerCache(t, "integration", "turbo", "--config", hostBConfig, "--json"), &hostB); err != nil {
		t.Fatal(err)
	}
	hostBServer := startLayerCache(t, binary, hostBConfig, hostBAddress)
	defer hostBServer.stop(t)

	got, source := fetchTurboArtifact(t, hostB.APIURL+artifactPath, hostB.Token)
	if !bytes.Equal(got, want) {
		t.Fatalf("Team Cache bytes = %q, want %q", got, want)
	}
	if source != "team" {
		t.Fatalf("fresh host source = %q, want team", source)
	}

	team.stop(t)
	got, source = fetchTurboArtifact(t, hostB.APIURL+artifactPath, hostB.Token)
	if !bytes.Equal(got, want) {
		t.Fatalf("warmed Local Cache bytes = %q, want %q", got, want)
	}
	if source != "local" {
		t.Fatalf("offline source = %q, want local", source)
	}
}

func TestFailedTurboTeamPublicationRetriesAfterRuntimeRestart(t *testing.T) {
	root := t.TempDir()
	binary := buildLayerCache(t)
	teamAddress := availableAddress(t)
	hostAddress := availableAddress(t)
	hostConfig := filepath.Join(root, "host.json")
	runLayerCache(t,
		"setup", "--config", hostConfig,
		"--data-dir", filepath.Join(root, "host-cache"),
		"--listen", hostAddress, "--project", "retry-project",
		"--compatibility-id", "acceptance-retry-schema1",
		"--team-url", "http://"+teamAddress, "--team-token", "retry-team-secret",
		"--max-size", "10485760", "--non-interactive", "--json",
	)
	hostConnection := turboConnection{}
	if err := json.Unmarshal(runLayerCache(t, "integration", "turbo", "--config", hostConfig, "--json"), &hostConnection); err != nil {
		t.Fatal(err)
	}
	host := startLayerCache(t, binary, hostConfig, hostAddress)
	want := []byte("durably-queued-team-artifact")
	request, err := http.NewRequest(http.MethodPut, hostConnection.APIURL+"/v8/artifacts/retry-after-restart", bytes.NewReader(want))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+hostConnection.Token)
	request.Header.Set("x-artifact-duration", "1200")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("x-layercache-degraded") != "team-publication-failed" {
		t.Fatalf("offline Team publication = HTTP %d degraded %q", response.StatusCode, response.Header.Get("x-layercache-degraded"))
	}
	gcRequest, err := http.NewRequest(http.MethodPost, "http://"+hostAddress+"/v1/gc", bytes.NewReader([]byte(`{"targetBytes":0}`)))
	if err != nil {
		t.Fatal(err)
	}
	gcRequest.Header.Set("Authorization", "Bearer "+hostConnection.Token)
	gcResponse, err := http.DefaultClient.Do(gcRequest)
	if err != nil {
		t.Fatal(err)
	}
	gcResponse.Body.Close()
	if gcResponse.StatusCode != http.StatusInternalServerError {
		t.Fatalf("GC with queued Team upload status = %d, want 500 while its artifact is pinned", gcResponse.StatusCode)
	}
	got, source := fetchTurboArtifact(t, hostConnection.APIURL+"/v8/artifacts/retry-after-restart", hostConnection.Token)
	if !bytes.Equal(got, want) || source != "local" {
		t.Fatalf("queued Local Cache artifact after GC = %q from %q, want %q from local", got, source, want)
	}
	host.stop(t)

	teamConfig := filepath.Join(root, "team.json")
	runLayerCache(t,
		"setup", "--config", teamConfig,
		"--data-dir", filepath.Join(root, "team-cache"),
		"--listen", teamAddress, "--role", "team", "--project", "retry-project",
		"--local-token", "retry-team-secret", "--max-size", "10485760",
		"--non-interactive", "--json",
	)
	team := startLayerCache(t, binary, teamConfig, teamAddress)
	defer team.stop(t)
	host = startLayerCache(t, binary, hostConfig, hostAddress)
	defer host.stop(t)

	deadline := time.Now().Add(5 * time.Second)
	for {
		request, err = http.NewRequest(http.MethodGet, "http://"+teamAddress+"/v8/artifacts/retry-after-restart", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer retry-team-secret")
		request.Header.Set("X-LayerCache-Compatibility", "acceptance-retry-schema1")
		response, err = http.DefaultClient.Do(request)
		if err == nil && response.StatusCode == http.StatusOK {
			got, readErr := io.ReadAll(response.Body)
			response.Body.Close()
			if readErr != nil || !bytes.Equal(got, want) {
				t.Fatalf("retried Team artifact = %q, error = %v", got, readErr)
			}
			break
		}
		if response != nil {
			response.Body.Close()
		}
		if time.Now().After(deadline) {
			t.Fatal("queued Team publication was not retried after restart")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func fetchTurboArtifact(t *testing.T, url, token string) ([]byte, string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	contents, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", url, response.StatusCode, contents)
	}
	return contents, response.Header.Get("x-layercache-source")
}
