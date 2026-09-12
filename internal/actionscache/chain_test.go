package actionscache_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/actionscache"
	"github.com/layercache/layercache/internal/artifact"
	"github.com/layercache/layercache/internal/measurement"
)

func TestCacheChainPrefersStrongerTeamMatchAndWarmsLocalBeforeServing(t *testing.T) {
	t.Parallel()

	scope := actionscache.Scope{
		Repository: "acme/widgets", Ref: "refs/heads/feature", DefaultRef: "refs/heads/main", Compatibility: "linux-x64-node24",
	}
	local := actionscache.NewMemoryStorage()
	putArchive(t, local, scope, "pnpm-lock-old", "v1", []byte("local prefix"))
	teamBackend := actionscache.NewMemoryStorage()
	putArchive(t, teamBackend, scope, "pnpm-lock", "v1", []byte("team exact"))
	team, closeTeam := newAuthenticatedRemoteStorage(t, teamBackend, scope)

	chain, err := actionscache.NewCacheChain(local, team)
	if err != nil {
		t.Fatalf("new chain storage: %v", err)
	}
	request := actionscache.LookupRequest{Scope: scope, Keys: []string{"pnpm-lock"}, Version: "v1"}
	result, err := chain.Lookup(context.Background(), request)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if result.Entry.Key != "pnpm-lock" || result.Match != actionscache.MatchExact {
		t.Fatalf("result = %#v, want Team Cache exact match over Local Cache prefix", result)
	}
	if result.Source != actionscache.SourceTeamCache {
		t.Fatalf("source = %q, want Team Cache", result.Source)
	}

	// Lookup must finish warming before it exposes an archive ID. Once it has
	// returned, the Team Cache can disappear and Open still succeeds locally.
	closeTeam()
	archive, err := chain.Open(context.Background(), actionscache.OpenRequest{Scope: scope, ID: result.Entry.ID})
	if err != nil {
		t.Fatalf("open warmed archive after Team Cache shutdown: %v", err)
	}
	contents, readErr := io.ReadAll(archive.Body)
	archive.Body.Close()
	if readErr != nil || !bytes.Equal(contents, []byte("team exact")) {
		t.Fatalf("warmed archive = %q, err = %v", contents, readErr)
	}

	warmed, err := chain.Lookup(context.Background(), request)
	if err != nil {
		t.Fatalf("lookup warmed Local Cache: %v", err)
	}
	if warmed.Source != actionscache.SourceLocalCache || warmed.Entry.Key != "pnpm-lock" {
		t.Fatalf("warmed lookup = %#v, want Local Cache exact hit", warmed)
	}
}

func TestCacheChainPublishesCommittedLocalArchiveToTeam(t *testing.T) {
	t.Parallel()

	scope := actionscache.Scope{
		Repository: "acme/widgets", Ref: "refs/heads/feature", DefaultRef: "refs/heads/main", Compatibility: "linux-x64-node24",
	}
	local := actionscache.NewMemoryStorage()
	team := actionscache.NewMemoryStorage()
	chain, err := actionscache.NewCacheChain(local, team)
	if err != nil {
		t.Fatal(err)
	}

	entry := putArchive(t, chain, scope, "shared-build", "v1", []byte("published bytes"))
	if entry.Key != "shared-build" {
		t.Fatalf("local commit = %#v", entry)
	}
	teamResult, err := team.Lookup(context.Background(), actionscache.LookupRequest{
		Scope: scope, Keys: []string{"shared-build"}, Version: "v1",
	})
	if err != nil {
		t.Fatalf("Team Cache lookup after local commit: %v", err)
	}
	teamArchive, err := team.Open(context.Background(), actionscache.OpenRequest{Scope: scope, ID: teamResult.Entry.ID})
	if err != nil {
		t.Fatalf("open Team Cache archive: %v", err)
	}
	contents, readErr := io.ReadAll(teamArchive.Body)
	teamArchive.Body.Close()
	if readErr != nil || !bytes.Equal(contents, []byte("published bytes")) {
		t.Fatalf("Team Cache archive = %q, err = %v", contents, readErr)
	}
}

func TestCacheChainKeepsSuccessfulLocalCommitWhenTeamPublicationFails(t *testing.T) {
	t.Parallel()

	scope := actionscache.Scope{
		Repository: "acme/widgets", Ref: "refs/heads/main", DefaultRef: "refs/heads/main", Compatibility: "linux-x64",
	}
	teamServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "Team Cache unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(teamServer.Close)
	team, err := actionscache.NewRemoteStorage(actionscache.RemoteStorageConfig{
		Endpoint: teamServer.URL, Token: "team-token", TransferIdleTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	local := actionscache.NewMemoryStorage()
	chain, err := actionscache.NewCacheChain(local, team)
	if err != nil {
		t.Fatal(err)
	}

	entry := putArchive(t, chain, scope, "survives-outage", "v1", []byte("local success"))
	result, err := local.Lookup(context.Background(), actionscache.LookupRequest{
		Scope: scope, Keys: []string{"survives-outage"}, Version: "v1",
	})
	if err != nil {
		t.Fatalf("Local Cache lookup after Team failure: %v", err)
	}
	if result.Entry.ID != entry.ID || result.Source != actionscache.SourceLocalCache {
		t.Fatalf("Local Cache result = %#v, local commit = %#v", result, entry)
	}
	archive, err := local.Open(context.Background(), actionscache.OpenRequest{Scope: scope, ID: entry.ID})
	if err != nil {
		t.Fatalf("open successful local commit: %v", err)
	}
	contents, readErr := io.ReadAll(archive.Body)
	archive.Body.Close()
	if readErr != nil || !bytes.Equal(contents, []byte("local success")) {
		t.Fatalf("local archive = %q, err = %v", contents, readErr)
	}
}

func TestNewCacheChainRequiresBothCaches(t *testing.T) {
	t.Parallel()

	local := actionscache.NewMemoryStorage()
	if _, err := actionscache.NewCacheChain(nil, local); err == nil {
		t.Fatal("nil Local Cache was accepted")
	}
	if _, err := actionscache.NewCacheChain(local, nil); err == nil {
		t.Fatal("nil Team Cache was accepted")
	}
}

func newAuthenticatedRemoteStorage(t *testing.T, backend actionscache.StorageIndex, scope actionscache.Scope) (*actionscache.RemoteStorage, func()) {
	t.Helper()
	const token = "team-token"
	handler, err := actionscache.NewHandler(actionscache.Config{
		Repository: scope.Repository, Ref: scope.Ref, DefaultRef: scope.DefaultRef, Compatibility: scope.Compatibility,
	}, backend)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !handler.AuthorizesArchiveDownload(request) && request.Header.Get("Authorization") != "Bearer "+token {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(writer, request)
	}))
	var closeOnce = make(chan struct{}, 1)
	closeServer := func() {
		select {
		case closeOnce <- struct{}{}:
			server.Close()
		default:
		}
	}
	t.Cleanup(closeServer)
	remote, err := actionscache.NewRemoteStorage(actionscache.RemoteStorageConfig{
		Endpoint: server.URL, Token: token, TransferIdleTimeout: time.Second,
	})
	if err != nil {
		closeServer()
		t.Fatal(err)
	}
	return remote, closeServer
}

func TestCacheChainReturnsLocalCandidateWhenTeamLookupFails(t *testing.T) {
	t.Parallel()

	scope := actionscache.Scope{Repository: "acme/widgets", Ref: "refs/heads/main", DefaultRef: "refs/heads/main", Compatibility: "linux-x64"}
	local := actionscache.NewMemoryStorage()
	putArchive(t, local, scope, "restore-local", "v1", []byte("local"))
	chain, err := actionscache.NewCacheChain(local, alwaysFailStorage{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := chain.Lookup(context.Background(), actionscache.LookupRequest{
		Scope: scope, Keys: []string{"restore-"}, Version: "v1",
	})
	if err != nil {
		t.Fatalf("lookup with Team Cache failure: %v", err)
	}
	if result.Entry.Key != "restore-local" || result.Source != actionscache.SourceLocalCache {
		t.Fatalf("result = %#v, want Local Cache fallback", result)
	}
	if !result.Degraded {
		t.Fatalf("result = %#v, want degraded Local Cache fallback", result)
	}
}

func TestActionsLookupTreatsTeamCacheOutageAsMiss(t *testing.T) {
	t.Parallel()

	scope := actionscache.Scope{
		Repository: "acme/widgets", Ref: "refs/heads/main",
		DefaultRef: "refs/heads/main", Compatibility: "linux-x64",
	}
	chain, err := actionscache.NewCacheChain(actionscache.NewMemoryStorage(), alwaysFailStorage{})
	if err != nil {
		t.Fatal(err)
	}
	var outcomes []measurement.FinalOutcome
	handler, err := actionscache.NewHandler(actionscache.Config{
		Repository: scope.Repository, Ref: scope.Ref, DefaultRef: scope.DefaultRef,
		Compatibility: scope.Compatibility,
		RecordOutcome: func(outcome measurement.FinalOutcome) error {
			outcomes = append(outcomes, outcome)
			return nil
		},
	}, chain)
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/_apis/artifactcache/cache?keys=missing&version=v1", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("Team Cache outage status = %d, want cache miss %d; body = %q", response.Code, http.StatusNoContent, response.Body.String())
	}
	if len(outcomes) != 1 || !outcomes[0].Degraded || outcomes[0].Result != measurement.ResultMiss {
		t.Fatalf("Team Cache outage outcomes = %#v, want one degraded miss", outcomes)
	}
}

func TestActionsLookupRecordsRemoteDegradationOnLocalFallback(t *testing.T) {
	t.Parallel()

	scope := actionscache.Scope{
		Repository: "acme/widgets", Ref: "refs/heads/main",
		DefaultRef: "refs/heads/main", Compatibility: "linux-x64",
	}
	local := actionscache.NewMemoryStorage()
	putArchive(t, local, scope, "restore-local", "v1", []byte("local"))
	chain, err := actionscache.NewCacheChain(local, alwaysFailStorage{})
	if err != nil {
		t.Fatal(err)
	}
	var outcomes []measurement.FinalOutcome
	handler, err := actionscache.NewHandler(actionscache.Config{
		Repository: scope.Repository, Ref: scope.Ref, DefaultRef: scope.DefaultRef,
		Compatibility: scope.Compatibility,
		RecordOutcome: func(outcome measurement.FinalOutcome) error {
			outcomes = append(outcomes, outcome)
			return nil
		},
	}, chain)
	if err != nil {
		t.Fatal(err)
	}

	lookupRequest := httptest.NewRequest(http.MethodGet, "/_apis/artifactcache/cache?keys=restore-&version=v1", nil)
	lookupResponse := httptest.NewRecorder()
	handler.ServeHTTP(lookupResponse, lookupRequest)
	var lookup struct {
		ArchiveLocation string `json:"archiveLocation"`
	}
	if lookupResponse.Code != http.StatusOK || json.Unmarshal(lookupResponse.Body.Bytes(), &lookup) != nil {
		t.Fatalf("Local fallback lookup = %d %s", lookupResponse.Code, lookupResponse.Body.String())
	}
	downloadResponse := httptest.NewRecorder()
	handler.ServeHTTP(downloadResponse, httptest.NewRequest(http.MethodGet, lookup.ArchiveLocation, nil))
	if downloadResponse.Code != http.StatusOK || downloadResponse.Body.String() != "local" {
		t.Fatalf("Local fallback download = %d %q", downloadResponse.Code, downloadResponse.Body.String())
	}
	if len(outcomes) != 1 || !outcomes[0].Degraded || outcomes[0].Result != measurement.ResultHit ||
		outcomes[0].Source != measurement.SourceLocalCache {
		t.Fatalf("Local fallback outcomes = %#v, want one degraded Local Cache hit", outcomes)
	}
}

func TestActionsLookupTreatsPublicCacheOutageAsMiss(t *testing.T) {
	t.Parallel()

	scope := actionscache.Scope{
		Repository: "acme/widgets", Ref: "refs/heads/main",
		DefaultRef: "refs/heads/main", Compatibility: "linux-x64",
	}
	chain, err := actionscache.NewCacheChainWithPublic(
		actionscache.NewMemoryStorage(), nil, alwaysFailPublicCache{},
	)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := actionscache.NewHandler(actionscache.Config{
		Repository: scope.Repository, Ref: scope.Ref, DefaultRef: scope.DefaultRef,
		Compatibility: scope.Compatibility,
	}, chain)
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/_apis/artifactcache/cache?keys=missing&version=v1", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("Public Cache outage status = %d, want cache miss %d; body = %q", response.Code, http.StatusNoContent, response.Body.String())
	}
}

func TestCacheChainConcurrentTeamHitsAllObserveOneCompleteWarmedArchive(t *testing.T) {
	t.Parallel()

	scope := actionscache.Scope{Repository: "acme/widgets", Ref: "refs/heads/main", DefaultRef: "refs/heads/main", Compatibility: "linux-x64"}
	local := &delayedCommitStorage{StorageIndex: actionscache.NewMemoryStorage(), delay: 75 * time.Millisecond}
	team := actionscache.NewMemoryStorage()
	want := bytes.Repeat([]byte("complete-archive-"), 128)
	putArchive(t, team, scope, "concurrent", "v1", want)
	chain, err := actionscache.NewCacheChain(local, team)
	if err != nil {
		t.Fatal(err)
	}

	const readers = 32
	start := make(chan struct{})
	errorsByReader := make(chan error, readers)
	var wait sync.WaitGroup
	for range readers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			result, err := chain.Lookup(context.Background(), actionscache.LookupRequest{
				Scope: scope, Keys: []string{"concurrent"}, Version: "v1",
			})
			if err != nil {
				errorsByReader <- err
				return
			}
			archive, err := chain.Open(context.Background(), actionscache.OpenRequest{Scope: scope, ID: result.Entry.ID})
			if err != nil {
				errorsByReader <- err
				return
			}
			contents, readErr := io.ReadAll(archive.Body)
			archive.Body.Close()
			if readErr != nil {
				errorsByReader <- readErr
				return
			}
			if !bytes.Equal(contents, want) {
				errorsByReader <- errors.New("reader observed incomplete warmed archive")
			}
		}()
	}
	close(start)
	wait.Wait()
	close(errorsByReader)
	for err := range errorsByReader {
		t.Errorf("concurrent Team Cache hit: %v", err)
	}
}

func TestCacheChainRejectsDeclaredRemoteOversizeBeforeReading(t *testing.T) {
	ctx := context.Background()
	scope := actionscache.Scope{
		Repository: "acme/widgets", Ref: "refs/heads/main",
		DefaultRef: "refs/heads/main", Compatibility: "linux-x64",
	}
	local, closeLocal := openSmallPersistentActionsCache(t, ctx, 4)
	defer closeLocal()
	remote := &declaredArchiveStorage{
		entry: actionscache.Entry{ID: 1, Key: "oversize", Version: "v1", Ref: scope.Ref, Size: 5},
		body:  []byte("12345"),
	}
	chain, err := actionscache.NewCacheChain(local, remote)
	if err != nil {
		t.Fatal(err)
	}
	_, err = chain.Lookup(ctx, actionscache.LookupRequest{Scope: scope, Keys: []string{"oversize"}, Version: "v1"})
	if !errors.Is(err, actionscache.ErrInvalidUpload) {
		t.Fatalf("oversize warm error = %v, want ErrInvalidUpload", err)
	}
	if remote.reads != 0 {
		t.Fatalf("oversize remote body read %d times, want 0", remote.reads)
	}
}

func TestActionsLookupTreatsRejectedRemoteArchiveAsMiss(t *testing.T) {
	ctx := context.Background()
	scope := actionscache.Scope{
		Repository: "acme/widgets", Ref: "refs/heads/main",
		DefaultRef: "refs/heads/main", Compatibility: "linux-x64",
	}
	local, closeLocal := openSmallPersistentActionsCache(t, ctx, 4)
	defer closeLocal()
	remote := &declaredArchiveStorage{
		entry: actionscache.Entry{ID: 1, Key: "oversize", Version: "v1", Ref: scope.Ref, Size: 5},
		body:  []byte("12345"),
	}
	chain, err := actionscache.NewCacheChain(local, remote)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := actionscache.NewHandler(actionscache.Config{
		Repository: scope.Repository, Ref: scope.Ref, DefaultRef: scope.DefaultRef,
		Compatibility: scope.Compatibility, MaxArtifactBytes: 4,
	}, chain)
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/_apis/artifactcache/cache?keys=oversize&version=v1", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("rejected remote archive status = %d, want cache miss %d; body = %q", response.Code, http.StatusNoContent, response.Body.String())
	}
	if remote.reads != 0 {
		t.Fatalf("oversize remote body read %d times, want 0", remote.reads)
	}
}

func TestCacheChainAbortsTruncatedRemoteWarmReservation(t *testing.T) {
	ctx := context.Background()
	scope := actionscache.Scope{
		Repository: "acme/widgets", Ref: "refs/heads/main",
		DefaultRef: "refs/heads/main", Compatibility: "linux-x64",
	}
	local, closeLocal := openSmallPersistentActionsCache(t, ctx, 4)
	defer closeLocal()
	remote := &declaredArchiveStorage{
		entry: actionscache.Entry{ID: 1, Key: "truncated", Version: "v1", Ref: scope.Ref, Size: 4},
		body:  []byte("12"),
	}
	chain, err := actionscache.NewCacheChain(local, remote)
	if err != nil {
		t.Fatal(err)
	}
	_, err = chain.Lookup(ctx, actionscache.LookupRequest{Scope: scope, Keys: []string{"truncated"}, Version: "v1"})
	if !errors.Is(err, actionscache.ErrIncompleteUpload) {
		t.Fatalf("truncated warm error = %v, want ErrIncompleteUpload", err)
	}
	if _, err := local.Reserve(ctx, actionscache.ReserveRequest{
		Scope: scope, Key: "truncated", Version: "v1", CacheSize: int64Pointer(4),
	}); err != nil {
		t.Fatalf("reserve identity after failed warm: %v", err)
	}
}

func openSmallPersistentActionsCache(
	t *testing.T,
	ctx context.Context,
	maxBytes int64,
) (*actionscache.PersistentStorage, func()) {
	t.Helper()
	root := t.TempDir()
	artifacts, err := artifact.Open(ctx, filepath.Join(root, "cas"), maxBytes, 0)
	if err != nil {
		t.Fatal(err)
	}
	storage, err := actionscache.OpenPersistentStorage(ctx, filepath.Join(root, "actions"), artifacts)
	if err != nil {
		artifacts.Close()
		t.Fatal(err)
	}
	return storage, func() {
		if err := storage.Close(); err != nil {
			t.Error(err)
		}
		if err := artifacts.Close(); err != nil {
			t.Error(err)
		}
	}
}

type declaredArchiveStorage struct {
	entry actionscache.Entry
	body  []byte
	reads int
}

func (storage *declaredArchiveStorage) Lookup(_ context.Context, request actionscache.LookupRequest) (actionscache.LookupResult, error) {
	return actionscache.LookupResult{
		Entry: storage.entry, Match: actionscache.MatchExact, RequestedKey: request.Keys[0],
		RefScope: actionscache.RefScopeCurrent, Source: actionscache.SourceTeamCache,
	}, nil
}

func (*declaredArchiveStorage) Reserve(context.Context, actionscache.ReserveRequest) (actionscache.Reservation, error) {
	return actionscache.Reservation{}, errors.New("read-only remote")
}

func (*declaredArchiveStorage) Upload(context.Context, actionscache.UploadRequest) error {
	return errors.New("read-only remote")
}

func (*declaredArchiveStorage) Commit(context.Context, actionscache.CommitRequest) (actionscache.Entry, error) {
	return actionscache.Entry{}, errors.New("read-only remote")
}

func (storage *declaredArchiveStorage) Open(context.Context, actionscache.OpenRequest) (actionscache.Archive, error) {
	return actionscache.Archive{
		Entry: storage.entry,
		Body: &countingReadCloser{
			reader: bytes.NewReader(storage.body),
			onRead: func() { storage.reads++ },
		},
	}, nil
}

type countingReadCloser struct {
	reader io.Reader
	onRead func()
}

func (reader *countingReadCloser) Read(buffer []byte) (int, error) {
	reader.onRead()
	return reader.reader.Read(buffer)
}

func (*countingReadCloser) Close() error { return nil }

type delayedCommitStorage struct {
	actionscache.StorageIndex
	delay time.Duration
}

func (storage *delayedCommitStorage) Commit(ctx context.Context, request actionscache.CommitRequest) (actionscache.Entry, error) {
	time.Sleep(storage.delay)
	return storage.StorageIndex.Commit(ctx, request)
}

func (storage *delayedCommitStorage) InvalidateEntry(ctx context.Context, id int64) error {
	return storage.StorageIndex.(interface {
		InvalidateEntry(context.Context, int64) error
	}).InvalidateEntry(ctx, id)
}

func (storage *delayedCommitStorage) UpdatePublicEntryMetadata(
	ctx context.Context,
	id int64,
	metadata *actionscache.PublicEntryMetadata,
) error {
	return storage.StorageIndex.(interface {
		UpdatePublicEntryMetadata(context.Context, int64, *actionscache.PublicEntryMetadata) error
	}).UpdatePublicEntryMetadata(ctx, id, metadata)
}

type alwaysFailStorage struct{}

func (alwaysFailStorage) Lookup(context.Context, actionscache.LookupRequest) (actionscache.LookupResult, error) {
	return actionscache.LookupResult{}, errors.New("unavailable")
}

func (alwaysFailStorage) Reserve(context.Context, actionscache.ReserveRequest) (actionscache.Reservation, error) {
	return actionscache.Reservation{}, errors.New("unavailable")
}

func (alwaysFailStorage) Upload(context.Context, actionscache.UploadRequest) error {
	return errors.New("unavailable")
}

func (alwaysFailStorage) Commit(context.Context, actionscache.CommitRequest) (actionscache.Entry, error) {
	return actionscache.Entry{}, errors.New("unavailable")
}

func (alwaysFailStorage) Open(context.Context, actionscache.OpenRequest) (actionscache.Archive, error) {
	return actionscache.Archive{}, errors.New("unavailable")
}

type alwaysFailPublicCache struct{ alwaysFailStorage }

func (alwaysFailPublicCache) RevalidateEntry(
	context.Context,
	*actionscache.PublicEntryMetadata,
) (*actionscache.PublicEntryMetadata, error) {
	return nil, errors.New("unavailable")
}
