package server

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/layercache/layercache/internal/access"
	"github.com/layercache/layercache/internal/actionscache"
	"github.com/layercache/layercache/internal/artifact"
	"github.com/layercache/layercache/internal/compatibility"
	"github.com/layercache/layercache/internal/config"
	"github.com/layercache/layercache/internal/measurement"
	"github.com/layercache/layercache/internal/publicbuild"
	"github.com/layercache/layercache/internal/publictrust"
	"github.com/layercache/layercache/internal/remote"
	"github.com/layercache/layercache/internal/uploadqueue"
)

type Server struct {
	config            config.Config
	store             *artifact.Store
	team              *remote.TurboClient
	public            *remote.PublicClient
	publications      *publictrust.Registry
	publicPrivate     ed25519.PrivateKey
	publicBuilds      *publicbuild.SQLiteCoordinator
	actionsStorage    *actionscache.PersistentStorage
	actionsPublic     *actionscache.PublicStorage
	actionsHandler    *actionscache.Handler
	measurements      *measurement.SQLiteRepository
	uploads           *uploadqueue.Queue
	uploadContext     context.Context
	cancelUploads     context.CancelFunc
	uploadWake        chan struct{}
	uploadDone        chan struct{}
	startedAt         time.Time
	runtimePID        int
	runtimeInstanceID string
	mux               *http.ServeMux
}

func New(ctx context.Context, cfg config.Config) (*Server, error) {
	store, err := artifact.Open(ctx, cfg.DataDir, cfg.MaxBytes, cfg.MinFreeBytes)
	if err != nil {
		return nil, err
	}
	runtimeToken, err := config.NewToken()
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("generate runtime instance identity: %w", err)
	}
	server := &Server{
		config: cfg, store: store, mux: http.NewServeMux(), startedAt: time.Now().UTC(),
		runtimePID: os.Getpid(), runtimeInstanceID: "runtime-" + runtimeToken,
	}
	server.measurements, err = measurement.OpenSQLiteRepository(filepath.Join(cfg.DataDir, "measurements.db"))
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	if cfg.Role == "local" && cfg.TeamURL != "" {
		server.team, err = remote.NewTurboClientForCompatibility(cfg.TeamURL, cfg.TeamToken, cfg.CompatibilityID)
		if err != nil {
			_ = server.Close()
			return nil, fmt.Errorf("configure Team Cache: %w", err)
		}
	}
	if cfg.Role == "local" && cfg.PublicURL != "" {
		server.public, err = remote.NewPublicClient(cfg.PublicURL, cfg.PublicTrustKey)
		if err != nil {
			_ = server.Close()
			return nil, fmt.Errorf("configure Public Cache: %w", err)
		}
	}
	if cfg.Role == "public" {
		server.publications, err = publictrust.OpenRegistry(ctx, cfg.DataDir)
		if err != nil {
			_ = server.Close()
			return nil, err
		}
		server.publicPrivate, err = publictrust.DecodePrivateKey(cfg.PublicPrivateKey)
		if err != nil {
			_ = server.Close()
			return nil, err
		}
		if len(cfg.PublicBuildRepositories) > 0 {
			server.publicBuilds, err = publicbuild.OpenSQLiteCoordinator(
				filepath.Join(cfg.DataDir, "public-builds.db"),
				publicBuildConfig(cfg),
			)
			if err != nil {
				_ = server.Close()
				return nil, fmt.Errorf("open Public Build coordinator: %w", err)
			}
		}
	}
	if cfg.Role != "public" {
		server.actionsStorage, err = actionscache.OpenPersistentStorage(ctx, cfg.DataDir, store)
		if err != nil {
			server.Close()
			return nil, err
		}
		var actionsIndex actionscache.StorageIndex
		actionsIndex, server.actionsPublic, err = configureActionsIndex(cfg, server.actionsStorage, server.public)
		if err != nil {
			server.Close()
			return nil, fmt.Errorf("configure GitHub Actions cache: %w", err)
		}
		archiveSigner, signerErr := actionscache.NewHMACArchiveURLSigner(cfg.LocalToken)
		if signerErr != nil {
			server.Close()
			return nil, signerErr
		}
		server.actionsHandler, err = actionscache.NewHandler(actionscache.Config{
			Repository: cfg.ActionsRepository, Ref: cfg.ActionsRef,
			DefaultRef: cfg.ActionsDefaultRef, Compatibility: cfg.CompatibilityID,
			RequireCompatibilitySelector: cfg.Role == "team",
			MaxArtifactBytes:             cfg.MaxBytes,
			ArchiveBaseURL:               cfg.ActionsArchiveBaseURL, ArchiveURLSigner: archiveSigner,
		}, actionsIndex)
		if err != nil {
			server.Close()
			return nil, err
		}
	}
	if cfg.Role == "local" && cfg.TeamURL != "" {
		if err := server.openUploadQueue(); err != nil {
			server.Close()
			return nil, err
		}
	}
	server.routes()
	return server, nil
}

func (server *Server) Close() error {
	server.closeUploadQueue()
	if server.actionsPublic != nil {
		_ = server.actionsPublic.Close()
	}
	if server.actionsStorage != nil {
		_ = server.actionsStorage.Close()
	}
	if server.publications != nil {
		_ = server.publications.Close()
	}
	if server.publicBuilds != nil {
		_ = server.publicBuilds.Close()
	}
	if server.measurements != nil {
		_ = server.measurements.Close()
	}
	return server.store.Close()
}

func (server *Server) Handler() http.Handler {
	return server.mux
}

func (server *Server) routes() {
	server.mux.HandleFunc("GET /healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"status":"ok"}`+"\n")
	})
	server.mux.Handle("GET /v1/status", server.requireToken(http.HandlerFunc(server.status)))
	server.mux.Handle("POST /v1/gc", server.requireToken(http.HandlerFunc(server.garbageCollect)))
	server.mux.Handle("GET /v1/reports", server.requireToken(http.HandlerFunc(server.periodReport)))
	server.mux.Handle("GET /v1/reports/{runID}", server.requireToken(http.HandlerFunc(server.runReport)))
	server.mux.Handle("GET /v8/artifacts/status", server.requireToken(http.HandlerFunc(server.turboStatus)))
	server.mux.Handle("POST /v8/artifacts/events", server.requireToken(http.HandlerFunc(server.turboEvents)))
	server.mux.Handle("/v8/artifacts/{hash}", server.requireToken(http.HandlerFunc(server.turboArtifact)))
	server.publicRoutes()
	server.publicBuildRoutes()
	if server.actionsHandler != nil {
		server.mux.Handle("/_apis/artifactcache/", server.requireToken(server.actionsHandler))
		server.mux.Handle("/_layercache/compatibility/", server.requireToken(server.actionsHandler))
	}
}

func (server *Server) status(writer http.ResponseWriter, request *http.Request) {
	stats, err := server.store.Stats(request.Context())
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "read cache status"})
		return
	}
	result := map[string]any{
		"running": true, "role": server.config.Role, "projectId": server.config.ProjectID,
		"startedAt": server.startedAt, "usageBytes": stats.UsageBytes,
		"artifacts": stats.Artifacts, "entries": stats.Entries,
		"runtimePid": server.runtimePID, "runtimeInstanceId": server.runtimeInstanceID,
	}
	if server.uploads != nil {
		if queued, queueErr := server.uploads.Stats(request.Context()); queueErr == nil {
			result["pendingUploads"] = queued.QueuedJobs
			result["pendingUploadBytes"] = queued.QueuedBytes
		}
	}
	writeJSON(writer, http.StatusOK, result)
}

func (server *Server) runReport(writer http.ResponseWriter, request *http.Request) {
	report, err := server.measurements.RunReport(request.PathValue("runID"))
	if errors.Is(err, measurement.ErrRunNotFound) {
		writeJSON(writer, http.StatusNotFound, map[string]string{"error": "run report not found"})
		return
	}
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "read run report"})
		return
	}
	writeJSON(writer, http.StatusOK, report)
}

func (server *Server) garbageCollect(writer http.ResponseWriter, request *http.Request) {
	targetBytes := server.config.MaxBytes * 8 / 10
	if request.ContentLength != 0 {
		defer request.Body.Close()
		var body struct {
			TargetBytes *int64 `json:"targetBytes"`
		}
		if err := json.NewDecoder(io.LimitReader(request.Body, 64<<10)).Decode(&body); err != nil {
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid garbage collection request"})
			return
		}
		if body.TargetBytes != nil {
			targetBytes = *body.TargetBytes
		}
	}
	if targetBytes < 0 || targetBytes > server.config.MaxBytes {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "targetBytes must be between zero and the configured limit"})
		return
	}
	before, err := server.store.Stats(request.Context())
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "read cache status before garbage collection"})
		return
	}
	if err := server.store.GC(request.Context(), targetBytes); err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "garbage collection failed"})
		return
	}
	after, err := server.store.Stats(request.Context())
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "read cache status after garbage collection"})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"targetBytes":   targetBytes,
		"beforeBytes":   before.UsageBytes,
		"afterBytes":    after.UsageBytes,
		"freedBytes":    before.UsageBytes - after.UsageBytes,
		"beforeEntries": before.Entries,
		"afterEntries":  after.Entries,
	})
}

func (server *Server) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if server.actionsHandler != nil && server.actionsHandler.AuthorizesArchiveDownload(request) {
			next.ServeHTTP(writer, request)
			return
		}
		got := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
		if got == "" {
			writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(server.config.LocalToken)) != 1 {
			runID, err := access.ParseWorkspaceToken(server.config.LocalToken, got, time.Now().UTC())
			if err != nil {
				writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
				return
			}
			request = request.WithContext(access.WithRunID(request.Context(), runID))
		}
		next.ServeHTTP(writer, request)
	})
}

func (server *Server) turboStatus(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]string{"status": "enabled"})
}

func (server *Server) turboEvents(writer http.ResponseWriter, request *http.Request) {
	defer request.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(request.Body, 1<<20))
	writeJSON(writer, http.StatusOK, map[string]bool{"accepted": true})
}

func (server *Server) turboArtifact(writer http.ResponseWriter, request *http.Request) {
	hash := request.PathValue("hash")
	if hash == "" || strings.Contains(hash, "/") {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid artifact hash"})
		return
	}
	compatibilityID, err := server.requestCompatibility(request)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	key := artifact.Key{
		Integration:   "turbo",
		Project:       server.config.ProjectID,
		Compatibility: compatibilityID,
		Native:        hash,
	}
	switch request.Method {
	case http.MethodPut:
		if server.config.Role == "public" {
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		server.putTurbo(writer, request, key)
	case http.MethodGet:
		server.getTurbo(writer, request, key, false)
	case http.MethodHead:
		server.getTurbo(writer, request, key, true)
	default:
		writer.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (server *Server) requestCompatibility(request *http.Request) (string, error) {
	if server.config.Role != "team" {
		return server.config.CompatibilityID, nil
	}
	values := request.Header.Values(compatibility.Header)
	if len(values) != 1 {
		return "", errors.New("send exactly one compatibility selector")
	}
	if err := compatibility.Validate(values[0]); err != nil {
		return "", err
	}
	return values[0], nil
}

func (server *Server) putTurbo(writer http.ResponseWriter, request *http.Request, key artifact.Key) {
	startedAt := time.Now().UTC()
	defer request.Body.Close()
	duration, _ := strconv.ParseInt(request.Header.Get("x-artifact-duration"), 10, 64)
	metadata := artifact.Metadata{
		DurationMS: duration,
		Tag:        request.Header.Get("x-artifact-tag"),
		Values: map[string]string{
			"clientCI":  request.Header.Get("x-artifact-client-ci"),
			"sourceSHA": request.Header.Get("x-artifact-source-sha"),
			"dirtyHash": request.Header.Get("x-artifact-dirty-hash"),
		},
	}
	entry, _, err := server.store.Put(request.Context(), key, metadata, io.LimitReader(request.Body, server.config.MaxBytes+1))
	if errors.Is(err, artifact.ErrConflict) {
		writeJSON(writer, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	if errors.Is(err, artifact.ErrQuota) {
		writeJSON(writer, http.StatusInsufficientStorage, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "cache write failed"})
		return
	}
	degraded := false
	if server.team != nil {
		prepared, pinErr := server.pinTurboUpload(request.Context(), key, entry)
		if pinErr != nil {
			degraded = true
			writer.Header().Set("x-layercache-degraded", "team-publication-failed")
			writer.Header().Set("x-layercache-upload-queue", "full-or-unavailable")
		} else if _, file, openErr := server.store.Get(request.Context(), key); openErr == nil {
			publishErr := server.team.Put(request.Context(), key.Native, entry, file)
			file.Close()
			if publishErr != nil {
				degraded = true
				writer.Header().Set("x-layercache-degraded", "team-publication-failed")
				if queueErr := server.enqueuePinnedTurboUpload(request.Context(), prepared); queueErr != nil {
					writer.Header().Set("x-layercache-upload-queue", "full-or-unavailable")
				} else {
					writer.Header().Set("x-layercache-upload-queue", "pending")
				}
			} else {
				server.releaseTurboUploadPin(prepared.pin.Owner)
				writer.Header().Set("x-layercache-team-upload", "complete")
			}
		} else {
			server.releaseTurboUploadPin(prepared.pin.Owner)
			degraded = true
			writer.Header().Set("x-layercache-degraded", "team-publication-failed")
			writer.Header().Set("x-layercache-upload-queue", "full-or-unavailable")
		}
	}
	server.recordTurboMiss(request, key, entry, startedAt, degraded)
	writeJSON(writer, http.StatusOK, map[string]bool{"stored": true})
}

func (server *Server) getTurbo(writer http.ResponseWriter, request *http.Request, key artifact.Key, head bool) {
	startedAt := time.Now().UTC()
	entry, file, source, err := server.resolveTurbo(request.Context(), key)
	if errors.Is(err, artifact.ErrNotFound) {
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "cache read failed"})
		return
	}
	defer file.Close()
	setTurboHeaders(writer, entry, source)
	if head {
		writer.WriteHeader(http.StatusOK)
		return
	}
	writer.WriteHeader(http.StatusOK)
	bytesWritten, copyErr := io.Copy(writer, file)
	if copyErr == nil && bytesWritten == entry.Size {
		server.recordTurboHit(request, key, entry, source, startedAt, time.Now().UTC())
	}
}

func (server *Server) recordTurboMiss(request *http.Request, key artifact.Key, entry artifact.Entry, uploadStarted time.Time, degraded bool) {
	runID := access.RunID(request.Context())
	if runID == "" {
		return
	}
	finishedAt := time.Now().UTC()
	execution := time.Duration(entry.Metadata.DurationMS) * time.Millisecond
	startedAt := finishedAt
	var executionDuration *time.Duration
	if execution > 0 {
		startedAt = uploadStarted.Add(-execution)
		executionDuration = &execution
	}
	_ = server.measurements.Record(measurement.FinalOutcome{
		RunID: runID, WorkID: measurementIdentity(key), ArtifactID: measurementIdentity(key),
		CompatibilityID: key.Compatibility, Result: measurement.ResultMiss, Source: measurement.SourceNone,
		StartedAt: startedAt, FinishedAt: finishedAt, ExecutionDuration: executionDuration,
		Timing: measurement.Timing{Upload: finishedAt.Sub(uploadStarted)},
		Bytes:  measurement.Bytes{Uploaded: entry.Size}, Degraded: degraded,
	})
}

func (server *Server) recordTurboHit(request *http.Request, key artifact.Key, entry artifact.Entry, source string, startedAt, finishedAt time.Time) {
	runID := access.RunID(request.Context())
	if runID == "" {
		return
	}
	var producerDuration *time.Duration
	if entry.Metadata.DurationMS > 0 {
		value := time.Duration(entry.Metadata.DurationMS) * time.Millisecond
		producerDuration = &value
	}
	_ = server.measurements.Record(measurement.FinalOutcome{
		RunID: runID, WorkID: measurementIdentity(key), ArtifactID: measurementIdentity(key),
		CompatibilityID: key.Compatibility, Result: measurement.ResultHit, Source: measurementSource(source),
		StartedAt: startedAt, FinishedAt: finishedAt, ProducerDuration: producerDuration,
		Timing: measurement.Timing{Download: finishedAt.Sub(startedAt)},
		Bytes:  measurement.Bytes{Downloaded: entry.Size},
	})
}

func measurementIdentity(key artifact.Key) string {
	digest := sha256.Sum256([]byte(key.Integration + "\x00" + key.Project + "\x00" + key.Compatibility + "\x00" + key.Native + "\x00" + key.Version + "\x00" + key.Ref))
	return fmt.Sprintf("sha256:%x", digest[:])
}

func measurementSource(source string) measurement.Source {
	switch source {
	case "local":
		return measurement.SourceLocalCache
	case "team":
		return measurement.SourceTeamCache
	case "public":
		return measurement.SourcePublicCache
	default:
		return measurement.SourceUnattributed
	}
}

func (server *Server) resolveTurbo(ctx context.Context, key artifact.Key) (artifact.Entry, io.ReadCloser, string, error) {
	entry, file, err := server.store.Get(ctx, key)
	if err == nil {
		if entry.Metadata.Values["origin"] != "public" || server.public == nil {
			return entry, file, server.config.Role, nil
		}
		publication, _, _, checkErr := server.public.Resolve(ctx, publictrust.Expected{
			Integration: key.Integration, Project: key.Project,
			Compatibility: key.Compatibility, NativeKey: key.Native,
		})
		if checkErr == nil && publication.Digest == entry.Digest {
			return entry, file, server.config.Role, nil
		}
		leaseExpires, leaseErr := time.Parse(time.RFC3339Nano, entry.Metadata.Values["leaseExpires"])
		trustFailure := (checkErr == nil && publication.Digest != entry.Digest) || errors.Is(checkErr, remote.ErrMiss) ||
			errors.Is(checkErr, publictrust.ErrSignature) ||
			errors.Is(checkErr, publictrust.ErrIdentity) ||
			errors.Is(checkErr, publictrust.ErrExpired)
		if !trustFailure && leaseErr == nil && time.Now().UTC().Before(leaseExpires) {
			return entry, file, server.config.Role, nil
		}
		file.Close()
		_ = server.store.Delete(ctx, key)
		err = artifact.ErrNotFound
	}
	if !errors.Is(err, artifact.ErrNotFound) && !errors.Is(err, artifact.ErrCorrupt) {
		return artifact.Entry{}, nil, "", err
	}
	if server.team != nil {
		download, teamErr := server.team.Get(ctx, key.Native)
		if teamErr == nil {
			defer download.Body.Close()
			if download.ExpectedDigest == "" {
				return artifact.Entry{}, nil, "", artifact.ErrCorrupt
			}
			if _, _, err := server.store.PutVerified(ctx, key, download.Metadata, download.Body, download.ExpectedDigest); err != nil {
				return artifact.Entry{}, nil, "", err
			}
			entry, file, err = server.store.Get(ctx, key)
			if err != nil {
				return artifact.Entry{}, nil, "", err
			}
			return entry, file, "team", nil
		}
	}
	if server.public != nil {
		download, publicErr := server.public.Get(ctx, publictrust.Expected{
			Integration: key.Integration, Project: key.Project,
			Compatibility: key.Compatibility, NativeKey: key.Native,
		})
		if publicErr == nil {
			defer download.Body.Close()
			metadata := artifact.Metadata{
				DurationMS: download.Publication.DurationMS,
				Values: map[string]string{
					"origin":         "public",
					"publicIdentity": download.Publication.Identity(),
					"leaseExpires":   download.Publication.ExpiresAt.Format(time.RFC3339Nano),
				},
			}
			stored, _, err := server.store.PutVerified(ctx, key, metadata, download.Body, download.Publication.Digest)
			if err != nil {
				return artifact.Entry{}, nil, "", err
			}
			if stored.Size != download.Publication.Size {
				_ = server.store.Delete(context.WithoutCancel(ctx), key)
				return artifact.Entry{}, nil, "", artifact.ErrCorrupt
			}
			entry, file, err = server.store.Get(ctx, key)
			if err != nil {
				return artifact.Entry{}, nil, "", err
			}
			return entry, file, "public", nil
		}
	}
	return artifact.Entry{}, nil, "", artifact.ErrNotFound
}

func setTurboHeaders(writer http.ResponseWriter, entry artifact.Entry, source string) {
	writer.Header().Set("Content-Type", "application/octet-stream")
	writer.Header().Set("Content-Length", strconv.FormatInt(entry.Size, 10))
	writer.Header().Set("x-artifact-duration", strconv.FormatInt(entry.Metadata.DurationMS, 10))
	if entry.Metadata.Tag != "" {
		writer.Header().Set("x-artifact-tag", entry.Metadata.Tag)
	}
	writer.Header().Set("x-layercache-digest", "sha256:"+entry.Digest)
	writer.Header().Set("x-layercache-source", source)
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func RunHTTP(ctx context.Context, cfg config.Config) error {
	cacheServer, err := New(ctx, cfg)
	if err != nil {
		return err
	}
	defer cacheServer.Close()
	httpServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           cacheServer.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	shutdownDone := make(chan struct{})
	go func() {
		<-ctx.Done()
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownContext)
		close(shutdownDone)
	}()
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve Layer Cache at %s: %w", cfg.Listen, err)
	}
	if ctx.Err() != nil {
		<-shutdownDone
	}
	return nil
}
