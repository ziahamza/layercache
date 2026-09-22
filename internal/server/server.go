package server

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/layercache/layercache/internal/access"
	"github.com/layercache/layercache/internal/actionscache"
	"github.com/layercache/layercache/internal/artifact"
	"github.com/layercache/layercache/internal/cloud"
	"github.com/layercache/layercache/internal/compatibility"
	"github.com/layercache/layercache/internal/config"
	"github.com/layercache/layercache/internal/measurement"
	"github.com/layercache/layercache/internal/publicbuild"
	"github.com/layercache/layercache/internal/publictrust"
	"github.com/layercache/layercache/internal/remote"
	"github.com/layercache/layercache/internal/retention"
	"github.com/layercache/layercache/internal/telemetry"
	"github.com/layercache/layercache/internal/uploadqueue"
)

type measurementRepository interface {
	Record(measurement.FinalOutcome) error
	EnrichActionsMiss(measurement.ActionsMissCompletion) error
	ObserveTurbo(measurement.TurboObservation) error
	RunReport(string) (measurement.RunReport, error)
	PeriodReport(time.Time, time.Time) (measurement.PeriodReport, error)
	Close() error
}

type publicBuildCoordinator interface {
	publicbuild.Coordinator
	publicbuild.StatusReader
	Close() error
}

type Server struct {
	storagePool       StoragePoolConfig
	config            config.Config
	runtimeLock       *runtimeDirectoryLock
	store             cacheStore
	localStore        *artifact.Store
	cloudStore        *cloud.Store
	team              *remote.TurboClient
	actionsTeam       *actionscache.RemoteStorage
	teamTokenMu       sync.RWMutex
	teamToken         string
	public            *remote.PublicClient
	publications      publicationRegistry
	publicPrivate     ed25519.PrivateKey
	publicBuilds      publicBuildCoordinator
	actionsStorage    actionsStorage
	cloudActions      *cloud.ActionsStorage
	actionsPublic     *actionscache.PublicCacheIndex
	actionsHandler    *actionscache.Handler
	measurements      measurementRepository
	uploads           *uploadqueue.Queue
	uploadContext     context.Context
	cancelUploads     context.CancelFunc
	uploadWake        chan struct{}
	uploadDone        chan struct{}
	startedAt         time.Time
	runtimePID        int
	runtimeInstanceID string
	mux               *http.ServeMux
	cloudCancel       context.CancelFunc
	cloudDone         chan struct{}
	cloudHealthMu     cloudHealthLock
	cloudHealthy      bool
	telemetry         *telemetry.Runtime
	telemetryDegraded bool
}

func New(ctx context.Context, cfg config.Config) (*Server, error) {
	policy, err := retention.Normalize(cfg.EvictionPolicy)
	if err != nil {
		return nil, err
	}
	cfg.EvictionPolicy = policy
	runtimeLock, err := acquireRuntimeDirectoryLock(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	localStore, err := artifact.Open(ctx, cfg.DataDir, cfg.MaxBytes, cfg.MinFreeBytes)
	if err != nil {
		_ = runtimeLock.Close()
		return nil, err
	}
	if err := localStore.SetEvictionPolicy(cfg.EvictionPolicy); err != nil {
		_ = localStore.Close()
		_ = runtimeLock.Close()
		return nil, err
	}
	runtimeToken, err := config.NewToken()
	if err != nil {
		_ = localStore.Close()
		_ = runtimeLock.Close()
		return nil, fmt.Errorf("generate runtime instance identity: %w", err)
	}
	server := &Server{
		config: cfg, runtimeLock: runtimeLock, store: localStore, localStore: localStore, mux: http.NewServeMux(), startedAt: time.Now().UTC(),
		teamToken: cfg.TeamToken, runtimePID: os.Getpid(), runtimeInstanceID: "runtime-" + runtimeToken,
	}
	telemetryContext, cancelTelemetrySetup := context.WithTimeout(ctx, 5*time.Second)
	server.telemetry, err = telemetry.NewFromEnvironment(telemetryContext, telemetry.Options{
		ServiceName: "layercache", Role: cfg.Role,
	})
	cancelTelemetrySetup()
	if err != nil {
		// Telemetry is an optional operational export. Cache traffic must remain
		// available when a collector or exporter configuration is unavailable.
		server.telemetryDegraded = true
	}
	if cfg.CloudPostgresURL != "" {
		server.cloudStore, err = cloud.Open(ctx, cloudStoreConfig(cfg))
		if err != nil {
			_ = server.Close()
			return nil, fmt.Errorf("open cloud cache persistence: %w", err)
		}
		server.store = server.cloudStore
		if cfg.Role == "team" || cfg.Role == "public" {
			if err := server.applyConfiguredCloudMemberships(ctx, server.cloudStore); err != nil {
				_ = server.Close()
				return nil, err
			}
		}
	}
	if cfg.CloudPostgresURL != "" {
		server.measurements, err = measurement.OpenPostgresRepository(ctx, cfg.CloudPostgresURL, cfg.ProjectID)
	} else {
		server.measurements, err = measurement.OpenSQLiteRepository(filepath.Join(cfg.DataDir, "measurements.db"))
	}
	if err != nil {
		_ = server.Close()
		return nil, fmt.Errorf("open measurement persistence: %w", err)
	}
	if cfg.Role == "local" && cfg.TeamURL != "" && cfg.TeamToken != "" {
		server.team, err = remote.NewTurboClientForCompatibilityWithTimeouts(cfg.TeamURL, cfg.TeamToken, cfg.CompatibilityID, remote.Timeouts{
			Metadata: cfg.RemoteMetadataTimeout, TransferIdle: cfg.RemoteTransferIdleTimeout,
		})
		if err != nil {
			_ = server.Close()
			return nil, fmt.Errorf("configure Team Cache: %w", err)
		}
	}
	if cfg.Role == "local" && cfg.PublicURL != "" {
		server.public, err = remote.NewPublicClientWithTimeouts(cfg.PublicURL, cfg.PublicTrustKey, remote.Timeouts{
			Metadata: cfg.RemoteMetadataTimeout, TransferIdle: cfg.RemoteTransferIdleTimeout,
		})
		if err != nil {
			_ = server.Close()
			return nil, fmt.Errorf("configure Public Cache: %w", err)
		}
	}
	if cfg.Role == "public" {
		if server.cloudStore != nil {
			server.publications, err = cloud.NewPublicationRegistry(server.cloudStore, 0)
		} else {
			server.publications, err = publictrust.OpenRegistry(ctx, cfg.DataDir)
		}
		if err == nil {
			err = server.reconcilePublicPins(ctx, server.publications)
		}
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
			buildConfig := publicBuildConfig(
				cfg,
				server.publications,
				server.store,
				func() publicBuildCoordinator { return server.publicBuilds },
			)
			if cfg.CloudPostgresURL != "" {
				server.publicBuilds, err = publicbuild.OpenPostgresCoordinator(
					ctx, cfg.CloudPostgresURL, cfg.ProjectID, buildConfig,
				)
			} else {
				server.publicBuilds, err = publicbuild.OpenSQLiteCoordinator(
					filepath.Join(cfg.DataDir, "public-builds.db"), buildConfig,
				)
			}
			if err != nil {
				_ = server.Close()
				return nil, fmt.Errorf("open Public Build coordinator: %w", err)
			}
		}
	}
	if cfg.Role != "public" {
		if server.cloudStore != nil {
			server.cloudActions, err = cloud.NewActionsStorage(server.cloudStore)
			server.actionsStorage = server.cloudActions
		} else {
			server.actionsStorage, err = actionscache.OpenPersistentStorage(ctx, cfg.DataDir, localStore)
		}
		if err != nil {
			server.Close()
			return nil, err
		}
		var actionsIndex actionscache.StorageIndex
		actionsIndex, server.actionsPublic, server.actionsTeam, err = configureActionsIndex(cfg, server.actionsStorage, localStore, server.public)
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
			Project: cfg.ProjectID, Repository: cfg.ActionsRepository, Ref: cfg.ActionsRef,
			DefaultRef: cfg.ActionsDefaultRef, Compatibility: cfg.CompatibilityID,
			RequireCompatibilitySelector: cfg.Role == "team",
			RequireRequestAuthority:      cfg.Role == "team",
			MaxArtifactBytes:             cfg.MaxBytes,
			ArtifactLimit:                server.cacheLimit,
			ArchiveBaseURL:               cfg.ActionsArchiveBaseURL, ArchiveURLSigner: archiveSigner,
			RecordOutcome:     server.measurements.Record,
			EnrichActionsMiss: server.measurements.EnrichActionsMiss,
		}, actionsIndex)
		if err != nil {
			server.Close()
			return nil, err
		}
	}
	if cfg.Role == "local" && cfg.TeamURL != "" && cfg.TeamToken != "" {
		if err := server.openUploadQueue(); err != nil {
			server.Close()
			return nil, err
		}
	}
	if server.cloudStore != nil {
		server.startCloudMaintenance()
	}
	server.routes()
	return server, nil
}

func (server *Server) Close() error {
	server.closeUploadQueue()
	server.stopCloudMaintenance()
	var closeErrors []error
	if server.actionsPublic != nil {
		closeErrors = append(closeErrors, server.actionsPublic.Close())
	}
	if server.actionsStorage != nil {
		closeErrors = append(closeErrors, server.actionsStorage.Close())
	}
	if server.publications != nil {
		closeErrors = append(closeErrors, server.publications.Close())
	}
	if server.publicBuilds != nil {
		closeErrors = append(closeErrors, server.publicBuilds.Close())
	}
	if server.measurements != nil {
		closeErrors = append(closeErrors, server.measurements.Close())
	}
	if server.cloudStore != nil {
		closeErrors = append(closeErrors, server.cloudStore.Close())
	}
	if server.localStore != nil {
		closeErrors = append(closeErrors, server.localStore.Close())
	} else if server.store != nil {
		closeErrors = append(closeErrors, server.store.Close())
	}
	if server.telemetry != nil {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		closeErrors = append(closeErrors, server.telemetry.Shutdown(shutdownContext))
		cancel()
	}
	if server.runtimeLock != nil {
		closeErrors = append(closeErrors, server.runtimeLock.Close())
	}
	return errors.Join(closeErrors...)
}

func (server *Server) Handler() http.Handler {
	return requestReadDeadlineHandler(server.telemetry.Handler(server.mux))
}

// UpdateTeamToken rotates all long-lived Team Cache adapters together. The
// runtime supervisor persists the same credential before making it active.
func (server *Server) UpdateTeamToken(token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return errors.New("Team Cache token is empty")
	}
	if server.team != nil {
		if err := server.team.SetToken(token); err != nil {
			return err
		}
	}
	if server.actionsTeam != nil {
		if err := server.actionsTeam.SetToken(token); err != nil {
			return err
		}
	}
	server.teamTokenMu.Lock()
	server.teamToken = token
	server.teamTokenMu.Unlock()
	return nil
}

func (server *Server) currentTeamToken() string {
	server.teamTokenMu.RLock()
	defer server.teamTokenMu.RUnlock()
	return server.teamToken
}

func (server *Server) routes() {
	server.mux.HandleFunc("GET /healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"status":"ok"}`+"\n")
	})
	server.mux.HandleFunc("POST /v1/auth/github/exchange", server.githubCapabilityExchange)
	server.mux.HandleFunc("POST /v1/auth/github-oidc/exchange", server.githubOIDCCapabilityExchange)
	server.mux.Handle("GET /v1/status", server.requireToken(http.HandlerFunc(server.status)))
	server.mux.Handle("POST /v1/gc", server.requireToken(http.HandlerFunc(server.garbageCollect)))
	server.mux.Handle("GET /v1/reports", server.requireToken(http.HandlerFunc(server.periodReport)))
	server.mux.Handle("GET /v1/reports/{runID}", server.requireToken(http.HandlerFunc(server.runReport)))
	server.mux.Handle("POST /v1/reports/turbo", server.requireToken(http.HandlerFunc(server.reconcileTurboReport)))
	server.cloudRoutes()
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
		"evictionPolicy": server.config.EvictionPolicy,
		"runtimePid":     server.runtimePID, "runtimeInstanceId": server.runtimeInstanceID,
	}
	if server.storagePool.Path != "" {
		result["storagePool"] = server.storagePool.status()
	}
	if server.cloudStore != nil {
		result["storageBackend"] = "postgresql+s3"
		result["cloudMaintenanceHealthy"] = server.cloudMaintenanceHealthy()
		if quota, err := server.cloudStore.Quota(request.Context()); err == nil {
			result["maxBytes"] = quota.MaxBytes
			result["metadataMaxBytes"] = quota.MetadataMaxBytes
		}
	} else {
		result["storageBackend"] = "embedded"
		result["maxBytes"] = server.config.MaxBytes
	}
	result["telemetry"] = server.telemetry.State()
	result["telemetryInitializationDegraded"] = server.telemetryDegraded
	var pendingUploads, pendingUploadBytes int64
	queueStatsAvailable := false
	if server.uploads != nil {
		queued, queueErr := server.uploads.Stats(request.Context())
		if queueErr != nil {
			writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "read Team Cache upload status"})
			return
		}
		pendingUploads += queued.QueuedJobs
		pendingUploadBytes += queued.QueuedBytes
		queueStatsAvailable = true
	}
	if actionsQueue, ok := server.actionsStorage.(actionsTeamPublicationStatsReader); ok {
		queued, queueErr := actionsQueue.TeamPublicationStats(request.Context())
		if queueErr != nil {
			writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "read Actions Team Cache publication status"})
			return
		}
		pendingUploads += queued.PendingJobs
		pendingUploadBytes += queued.PendingBytes
		queueStatsAvailable = true
	}
	if queueStatsAvailable {
		result["pendingUploads"] = pendingUploads
		result["pendingUploadBytes"] = pendingUploadBytes
	}
	if server.publicBuilds != nil {
		if queue, queueErr := server.publicBuilds.Status(request.Context()); queueErr == nil {
			result["publicBuild"] = queue
			result["publicBuildStateAvailable"] = true
		} else {
			// Cache health remains observable when the queue backend is degraded.
			// The omitted summary makes the authenticated CLI report it unavailable.
			result["publicBuildStateAvailable"] = false
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
	maximum, err := server.cacheLimit(request.Context())
	if err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "cache quota unavailable"})
		return
	}
	targetBytes := maximum - maximum/5
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
	if targetBytes < 0 || targetBytes > maximum {
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
	values := request.Header.Values(compatibility.Header)
	claims, hasClaims := access.ClaimsFromContext(request.Context())
	if len(values) == 0 && hasClaims && claims.Compatibility != "" {
		return claims.Compatibility, nil
	}
	if len(values) != 1 {
		return "", errors.New("send exactly one compatibility selector")
	}
	if err := compatibility.Validate(values[0]); err != nil {
		return "", err
	}
	if hasClaims && claims.Compatibility != "" && claims.Compatibility != values[0] {
		return "", errors.New("compatibility selector is outside the authenticated authority")
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
	maximum, err := server.cacheLimit(request.Context())
	if err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "cache quota unavailable"})
		return
	}
	entry, _, err := server.store.Put(request.Context(), key, metadata, io.LimitReader(request.Body, maximum+1))
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
	resolution, err := server.resolveTurbo(request.Context(), key)
	if errors.Is(err, artifact.ErrNotFound) || errors.Is(err, artifact.ErrCorrupt) {
		if resolution.Degraded {
			writer.Header().Set("x-layercache-degraded", "remote-read-failed")
		}
		server.recordTurboLookupMiss(request, key, startedAt, time.Now().UTC(), resolution.Degraded)
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "cache read failed"})
		return
	}
	entry, file, source := resolution.Entry, resolution.Body, resolution.Source
	if resolution.Degraded {
		writer.Header().Set("x-layercache-degraded", "remote-read-failed")
	}
	if server.cloudStore != nil {
		cloudBody := file
		staged, cleanup, verifyErr := server.stageVerifiedTurboRead(request.Context(), entry, cloudBody)
		closeErr := cloudBody.Close()
		if verifyErr == nil {
			verifyErr = closeErr
		}
		if verifyErr != nil {
			if cleanup != nil {
				cleanup()
			}
			if errors.Is(verifyErr, artifact.ErrQuota) {
				writer.Header().Set("x-layercache-degraded", "verification-staging-full")
				writeJSON(writer, http.StatusInsufficientStorage, map[string]string{"error": "insufficient verification staging space"})
				return
			}
			if errors.Is(verifyErr, artifact.ErrCorrupt) {
				_ = server.store.Delete(context.WithoutCancel(request.Context()), key)
				writer.WriteHeader(http.StatusNotFound)
				return
			}
			writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "cache read failed"})
			return
		}
		file = staged
		defer cleanup()
	} else {
		defer file.Close()
	}
	setTurboHeaders(writer, entry, source)
	if head {
		writer.WriteHeader(http.StatusOK)
		return
	}
	writer.WriteHeader(http.StatusOK)
	bytesWritten, copyErr := io.Copy(writer, file)
	if copyErr == nil && bytesWritten == entry.Size {
		server.recordTurboHit(request, key, entry, source, startedAt, time.Now().UTC(), resolution.Degraded)
	}
}

func (server *Server) recordTurboLookupMiss(
	request *http.Request,
	key artifact.Key,
	startedAt time.Time,
	finishedAt time.Time,
	degraded bool,
) {
	runID := access.RunID(request.Context())
	if runID == "" {
		return
	}
	identity := measurement.TurboArtifactIdentity(key.Project, key.Compatibility, key.Native)
	_ = server.measurements.ObserveTurbo(measurement.TurboObservation{
		RunID: runID, ArtifactID: identity,
		Result: measurement.ResultMiss, Source: measurement.SourceNone,
		StartedAt: startedAt, FinishedAt: finishedAt,
		Timing: measurement.Timing{Lookup: finishedAt.Sub(startedAt)}, Degraded: degraded,
	})
}

func (server *Server) recordTurboMiss(request *http.Request, key artifact.Key, entry artifact.Entry, uploadStarted time.Time, degraded bool) {
	runID := access.RunID(request.Context())
	if runID == "" {
		return
	}
	finishedAt := time.Now().UTC()
	identity := measurement.TurboArtifactIdentity(key.Project, key.Compatibility, key.Native)
	_ = server.measurements.ObserveTurbo(measurement.TurboObservation{
		RunID: runID, ArtifactID: identity,
		Result: measurement.ResultMiss, Source: measurement.SourceNone,
		StartedAt: uploadStarted, FinishedAt: finishedAt,
		Timing: measurement.Timing{Upload: finishedAt.Sub(uploadStarted)},
		Bytes:  measurement.Bytes{Uploaded: entry.Size}, Degraded: degraded,
	})
}

func (server *Server) recordTurboHit(
	request *http.Request,
	key artifact.Key,
	entry artifact.Entry,
	source string,
	startedAt time.Time,
	finishedAt time.Time,
	degraded bool,
) {
	runID := access.RunID(request.Context())
	if runID == "" {
		return
	}
	identity := measurement.TurboArtifactIdentity(key.Project, key.Compatibility, key.Native)
	_ = server.measurements.ObserveTurbo(measurement.TurboObservation{
		RunID: runID, ArtifactID: identity,
		Result: measurement.ResultHit, Source: measurementSource(source),
		StartedAt: startedAt, FinishedAt: finishedAt,
		Timing: measurement.Timing{Download: finishedAt.Sub(startedAt)},
		Bytes:  measurement.Bytes{Downloaded: entry.Size}, Degraded: degraded,
	})
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

type turboResolution struct {
	Entry    artifact.Entry
	Body     io.ReadCloser
	Source   string
	Degraded bool
}

func (server *Server) resolveTurbo(ctx context.Context, key artifact.Key) (turboResolution, error) {
	degraded := false
	entry, file, err := server.store.Get(ctx, key)
	if err == nil {
		if entry.Metadata.Values["origin"] != "public" {
			return turboResolution{Entry: entry, Body: file, Source: server.config.Role}, nil
		}
		if server.config.Role != "local" {
			file.Close()
			_ = server.store.Delete(ctx, key)
			err = artifact.ErrNotFound
		} else {
			var checkErr error
			if server.public != nil {
				var publication publictrust.Publication
				publication, _, _, checkErr = server.public.Resolve(ctx, publictrust.Expected{
					Integration: key.Integration, Project: key.Project,
					Compatibility: key.Compatibility, NativeKey: key.Native,
				})
				if checkErr == nil && publication.Digest == entry.Digest {
					return turboResolution{Entry: entry, Body: file, Source: server.config.Role}, nil
				}
				if checkErr == nil {
					checkErr = publictrust.ErrIdentity
				}
			} else {
				checkErr = errors.Join(actionscache.ErrPublicOffline, errors.New("Public Cache endpoint is not configured"))
			}
			if checkErr != nil && !errors.Is(checkErr, remote.ErrMiss) {
				degraded = true
			}
			leaseExpires, leaseErr := time.Parse(time.RFC3339Nano, entry.Metadata.Values["leaseExpires"])
			trustFailure := errors.Is(checkErr, remote.ErrMiss) ||
				errors.Is(checkErr, publictrust.ErrSignature) ||
				errors.Is(checkErr, publictrust.ErrIdentity) ||
				errors.Is(checkErr, publictrust.ErrExpired)
			if !trustFailure && leaseErr == nil && time.Now().UTC().Before(leaseExpires) {
				return turboResolution{
					Entry: entry, Body: file, Source: server.config.Role, Degraded: degraded,
				}, nil
			}
			file.Close()
			_ = server.store.Delete(ctx, key)
			err = artifact.ErrNotFound
		}
	}
	if errors.Is(err, artifact.ErrCorrupt) {
		// A poisoned local record must not block a verified Team/Public repair.
		// Delete the logical reference before PutVerified publishes replacement
		// bytes, otherwise first-writer-wins correctly reports a conflict.
		_ = server.store.Delete(context.WithoutCancel(ctx), key)
		err = artifact.ErrNotFound
		degraded = true
	}
	if !errors.Is(err, artifact.ErrNotFound) && !errors.Is(err, artifact.ErrCorrupt) {
		return turboResolution{Degraded: degraded}, err
	}
	if server.team != nil {
		download, teamErr := server.team.Get(ctx, key.Native)
		if teamErr == nil {
			if download.ExpectedDigest != "" {
				_, _, storeErr := server.store.PutVerified(ctx, key, download.Metadata, download.Body, download.ExpectedDigest)
				_ = download.Body.Close()
				if storeErr == nil {
					entry, file, err = server.store.Get(ctx, key)
					if err == nil {
						return turboResolution{
							Entry: entry, Body: file, Source: "team", Degraded: degraded,
						}, nil
					}
					if ctx.Err() != nil {
						return turboResolution{Degraded: degraded}, ctx.Err()
					}
				} else if ctx.Err() != nil {
					return turboResolution{Degraded: degraded}, ctx.Err()
				}
				if errors.Is(storeErr, artifact.ErrConflict) {
					// A Local Build may have won while the remote bytes were in
					// flight. Preserve and serve that first-writer-wins result.
					entry, file, localErr := server.store.Get(ctx, key)
					if localErr == nil {
						return turboResolution{
							Entry: entry, Body: file, Source: server.config.Role, Degraded: degraded,
						}, nil
					}
				}
				// A malformed or corrupt Team response is a degraded miss. The
				// verified Public source may still repair the original absence.
				degraded = true
			} else {
				_ = download.Body.Close()
				degraded = true
			}
		} else if ctx.Err() != nil {
			return turboResolution{Degraded: degraded}, ctx.Err()
		} else if !errors.Is(teamErr, remote.ErrMiss) {
			degraded = true
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
				if errors.Is(err, artifact.ErrConflict) {
					entry, file, localErr := server.store.Get(ctx, key)
					if localErr == nil {
						return turboResolution{
							Entry: entry, Body: file, Source: server.config.Role, Degraded: degraded,
						}, nil
					}
				}
				if ctx.Err() != nil {
					return turboResolution{Degraded: degraded}, ctx.Err()
				}
				return turboResolution{Degraded: true}, artifact.ErrNotFound
			}
			if stored.Size != download.Publication.Size {
				_ = server.store.Delete(context.WithoutCancel(ctx), key)
				return turboResolution{Degraded: true}, artifact.ErrCorrupt
			}
			entry, file, err = server.store.Get(ctx, key)
			if err != nil {
				return turboResolution{Degraded: degraded}, err
			}
			return turboResolution{
				Entry: entry, Body: file, Source: "public", Degraded: degraded,
			}, nil
		}
		if ctx.Err() != nil {
			return turboResolution{Degraded: degraded}, ctx.Err()
		}
		if !errors.Is(publicErr, remote.ErrMiss) {
			degraded = true
		}
	}
	return turboResolution{Degraded: degraded}, artifact.ErrNotFound
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
	return RunHTTPWithReady(ctx, cfg, nil)
}

// RunHTTPWithReady exposes the initialized runtime after its listener is bound
// so a process supervisor can rotate expiring remote credentials without
// reporting readiness for an address that cannot accept requests.
func RunHTTPWithReady(ctx context.Context, cfg config.Config, ready func(*Server)) error {
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
		MaxHeaderBytes:    64 << 10,
	}
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("serve Layer Cache at %s: %w", cfg.Listen, err)
	}
	defer listener.Close()
	shutdownDone := make(chan struct{})
	go func() {
		<-ctx.Done()
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownContext)
		close(shutdownDone)
	}()
	if ready != nil {
		ready(cacheServer)
	}
	if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve Layer Cache at %s: %w", cfg.Listen, err)
	}
	if ctx.Err() != nil {
		<-shutdownDone
	}
	return nil
}
