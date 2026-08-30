package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/artifact"
	"github.com/layercache/layercache/internal/config"
	"github.com/layercache/layercache/internal/remote"
	"github.com/layercache/layercache/internal/uploadqueue"
)

func TestTurboArtifactIsPinnedBeforeTheImmediateTeamUpload(t *testing.T) {
	ctx := context.Background()
	var instance *Server
	team := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := instance.store.GC(ctx, 0); !errors.Is(err, artifact.ErrQuota) {
			t.Errorf("GC during Team upload = %v, want ErrQuota", err)
		}
		if _, err := io.Copy(io.Discard, request.Body); err != nil {
			t.Errorf("read Team upload: %v", err)
		}
		writer.WriteHeader(http.StatusOK)
	}))
	defer team.Close()

	cfg, err := config.Defaults()
	if err != nil {
		t.Fatal(err)
	}
	cfg.DataDir = t.TempDir()
	cfg.MaxBytes = 4
	cfg.MinFreeBytes = 0
	cfg.ProjectID = "github.com/acme/widget"
	cfg.ActionsRepository = cfg.ProjectID
	cfg.CompatibilityID = "linux-amd64-schema1"
	cfg.TeamURL = team.URL
	cfg.TeamToken = "team-token"
	instance, err = New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instance.Close() })

	request := httptest.NewRequest(http.MethodPut, "/v8/artifacts/immediate", bytes.NewReader([]byte("keep")))
	request.Header.Set("Authorization", "Bearer "+cfg.LocalToken)
	response := httptest.NewRecorder()
	instance.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("Local Cache PUT status = %d, want 200: %s", response.Code, response.Body.String())
	}
	key := artifact.Key{
		Integration: "turbo", Project: cfg.ProjectID,
		Compatibility: cfg.CompatibilityID, Native: "immediate",
	}
	if _, file, err := instance.store.Get(ctx, key); err != nil {
		t.Fatalf("Get Local Cache entry after immediate Team upload: %v", err)
	} else {
		_ = file.Close()
	}
}

func TestQueuedTurboArtifactStaysPinnedUntilUploadCompletion(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := artifact.Open(ctx, filepath.Join(root, "cache"), 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	queue, err := uploadqueue.Open(uploadqueue.Config{
		Path: filepath.Join(root, "uploads.db"), MaxQueuedBytes: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = queue.Close() })
	key := artifact.Key{
		Integration: "turbo", Project: "github.com/acme/widget",
		Compatibility: "linux-amd64-schema1", Native: "queued",
	}
	entry, _, err := store.Put(ctx, key, artifact.Metadata{}, bytes.NewReader([]byte("keep")))
	if err != nil {
		t.Fatal(err)
	}
	instance := &Server{
		config: config.Config{
			TeamURL: "https://team.example", ProjectID: key.Project,
			CompatibilityID: key.Compatibility,
		},
		store: store, uploads: queue, uploadWake: make(chan struct{}, 1),
	}
	if err := instance.enqueueTurboUpload(ctx, key, entry); err != nil {
		t.Fatalf("enqueue Team Cache upload: %v", err)
	}
	if err := store.GC(ctx, 0); !errors.Is(err, artifact.ErrQuota) {
		t.Fatalf("GC while Team upload is queued = %v, want ErrQuota", err)
	}
	if _, file, err := store.Get(ctx, key); err != nil {
		t.Fatalf("Get queued Team upload artifact: %v", err)
	} else {
		_ = file.Close()
	}

	lease, err := queue.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	instance.uploadContext = ctx
	instance.completeUpload(lease)
	if err := store.GC(ctx, 0); err != nil {
		t.Fatalf("GC after Team upload completion: %v", err)
	}
	if _, _, err := store.Get(ctx, key); !errors.Is(err, artifact.ErrNotFound) {
		t.Fatalf("Get completed Team upload artifact = %v, want ErrNotFound", err)
	}
}

func TestRestartReconcilesQueuedTurboPinWithOriginalCompatibility(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := artifact.Open(ctx, root, 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	queuePath := filepath.Join(root, "team-uploads.db")
	queue, err := uploadqueue.Open(uploadqueue.Config{Path: queuePath, MaxQueuedBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	key := artifact.Key{
		Integration: "turbo", Project: "github.com/acme/widget",
		Compatibility: "linux-amd64-node@24-schema1", Native: "queued",
	}
	entry, _, err := store.Put(ctx, key, artifact.Metadata{}, bytes.NewReader([]byte("keep")))
	if err != nil {
		t.Fatal(err)
	}
	beforeRestart := &Server{
		config: config.Config{
			TeamURL: "https://team.example", ProjectID: key.Project,
			CompatibilityID: key.Compatibility,
		},
		store: store, uploads: queue, uploadWake: make(chan struct{}, 1),
	}
	if err := beforeRestart.enqueueTurboUpload(ctx, key, entry); err != nil {
		t.Fatalf("enqueue Team Cache upload: %v", err)
	}
	if err := queue.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = artifact.Open(ctx, root, 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	queue, err = uploadqueue.Open(uploadqueue.Config{Path: queuePath, MaxQueuedBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = queue.Close() })
	afterRestart := &Server{
		config: config.Config{
			TeamURL: "https://team.example", ProjectID: key.Project,
			CompatibilityID: "darwin-arm64-node@24-schema1",
		},
		store: store, uploads: queue,
	}
	if err := afterRestart.reconcileUploadPins(ctx); err != nil {
		t.Fatalf("reconcile Team upload pins: %v", err)
	}
	if err := store.GC(ctx, 0); !errors.Is(err, artifact.ErrQuota) {
		t.Fatalf("GC after compatibility changed = %v, want ErrQuota", err)
	}
	if _, file, err := store.Get(ctx, key); err != nil {
		t.Fatalf("Get queued artifact under its original compatibility: %v", err)
	} else {
		_ = file.Close()
	}
}

func TestRestartUploadsQueuedTurboArtifactWithOriginalCompatibility(t *testing.T) {
	ctx := context.Background()
	requests := make(chan *http.Request, 1)
	bodies := make(chan []byte, 1)
	team := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read Team upload: %v", err)
		}
		requests <- request.Clone(context.Background())
		bodies <- body
		writer.WriteHeader(http.StatusOK)
	}))
	defer team.Close()

	root := t.TempDir()
	store, err := artifact.Open(ctx, root, 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	queuePath := filepath.Join(root, "team-uploads.db")
	queue, err := uploadqueue.Open(uploadqueue.Config{Path: queuePath, MaxQueuedBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	key := artifact.Key{
		Integration: "turbo", Project: "github.com/acme/widget",
		Compatibility: "linux-amd64-node@24-schema1", Native: "queued-original",
	}
	entry, _, err := store.Put(ctx, key, artifact.Metadata{}, bytes.NewReader([]byte("keep")))
	if err != nil {
		t.Fatal(err)
	}
	beforeRestart := &Server{
		config: config.Config{
			TeamURL: team.URL, TeamToken: "team-token", ProjectID: key.Project,
			CompatibilityID: key.Compatibility,
		},
		store: store, uploads: queue, uploadWake: make(chan struct{}, 1),
	}
	if err := beforeRestart.enqueueTurboUpload(ctx, key, entry); err != nil {
		t.Fatal(err)
	}
	if err := queue.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = artifact.Open(ctx, root, 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	queue, err = uploadqueue.Open(uploadqueue.Config{Path: queuePath, MaxQueuedBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = queue.Close() })
	currentCompatibility := "darwin-arm64-node@24-schema1"
	currentClient, err := remote.NewTurboClientForCompatibility(team.URL, "team-token", currentCompatibility)
	if err != nil {
		t.Fatal(err)
	}
	afterRestart := &Server{
		config: config.Config{
			TeamURL: team.URL, TeamToken: "team-token", ProjectID: key.Project,
			CompatibilityID: currentCompatibility,
		},
		store: store, uploads: queue, team: currentClient, uploadContext: ctx,
	}
	if err := afterRestart.reconcileUploadPins(ctx); err != nil {
		t.Fatal(err)
	}
	lease, err := queue.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	afterRestart.processUpload(lease)

	var request *http.Request
	select {
	case request = <-requests:
	case <-time.After(2 * time.Second):
		t.Fatal("queued Team upload did not reach the configured endpoint")
	}
	if got := request.Header.Get("X-LayerCache-Compatibility"); got != key.Compatibility {
		t.Fatalf("Team upload compatibility = %q, want %q", got, key.Compatibility)
	}
	if request.URL.Path != "/v8/artifacts/"+key.Native {
		t.Fatalf("Team upload path = %q, want original native key", request.URL.Path)
	}
	select {
	case got := <-bodies:
		if !bytes.Equal(got, []byte("keep")) {
			t.Fatalf("Team upload body = %q, want %q", got, "keep")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued Team upload body was not received")
	}
	job, err := queue.Get(ctx, lease.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != uploadqueue.StateCompleted {
		t.Fatalf("Team upload state = %q, want completed", job.State)
	}
}

func TestRestartReleasesPinLeftAfterQueueCompletion(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := artifact.Open(ctx, root, 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	queuePath := filepath.Join(root, "team-uploads.db")
	queue, err := uploadqueue.Open(uploadqueue.Config{Path: queuePath, MaxQueuedBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	key := artifact.Key{
		Integration: "turbo", Project: "github.com/acme/widget",
		Compatibility: "linux-amd64-schema1", Native: "completed-before-crash",
	}
	entry, _, err := store.Put(ctx, key, artifact.Metadata{}, bytes.NewReader([]byte("keep")))
	if err != nil {
		t.Fatal(err)
	}
	instance := &Server{
		config: config.Config{
			TeamURL: "https://team.example", ProjectID: key.Project,
			CompatibilityID: key.Compatibility,
		},
		store: store, uploads: queue, uploadWake: make(chan struct{}, 1),
	}
	if err := instance.enqueueTurboUpload(ctx, key, entry); err != nil {
		t.Fatal(err)
	}
	lease, err := queue.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Complete(ctx, lease); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after the queue commit but before Server.completeUpload
	// could release the independently persisted Local Cache pin.
	if err := queue.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = artifact.Open(ctx, root, 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	queue, err = uploadqueue.Open(uploadqueue.Config{Path: queuePath, MaxQueuedBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = queue.Close() })
	afterRestart := &Server{config: instance.config, store: store, uploads: queue}
	if err := afterRestart.reconcileUploadPins(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.GC(ctx, 0); err != nil {
		t.Fatalf("GC after completed upload pin reconciliation: %v", err)
	}
	if _, _, err := store.Get(ctx, key); !errors.Is(err, artifact.ErrNotFound) {
		t.Fatalf("Get artifact after completed pin reconciliation = %v, want ErrNotFound", err)
	}
}

func TestProcessUploadReleasesLeaseWhenRuntimeIsStopping(t *testing.T) {
	queue, err := uploadqueue.Open(uploadqueue.Config{
		Path:           filepath.Join(t.TempDir(), "uploads.db"),
		MaxQueuedBytes: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = queue.Close() })

	queued, err := queue.Enqueue(context.Background(), uploadqueue.EnqueueRequest{
		Adapter: uploadqueue.AdapterTurbo,
		Target: uploadqueue.Target{
			Scope:    uploadqueue.ScopeTeam,
			Endpoint: "https://old-team.example",
			Project:  "project",
		},
		Identity: "artifact",
		Digest:   "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Size:     64,
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := queue.Claim(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	stopping, cancel := context.WithCancel(context.Background())
	cancel()
	instance := &Server{
		config:        config.Config{TeamURL: "https://new-team.example", ProjectID: "project"},
		uploads:       queue,
		uploadContext: stopping,
	}
	instance.processUpload(lease)

	job, err := queue.Get(context.Background(), queued.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != uploadqueue.StatePending {
		t.Fatalf("job state after shutdown interrupted a lease = %q, want pending", job.State)
	}
}

func TestClaimedUploadIsRequeuedWhenRuntimeStopsBeforeProcessing(t *testing.T) {
	queue, err := uploadqueue.Open(uploadqueue.Config{
		Path:           filepath.Join(t.TempDir(), "uploads.db"),
		MaxQueuedBytes: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = queue.Close() })

	queued, err := queue.Enqueue(context.Background(), uploadqueue.EnqueueRequest{
		Adapter: uploadqueue.AdapterTurbo,
		Target: uploadqueue.Target{
			Scope:    uploadqueue.ScopeTeam,
			Endpoint: "https://team.example",
			Project:  "project",
		},
		Identity: "artifact",
		Digest:   "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Size:     64,
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := queue.Claim(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	stopping, cancel := context.WithCancel(context.Background())
	cancel()
	instance := &Server{uploads: queue, uploadContext: stopping}

	if instance.processClaimedUpload(lease) {
		t.Fatal("claim processing continued after runtime cancellation")
	}
	job, err := queue.Get(context.Background(), queued.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != uploadqueue.StatePending || job.LastError != "runtime_stopping" {
		t.Fatalf("job after interrupted claim = %#v, want pending runtime_stopping", job)
	}
}

func TestQueueTransitionsSurviveRuntimeCancellation(t *testing.T) {
	queue, err := uploadqueue.Open(uploadqueue.Config{
		Path:           filepath.Join(t.TempDir(), "uploads.db"),
		MaxQueuedBytes: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = queue.Close() })

	queued, err := queue.Enqueue(context.Background(), uploadqueue.EnqueueRequest{
		Adapter: uploadqueue.AdapterTurbo,
		Target: uploadqueue.Target{
			Scope: uploadqueue.ScopeTeam, Endpoint: "https://team.example", Project: "project",
		},
		Identity: "artifact",
		Digest:   "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Size:     64,
	})
	if err != nil {
		t.Fatal(err)
	}
	stopping, cancel := context.WithCancel(context.Background())
	cancel()
	instance := &Server{uploads: queue, uploadContext: stopping}

	lease, err := instance.claimUpload()
	if err != nil {
		t.Fatalf("claim after lifecycle cancellation: %v", err)
	}
	instance.completeUpload(lease)
	job, err := queue.Get(context.Background(), queued.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != uploadqueue.StateCompleted {
		t.Fatalf("job state after completion during shutdown = %q, want completed", job.State)
	}
}
