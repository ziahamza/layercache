package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/actionscache"
	"github.com/layercache/layercache/internal/artifact"
	"github.com/layercache/layercache/internal/config"
	"github.com/layercache/layercache/internal/uploadqueue"
)

func TestStatusAggregatesTurboAndActionsTeamPublications(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	store, err := artifact.Open(ctx, filepath.Join(root, "cache"), 64<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	actions, err := actionscache.OpenPersistentStorage(ctx, filepath.Join(root, "actions"), store)
	if err != nil {
		t.Fatal(err)
	}
	defer actions.Close()
	team := &blockingActionsTeamWriter{started: make(chan struct{}), release: make(chan struct{})}
	defer close(team.release)
	if err := actions.EnableTeamPublication(team); err != nil {
		t.Fatal(err)
	}

	actionsBytes := []byte("pending-actions")
	actionsSize := int64(len(actionsBytes))
	scope := actionscache.Scope{
		Repository: "acme/widget", Compatibility: "linux-amd64-schema1",
		Ref: "refs/heads/main", DefaultRef: "refs/heads/main",
	}
	reservation, err := actions.Reserve(ctx, actionscache.ReserveRequest{
		Scope: scope, Key: "pending-actions", Version: "v1", CacheSize: &actionsSize,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := actions.Upload(ctx, actionscache.UploadRequest{
		ReservationID: reservation.ID, Start: 0, End: actionsSize - 1, Body: bytes.NewReader(actionsBytes),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := actions.Commit(ctx, actionscache.CommitRequest{ReservationID: reservation.ID, Size: actionsSize}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-team.started:
	case <-time.After(5 * time.Second):
		t.Fatal("Actions Team publication did not start")
	}

	turbo, err := uploadqueue.Open(uploadqueue.Config{
		Path: filepath.Join(root, "turbo-uploads.db"), MaxQueuedBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer turbo.Close()
	const turboBytes = int64(7)
	if _, err := turbo.Enqueue(ctx, uploadqueue.EnqueueRequest{
		Adapter: uploadqueue.AdapterTurbo,
		Target: uploadqueue.Target{
			Scope: uploadqueue.ScopeTeam, Endpoint: "http://127.0.0.1:1", Project: "github.com/acme/widget",
		},
		Identity: "pending-turbo", Digest: strings.Repeat("a", 64), Size: turboBytes,
	}); err != nil {
		t.Fatal(err)
	}

	instance := &Server{
		config: config.Config{Role: "local", ProjectID: "github.com/acme/widget"},
		store:  store, actionsStorage: actions, uploads: turbo,
	}
	response := httptest.NewRecorder()
	instance.status(response, httptest.NewRequest(http.MethodGet, "/v1/status", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	var status struct {
		PendingUploads     int64 `json:"pendingUploads"`
		PendingUploadBytes int64 `json:"pendingUploadBytes"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.PendingUploads != 2 || status.PendingUploadBytes != actionsSize+turboBytes {
		t.Fatalf("combined pending uploads = %#v", status)
	}
}

type blockingActionsTeamWriter struct {
	startedOnce sync.Once
	started     chan struct{}
	release     chan struct{}
}

func (writer *blockingActionsTeamWriter) Reserve(
	ctx context.Context,
	_ actionscache.ReserveRequest,
) (actionscache.Reservation, error) {
	writer.startedOnce.Do(func() { close(writer.started) })
	select {
	case <-ctx.Done():
		return actionscache.Reservation{}, ctx.Err()
	case <-writer.release:
		return actionscache.Reservation{ID: 1}, nil
	}
}

func (*blockingActionsTeamWriter) Upload(context.Context, actionscache.UploadRequest) error {
	return nil
}

func (*blockingActionsTeamWriter) Commit(context.Context, actionscache.CommitRequest) (actionscache.Entry, error) {
	return actionscache.Entry{}, nil
}
