package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/layercache/layercache/internal/actionscache"
	"github.com/layercache/layercache/internal/artifact"
	"github.com/layercache/layercache/internal/compatibility"
	"github.com/layercache/layercache/internal/config"
	"github.com/layercache/layercache/internal/remote"
	"github.com/layercache/layercache/internal/server"
)

func TestTeamTurboUsesAuthenticatedClientCompatibility(t *testing.T) {
	t.Parallel()

	endpoint, cfg := newTeamServer(t)
	linux, err := remote.NewTurboClientForCompatibility(endpoint.URL, cfg.LocalToken, "linux-amd64-schema1")
	if err != nil {
		t.Fatal(err)
	}
	mac, err := remote.NewTurboClientForCompatibility(endpoint.URL, cfg.LocalToken, "darwin-arm64-schema1")
	if err != nil {
		t.Fatal(err)
	}

	payload := []byte("linux output")
	if err := linux.Put(context.Background(), "same-turbo-hash", turboEntry(payload), bytes.NewReader(payload)); err != nil {
		t.Fatalf("publish Linux artifact: %v", err)
	}
	if _, err := mac.Get(context.Background(), "same-turbo-hash"); err != remote.ErrMiss {
		t.Fatalf("macOS lookup = %v, want a compatibility miss", err)
	}

	missingSelector := authenticatedRequest(t, cfg.LocalToken, http.MethodPut,
		endpoint.URL+"/v8/artifacts/server-host-only", strings.NewReader("must not be stored"))
	response, err := http.DefaultClient.Do(missingSelector)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing selector status = %d, want 400", response.StatusCode)
	}

	hostLookup := authenticatedRequest(t, cfg.LocalToken, http.MethodGet,
		endpoint.URL+"/v8/artifacts/server-host-only", nil)
	hostLookup.Header.Set(compatibility.Header, cfg.CompatibilityID)
	response, err = http.DefaultClient.Do(hostLookup)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("server-host namespace lookup status = %d, want 404", response.StatusCode)
	}
}

func TestTeamActionsUsesAuthenticatedClientCompatibility(t *testing.T) {
	t.Parallel()

	endpoint, cfg := newTeamServer(t)
	linux := newActionsRemote(t, endpoint.URL, cfg.LocalToken)
	mac := newActionsRemote(t, endpoint.URL, cfg.LocalToken)
	linuxScope := actionsScope("linux-amd64-schema1")
	macScope := actionsScope("darwin-arm64-schema1")

	payload := []byte("linux action output")
	size := int64(len(payload))
	reservation, err := linux.Reserve(context.Background(), actionscache.ReserveRequest{
		Scope: linuxScope, Key: "same-actions-key", Version: "v1", CacheSize: &size,
	})
	if err != nil {
		t.Fatalf("reserve Linux artifact: %v", err)
	}
	if err := linux.Upload(context.Background(), actionscache.UploadRequest{
		ReservationID: reservation.ID, Start: 0, End: size - 1, Body: bytes.NewReader(payload),
	}); err != nil {
		t.Fatalf("upload Linux artifact: %v", err)
	}
	if _, err := linux.Commit(context.Background(), actionscache.CommitRequest{
		ReservationID: reservation.ID, Size: size,
	}); err != nil {
		t.Fatalf("commit Linux artifact: %v", err)
	}
	linuxResult, err := linux.Lookup(context.Background(), actionscache.LookupRequest{
		Scope: linuxScope, Keys: []string{"same-actions-key"}, Version: "v1",
	})
	if err != nil {
		t.Fatalf("lookup Linux artifact: %v", err)
	}
	archive, err := linux.Open(context.Background(), actionscache.OpenRequest{Scope: linuxScope, ID: linuxResult.Entry.ID})
	if err != nil {
		t.Fatalf("stock signed-URL download without request headers: %v", err)
	}
	restored, readErr := io.ReadAll(archive.Body)
	archive.Body.Close()
	if readErr != nil || !bytes.Equal(restored, payload) {
		t.Fatalf("restored archive = %q, err = %v", restored, readErr)
	}

	lookup := authenticatedRequest(t, cfg.LocalToken, http.MethodGet,
		endpoint.URL+"/_layercache/compatibility/"+linuxScope.Compatibility+
			"/_apis/artifactcache/cache?keys=same-actions-key&version=v1", nil)
	response, err := http.DefaultClient.Do(lookup)
	if err != nil {
		t.Fatal(err)
	}
	var lookupBody struct {
		ArchiveLocation string `json:"archiveLocation"`
	}
	if err := json.NewDecoder(response.Body).Decode(&lookupBody); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	tamperedLocation := strings.Replace(lookupBody.ArchiveLocation,
		"/linux-amd64-schema1/", "/darwin-arm64-schema1/", 1)
	tampered, err := http.Get(tamperedLocation)
	if err != nil {
		t.Fatal(err)
	}
	tampered.Body.Close()
	if tampered.StatusCode != http.StatusUnauthorized && tampered.StatusCode != http.StatusNotFound {
		t.Fatalf("tampered compatibility URL status = %d, want rejected capability", tampered.StatusCode)
	}

	_, err = mac.Lookup(context.Background(), actionscache.LookupRequest{
		Scope: macScope, Keys: []string{"same-actions-key"}, Version: "v1",
	})
	if err != actionscache.ErrNotFound {
		t.Fatalf("macOS lookup = %v, want a compatibility miss", err)
	}

	missingSelector := authenticatedRequest(t, cfg.LocalToken, http.MethodGet,
		endpoint.URL+"/_apis/artifactcache/cache?keys=same-actions-key&version=v1", nil)
	response, err = http.DefaultClient.Do(missingSelector)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("missing selector status = %d, want 400: %s", response.StatusCode, body)
	}
}

func TestTeamCompatibilitySelectorDoesNotAuthorizeRequests(t *testing.T) {
	t.Parallel()

	endpoint, _ := newTeamServer(t)
	request, err := http.NewRequest(http.MethodGet, endpoint.URL+"/v8/artifacts/private", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(compatibility.Header, "linux-amd64-schema1")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("selector-only status = %d, want 401", response.StatusCode)
	}
}

func newTeamServer(t *testing.T) (*httptest.Server, config.Config) {
	t.Helper()
	cfg := config.Config{
		Version: 1, Role: "team", DataDir: t.TempDir(), Listen: "127.0.0.1:7437",
		MaxBytes: 1 << 20, ProjectID: "github.com/acme/widget", LocalToken: "team-token",
		CompatibilityID: "server-host-linux-amd64-schema1", ActionsRepository: "acme/widget",
		ActionsRef: "refs/heads/main", ActionsDefaultRef: "refs/heads/main",
		BuildkitBuilder: "layercache-test",
	}
	instance, err := server.New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instance.Close() })
	endpoint := httptest.NewServer(instance.Handler())
	t.Cleanup(endpoint.Close)
	return endpoint, cfg
}

func newActionsRemote(t *testing.T, endpoint, token string) *actionscache.RemoteStorage {
	t.Helper()
	storage, err := actionscache.NewRemoteStorage(actionscache.RemoteStorageConfig{
		Endpoint: endpoint, Token: token,
	})
	if err != nil {
		t.Fatal(err)
	}
	return storage
}

func actionsScope(identity string) actionscache.Scope {
	return actionscache.Scope{
		Repository: "acme/widget", Ref: "refs/heads/main", DefaultRef: "refs/heads/main",
		Compatibility: identity,
	}
}

func authenticatedRequest(t *testing.T, token, method, target string, body io.Reader) *http.Request {
	t.Helper()
	request, err := http.NewRequest(method, target, body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	return request
}

func turboEntry(payload []byte) artifact.Entry {
	return artifact.Entry{Size: int64(len(payload))}
}
