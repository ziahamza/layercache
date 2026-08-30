package actionscache_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/actionscache"
)

func TestRemoteStorageSpeaksV1WithBearerAuthentication(t *testing.T) {
	t.Parallel()

	const token = "team-cache-token"
	scope := actionscache.Scope{
		Repository:    "acme/widgets",
		Ref:           "refs/heads/feature",
		DefaultRef:    "refs/heads/main",
		Compatibility: "linux-x64-node24",
	}
	backend := actionscache.NewMemoryStorage()
	putArchive(t, backend, scope, "pnpm-restore-old", "v1", []byte("old"))
	putArchive(t, backend, scope, "pnpm-restore-new", "v1", []byte("new"))

	handler, err := actionscache.NewHandler(actionscache.Config{
		Repository:     scope.Repository,
		Ref:            scope.Ref,
		DefaultRef:     scope.DefaultRef,
		Compatibility:  scope.Compatibility,
		ArchiveBaseURL: "",
	}, backend)
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	var mu sync.Mutex
	var authenticatedRequests int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if handler.AuthorizesArchiveDownload(request) {
			if got := request.Header.Get("Authorization"); got != "" {
				http.Error(writer, "archive bearer token must not be sent", http.StatusBadRequest)
				return
			}
			if got := request.Header.Get("X-LayerCache-Compatibility"); got != "" {
				http.Error(writer, "archive compatibility header must not be sent", http.StatusBadRequest)
				return
			}
			handler.ServeHTTP(writer, request)
			return
		}
		if got := request.Header.Get("Authorization"); got != "Bearer "+token {
			http.Error(writer, "missing bearer token", http.StatusUnauthorized)
			return
		}
		if got := request.Header.Get("X-LayerCache-Compatibility"); got != scope.Compatibility {
			http.Error(writer, "missing compatibility identity", http.StatusBadRequest)
			return
		}
		mu.Lock()
		authenticatedRequests++
		mu.Unlock()
		handler.ServeHTTP(writer, request)
	}))
	t.Cleanup(server.Close)

	remote, err := actionscache.NewRemoteStorage(actionscache.RemoteStorageConfig{
		Endpoint:            server.URL,
		Token:               token,
		TransferIdleTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("new remote storage: %v", err)
	}

	result, err := remote.Lookup(context.Background(), actionscache.LookupRequest{
		Scope: scope, Keys: []string{"missing", "pnpm-restore-"}, Version: "v1",
	})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if result.Entry.Key != "pnpm-restore-new" || result.Match != actionscache.MatchPrefix || result.RequestedKey != "pnpm-restore-" {
		t.Fatalf("lookup result = %#v, want newest prefix for the second ordered key", result)
	}
	if result.RefScope != actionscache.RefScopeCurrent {
		t.Fatalf("ref scope = %q, want current", result.RefScope)
	}
	if result.Source != actionscache.SourceTeamCache {
		t.Fatalf("source = %q, want Team Cache", result.Source)
	}
	wrongScope := scope
	wrongScope.Compatibility = "darwin-arm64-node24"
	if _, err := remote.Open(context.Background(), actionscache.OpenRequest{
		Scope: wrongScope, ID: result.Entry.ID,
	}); !errors.Is(err, actionscache.ErrNotFound) {
		t.Fatalf("cross-compatibility open = %v, want not found", err)
	}

	archive, err := remote.Open(context.Background(), actionscache.OpenRequest{Scope: scope, ID: result.Entry.ID})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	contents, readErr := io.ReadAll(archive.Body)
	closeErr := archive.Body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read archive: read=%v close=%v", readErr, closeErr)
	}
	if !bytes.Equal(contents, []byte("new")) {
		t.Fatalf("archive = %q, want new", contents)
	}

	reservation, err := remote.Reserve(context.Background(), actionscache.ReserveRequest{
		Scope: scope, Key: "pnpm-created-remotely", Version: "v1", CacheSize: sizePointer(6),
	})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := remote.Upload(context.Background(), actionscache.UploadRequest{
		ReservationID: reservation.ID, Start: 0, End: 5, Body: bytes.NewReader([]byte("remote")),
	}); err != nil {
		t.Fatalf("upload: %v", err)
	}
	entry, err := remote.Commit(context.Background(), actionscache.CommitRequest{ReservationID: reservation.ID, Size: 6})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if entry.Key != "pnpm-created-remotely" || entry.Size != 6 {
		t.Fatalf("committed entry = %#v", entry)
	}
	remoteCreatedArchive, err := remote.Open(context.Background(), actionscache.OpenRequest{Scope: scope, ID: entry.ID})
	if err != nil {
		t.Fatalf("open newly committed remote entry: %v", err)
	}
	remoteCreatedContents, err := io.ReadAll(remoteCreatedArchive.Body)
	remoteCreatedArchive.Body.Close()
	if err != nil || !bytes.Equal(remoteCreatedContents, []byte("remote")) {
		t.Fatalf("newly committed remote archive = %q, err = %v", remoteCreatedContents, err)
	}

	created, err := backend.Lookup(context.Background(), actionscache.LookupRequest{
		Scope: scope, Keys: []string{"pnpm-created-remotely"}, Version: "v1",
	})
	if err != nil {
		t.Fatalf("lookup backend after remote commit: %v", err)
	}
	createdArchive, err := backend.Open(context.Background(), actionscache.OpenRequest{Scope: scope, ID: created.Entry.ID})
	if err != nil {
		t.Fatalf("open backend after remote commit: %v", err)
	}
	createdContents, err := io.ReadAll(createdArchive.Body)
	createdArchive.Body.Close()
	if err != nil || !bytes.Equal(createdContents, []byte("remote")) {
		t.Fatalf("created archive = %q, err = %v", createdContents, err)
	}

	mu.Lock()
	requestCount := authenticatedRequests
	mu.Unlock()
	if requestCount != 5 {
		t.Fatalf("authenticated requests = %d, want two lookups, reserve, upload, and commit", requestCount)
	}
}

func putArchive(t *testing.T, storage actionscache.StorageIndex, scope actionscache.Scope, key, version string, body []byte) actionscache.Entry {
	t.Helper()
	size := int64(len(body))
	reservation, err := storage.Reserve(context.Background(), actionscache.ReserveRequest{
		Scope: scope, Key: key, Version: version, CacheSize: &size,
	})
	if err != nil {
		t.Fatalf("reserve %q: %v", key, err)
	}
	if len(body) > 0 {
		if err := storage.Upload(context.Background(), actionscache.UploadRequest{
			ReservationID: reservation.ID,
			Start:         0,
			End:           int64(len(body) - 1),
			Body:          bytes.NewReader(body),
		}); err != nil {
			t.Fatalf("upload %q: %v", key, err)
		}
	}
	entry, err := storage.Commit(context.Background(), actionscache.CommitRequest{ReservationID: reservation.ID, Size: size})
	if err != nil {
		t.Fatalf("commit %q: %v", key, err)
	}
	return entry
}

func sizePointer(size int64) *int64 {
	return &size
}

func TestRemoteStorageMapsV1MissAndConflict(t *testing.T) {
	t.Parallel()

	scope := actionscache.Scope{Repository: "acme/widgets", Ref: "refs/heads/main", DefaultRef: "refs/heads/main", Compatibility: "linux-x64"}
	backend := actionscache.NewMemoryStorage()
	handler, err := actionscache.NewHandler(actionscache.Config{
		Repository: scope.Repository, Ref: scope.Ref, DefaultRef: scope.DefaultRef, Compatibility: scope.Compatibility,
	}, backend)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	remote, err := actionscache.NewRemoteStorage(actionscache.RemoteStorageConfig{Endpoint: server.URL, Token: "test-token", TransferIdleTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}

	_, err = remote.Lookup(context.Background(), actionscache.LookupRequest{Scope: scope, Keys: []string{"absent"}, Version: "v1"})
	if err != actionscache.ErrNotFound {
		t.Fatalf("lookup error = %v, want ErrNotFound", err)
	}

	request := actionscache.ReserveRequest{Scope: scope, Key: "winner", Version: "v1", CacheSize: sizePointer(1)}
	if _, err := remote.Reserve(context.Background(), request); err != nil {
		t.Fatalf("first reserve: %v", err)
	}
	if _, err := remote.Reserve(context.Background(), request); err != actionscache.ErrAlreadyExists {
		t.Fatalf("second reserve error = %v, want ErrAlreadyExists", err)
	}
}

func TestRemoteStorageRejectsInvalidEndpoint(t *testing.T) {
	t.Parallel()

	for _, endpoint := range []string{"", "://not-a-url", "https://example.com/cache?token=secret"} {
		t.Run(fmt.Sprintf("%q", endpoint), func(t *testing.T) {
			if _, err := actionscache.NewRemoteStorage(actionscache.RemoteStorageConfig{Endpoint: endpoint, Token: "test-token"}); err == nil {
				t.Fatalf("NewRemoteStorage(%q) succeeded, want an error", endpoint)
			}
		})
	}
}

func TestRemoteStorageUsesResponseHeaderDeadline(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		time.Sleep(150 * time.Millisecond)
		writer.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	remote, err := actionscache.NewRemoteStorage(actionscache.RemoteStorageConfig{
		Endpoint: server.URL, Token: "team-token", ResponseHeaderTimeout: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = remote.Lookup(context.Background(), actionscache.LookupRequest{Keys: []string{"key"}, Version: "v1"})
	if err == nil {
		t.Fatal("lookup succeeded after the response-header deadline")
	}
	if elapsed := time.Since(started); elapsed > 130*time.Millisecond {
		t.Fatalf("lookup returned after %s, want response-header deadline near 50ms", elapsed)
	}
}

func TestRemoteStorageDerivesMatchMetadataFromStockV1Response(t *testing.T) {
	t.Parallel()

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("keys") != "primary,restore-" {
			http.Error(writer, "ordered keys were changed", http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(writer, `{"cacheKey":"restore-hit","scope":"refs/heads/main","cacheVersion":"v1","creationTime":"2026-08-30T00:00:00Z","archiveLocation":%q}`,
			server.URL+"/_apis/artifactcache/caches/7/archive?download=opaque")
	}))
	t.Cleanup(server.Close)
	remote, err := actionscache.NewRemoteStorage(actionscache.RemoteStorageConfig{
		Endpoint: server.URL, Token: "team-token", TransferIdleTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := remote.Lookup(context.Background(), actionscache.LookupRequest{
		Scope: actionscache.Scope{
			Ref: "refs/heads/feature", DefaultRef: "refs/heads/main", Compatibility: "linux-x64",
		},
		Keys: []string{"primary", "restore-"}, Version: "v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Match != actionscache.MatchPrefix || result.RequestedKey != "restore-" || result.RefScope != actionscache.RefScopeDefault {
		t.Fatalf("derived match metadata = %#v", result)
	}
}

func TestRemoteStorageRejectsArchiveLocationForDifferentCompatibility(t *testing.T) {
	t.Parallel()

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(writer, `{"cacheKey":"key","scope":"refs/heads/main","cacheVersion":"v1","creationTime":"2026-08-30T00:00:00Z","archiveLocation":%q}`,
			server.URL+"/_layercache/compatibility/darwin-arm64-schema1/_apis/artifactcache/caches/7/archive")
	}))
	t.Cleanup(server.Close)
	remote, err := actionscache.NewRemoteStorage(actionscache.RemoteStorageConfig{
		Endpoint: server.URL, Token: "team-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = remote.Lookup(context.Background(), actionscache.LookupRequest{
		Scope: actionscache.Scope{
			Repository: "acme/widget", Ref: "refs/heads/main", DefaultRef: "refs/heads/main",
			Compatibility: "linux-amd64-schema1",
		},
		Keys: []string{"key"}, Version: "v1",
	})
	if err == nil || !strings.Contains(err.Error(), "different compatibility") {
		t.Fatalf("lookup error = %v, want compatibility rejection", err)
	}
}

func TestRemoteStorageTransferTimeoutTracksProgressRatherThanTotalDuration(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Length", "5")
		flusher, ok := writer.(http.Flusher)
		if !ok {
			t.Fatal("test server does not support flushing")
		}
		for _, value := range []byte("alive") {
			_, _ = writer.Write([]byte{value})
			flusher.Flush()
			if request.URL.Path == "/_apis/artifactcache/caches/2/archive" {
				time.Sleep(120 * time.Millisecond)
			} else {
				time.Sleep(20 * time.Millisecond)
			}
		}
	}))
	t.Cleanup(server.Close)
	remote, err := actionscache.NewRemoteStorage(actionscache.RemoteStorageConfig{
		Endpoint: server.URL, Token: "team-token", TransferIdleTimeout: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	scope := actionscache.Scope{Repository: "acme/widgets", Ref: "refs/heads/main", DefaultRef: "refs/heads/main", Compatibility: "linux-x64"}

	archive, err := remote.Open(context.Background(), actionscache.OpenRequest{Scope: scope, ID: 1})
	if err != nil {
		t.Fatalf("open progressing transfer: %v", err)
	}
	contents, readErr := io.ReadAll(archive.Body)
	archive.Body.Close()
	if readErr != nil || string(contents) != "alive" {
		t.Fatalf("progressing transfer = %q, err = %v", contents, readErr)
	}

	archive, err = remote.Open(context.Background(), actionscache.OpenRequest{Scope: scope, ID: 2})
	if err != nil {
		t.Fatalf("open stalled transfer: %v", err)
	}
	_, readErr = io.ReadAll(archive.Body)
	archive.Body.Close()
	if !errors.Is(readErr, actionscache.ErrTransferIdleTimeout) {
		t.Fatalf("stalled transfer error = %v, want ErrTransferIdleTimeout", readErr)
	}
}
