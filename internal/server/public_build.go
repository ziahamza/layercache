package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/layercache/layercache/internal/artifact"
	"github.com/layercache/layercache/internal/config"
	"github.com/layercache/layercache/internal/publicbuild"
	"github.com/layercache/layercache/internal/publictrust"
)

const publicBuildRequestLimit = 64 << 10

var (
	publicBuildSensitiveLogValue = regexp.MustCompile(`(?i)(authorization|bearer|cookie|password|secret|token)([=:][[:space:]]*|[[:space:]]+)([^[:space:]]+)`)
	publicBuildWorkspacePath     = regexp.MustCompile(`(?:/home|/Users|/workspace|/workspaces|/private/tmp|/tmp)/[^[:space:]]+`)
	publicBuildWorkerID          = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
)

type publicBuildRequestBody struct {
	Repository   string                      `json:"repository"`
	Commit       string                      `json:"commit"`
	Integration  publicbuild.Integration     `json:"integration"`
	Target       string                      `json:"target"`
	RecipeDigest string                      `json:"recipeDigest"`
	Platform     publicbuild.Platform        `json:"platform"`
	Inputs       []publicbuild.DeclaredInput `json:"inputs,omitempty"`
	Resources    publicBuildResourcesBody    `json:"resources"`
}

type publicBuildResourcesBody struct {
	CPUMillis           int64 `json:"cpuMillis"`
	MemoryBytes         int64 `json:"memoryBytes"`
	DiskBytes           int64 `json:"diskBytes"`
	TimeoutMilliseconds int64 `json:"timeoutMilliseconds"`
}

type publicBuildRequestResponse struct {
	Build  publicBuildResponse `json:"build"`
	Reused bool                `json:"reused"`
}

type publicBuildResponse struct {
	ID          string                          `json:"id"`
	Request     publicBuildRequestBody          `json:"request"`
	State       publicbuild.State               `json:"state"`
	WorkerID    string                          `json:"workerId,omitempty"`
	RequestedAt time.Time                       `json:"requestedAt"`
	StartedAt   *time.Time                      `json:"startedAt,omitempty"`
	FinishedAt  *time.Time                      `json:"finishedAt,omitempty"`
	Publication *publicBuildPublicationResponse `json:"publication,omitempty"`
	Failure     string                          `json:"failure,omitempty"`
}

type publicBuildPublicationResponse struct {
	PublicCachePublication       string `json:"publicCachePublication"`
	NativeKey                    string `json:"nativeKey,omitempty"`
	PublicImportSelector         string `json:"publicImportSelector,omitempty"`
	Digest                       string `json:"digest"`
	SizeBytes                    int64  `json:"sizeBytes"`
	MediaType                    string `json:"mediaType"`
	ProducerDurationMilliseconds int64  `json:"producerDurationMilliseconds"`
}

type publicBuildWorkerLeaseBody struct {
	WorkerID     string                               `json:"workerId"`
	Integrations []publicbuild.Integration            `json:"integrations"`
	Platforms    []publicbuild.Platform               `json:"platforms"`
	Recipes      []publicbuild.WorkerRecipeCapability `json:"recipes,omitempty"`
}

type publicBuildWorkerLeaseResponse struct {
	LeaseToken string              `json:"leaseToken"`
	WorkerID   string              `json:"workerId"`
	LeasedAt   time.Time           `json:"leasedAt"`
	ExpiresAt  time.Time           `json:"expiresAt"`
	Build      publicBuildResponse `json:"build"`
}

type publicBuildLeaseBody struct {
	LeaseToken string `json:"leaseToken"`
	WorkerID   string `json:"workerId"`
}

type publicBuildAppendLogBody struct {
	publicBuildLeaseBody
	Message string `json:"message"`
}

type publicBuildCompleteBody struct {
	publicBuildLeaseBody
	PublicationIdentity string `json:"publicationIdentity"`
}

type publicBuildFailBody struct {
	publicBuildLeaseBody
	Reason string `json:"reason"`
}

type publicBuildLogsResponse struct {
	BuildID string                   `json:"buildId"`
	Logs    []publicBuildLogResponse `json:"logs"`
}

type publicBuildLogResponse struct {
	Sequence  uint64    `json:"sequence"`
	Timestamp time.Time `json:"timestamp"`
	Message   string    `json:"message"`
}

func publicBuildConfig(
	cfg config.Config,
	publications publicationRegistry,
	store cacheStore,
	builds func() publicBuildCoordinator,
) publicbuild.Config {
	secrets := []string{
		cfg.PublisherToken, cfg.PublicBuildWorkerToken, cfg.PublicCollectorToken,
		cfg.LocalToken, cfg.PublicPrivateKey, cfg.TeamToken,
	}
	recipePolicy, _ := publicbuild.NewRecipeAllowlist(cfg.PublicBuildRecipeDigests)
	return publicbuild.Config{
		AllowlistedRepositories: append([]string(nil), cfg.PublicBuildRepositories...),
		Limits: publicbuild.Resources{
			CPUMillis:   8_000,
			MemoryBytes: 16 << 30,
			DiskBytes:   50 << 30,
			Timeout:     time.Hour,
		},
		SanitizeLog: func(message string) string {
			for _, secret := range secrets {
				if secret != "" {
					message = strings.ReplaceAll(message, secret, "[REDACTED]")
				}
			}
			message = publicBuildSensitiveLogValue.ReplaceAllString(message, "$1$2[REDACTED]")
			return publicBuildWorkspacePath.ReplaceAllString(message, "[WORKSPACE]")
		},
		SourcePolicy: publicbuild.NewGitHubSourcePolicy(publicbuild.GitHubSourcePolicyOptions{
			BaseURL: cfg.GitHubAPIURL, ApprovedRefs: cfg.PublicBuildApprovedRefs,
		}),
		RecipePolicy: recipePolicy,
		Publications: registryPublicationIndex{
			registry: publications, store: store, builds: builds,
			accept: func(publication publictrust.Publication) bool {
				return publicBuilderImageMatchesConfig(cfg, publication.BuilderImageDigest)
			},
		},
	}
}

type registryPublicationIndex struct {
	registry publicationRegistry
	store    cacheStore
	builds   func() publicBuildCoordinator
	accept   func(publictrust.Publication) bool
}

func (index registryPublicationIndex) Find(ctx context.Context, request publicbuild.BuildRequest) (publicbuild.ExistingPublication, error) {
	if index.registry == nil {
		return publicbuild.ExistingPublication{}, publicbuild.ErrPublicationNotFound
	}
	publication, err := index.registry.FindBuild(ctx, publictrust.BuildIdentity{
		Repository: request.Repository, Commit: request.Commit,
		Integration: string(request.Integration), Target: request.Target,
		RecipeDigest: request.RecipeDigest, Platform: string(request.Platform),
		Inputs: publicTrustInputs(request.Inputs),
	})
	if errors.Is(err, publictrust.ErrNotFound) || errors.Is(err, publictrust.ErrExpired) ||
		errors.Is(err, publictrust.ErrRevoked) || errors.Is(err, publictrust.ErrAmbiguous) {
		return publicbuild.ExistingPublication{}, publicbuild.ErrPublicationNotFound
	}
	if err != nil {
		return publicbuild.ExistingPublication{}, err
	}
	if index.accept != nil && !index.accept(publication) {
		// A publication produced by a previously reviewed image can remain
		// resolvable, but it must not suppress a build after the configured
		// kernel/rootfs/contract set rotates.
		return publicbuild.ExistingPublication{}, publicbuild.ErrPublicationNotFound
	}
	if index.builds != nil {
		builds := index.builds()
		if builds == nil {
			return publicbuild.ExistingPublication{}, errors.New("Public Build coordinator is unavailable")
		}
		build, inspectErr := builds.Inspect(ctx, publication.BuildID)
		if errors.Is(inspectErr, publicbuild.ErrNotFound) ||
			inspectErr == nil && !completedPublicBuildMatchesRetry(build, publication) {
			return publicbuild.ExistingPublication{}, index.retireUnavailable(ctx, publication)
		}
		if inspectErr != nil {
			return publicbuild.ExistingPublication{}, inspectErr
		}
	}
	available, err := verifiedPublicArtifact(ctx, index.store, publication)
	if err != nil {
		return publicbuild.ExistingPublication{}, err
	}
	if !available {
		return publicbuild.ExistingPublication{}, index.retireUnavailable(ctx, publication)
	}
	return publicbuild.ExistingPublication{
		Identity: publication.Identity(), BuildID: publication.BuildID,
		Digest: "sha256:" + publication.Digest, SizeBytes: publication.Size,
		MediaType:        publicBuildMediaType(publication.Integration),
		ProducerDuration: time.Duration(publication.DurationMS) * time.Millisecond,
	}, nil
}

func (index registryPublicationIndex) retireUnavailable(
	ctx context.Context,
	publication publictrust.Publication,
) error {
	if err := retireInvalidPublicPublication(ctx, index.registry, index.store, publication); err != nil {
		return fmt.Errorf("retire unusable Public Cache publication: %w", err)
	}
	return publicbuild.ErrPublicationUnavailable
}

func (server *Server) publicBuildRoutes() {
	if server.config.Role != "public" {
		return
	}
	server.mux.Handle("POST /v1/public-builds", server.requireToken(http.HandlerFunc(server.requestPublicBuild)))
	server.mux.Handle("GET /v1/public-builds/{id}", server.requireToken(http.HandlerFunc(server.inspectPublicBuild)))
	server.mux.Handle("GET /v1/public-builds/{id}/logs", server.requireToken(http.HandlerFunc(server.publicBuildLogs)))
	server.mux.Handle("POST /v1/public-builds/{id}/cancel", server.requireToken(http.HandlerFunc(server.cancelPublicBuild)))

	workerCredential := server.config.PublicBuildWorkerToken
	collectorCredential := server.config.PublicCollectorToken
	server.mux.Handle("POST /v1/public-build-worker/lease", server.requirePublicCredential(workerCredential, http.HandlerFunc(server.leasePublicBuild)))
	server.mux.Handle("GET /v1/public-build-worker/{id}", server.requirePublicCredential(workerCredential, http.HandlerFunc(server.inspectWorkerPublicBuild)))
	server.mux.Handle("POST /v1/public-build-worker/{id}/heartbeat", server.requirePublicCredential(workerCredential, http.HandlerFunc(server.renewPublicBuildLease)))
	server.mux.Handle("POST /v1/public-build-worker/{id}/logs", server.requirePublicCredential(workerCredential, http.HandlerFunc(server.appendPublicBuildLog)))
	server.mux.Handle("POST /v1/public-build-worker/{id}/complete", server.requirePublicCredential(collectorCredential, http.HandlerFunc(server.completePublicBuild)))
	server.mux.Handle("POST /v1/public-build-worker/{id}/fail", server.requirePublicCredential(workerCredential, http.HandlerFunc(server.failPublicBuild)))
}

func (server *Server) inspectWorkerPublicBuild(writer http.ResponseWriter, request *http.Request) {
	if !server.publicBuildAvailable(writer) {
		return
	}
	build, err := server.publicBuilds.Inspect(request.Context(), request.PathValue("id"))
	if !writePublicBuildLookupError(writer, err) {
		return
	}
	writeJSON(writer, http.StatusOK, server.publicBuildView(build))
}

func (server *Server) renewPublicBuildLease(writer http.ResponseWriter, request *http.Request) {
	if !server.publicBuildAvailable(writer) {
		return
	}
	var body publicBuildLeaseBody
	if err := decodePublicBuildJSON(request, &body); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid Public Build heartbeat"})
		return
	}
	lease, err := server.publicBuilds.Renew(request.Context(), publicBuildLease(request.PathValue("id"), body))
	if !writePublicBuildLeaseError(writer, err) {
		return
	}
	writeJSON(writer, http.StatusOK, publicBuildWorkerLeaseResponse{
		LeaseToken: lease.Token, WorkerID: lease.WorkerID, LeasedAt: lease.LeasedAt,
		ExpiresAt: lease.ExpiresAt,
		Build:     server.publicBuildView(lease.Build),
	})
}

func (server *Server) requestPublicBuild(writer http.ResponseWriter, request *http.Request) {
	if !server.publicBuildAvailable(writer) {
		return
	}
	var body publicBuildRequestBody
	if err := decodePublicBuildJSON(request, &body); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid Public Build request"})
		return
	}
	resources, err := body.Resources.domain()
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	result, err := server.publicBuilds.Request(request.Context(), publicbuild.BuildRequest{
		Repository: body.Repository, Commit: body.Commit, Integration: body.Integration,
		Target: body.Target, RecipeDigest: body.RecipeDigest, Platform: body.Platform,
		Inputs:    body.Inputs,
		Resources: resources,
	})
	if errors.Is(err, publicbuild.ErrRejected) {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if errors.Is(err, publicbuild.ErrPublicationPending) {
		writer.Header().Set("Retry-After", "1")
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "request Public Build"})
		return
	}
	status := http.StatusAccepted
	if result.Reused {
		status = http.StatusOK
	}
	writeJSON(writer, status, publicBuildRequestResponse{Build: server.publicBuildView(result.Build), Reused: result.Reused})
}

func (server *Server) inspectPublicBuild(writer http.ResponseWriter, request *http.Request) {
	if !server.publicBuildAvailable(writer) {
		return
	}
	build, err := server.publicBuilds.Inspect(request.Context(), request.PathValue("id"))
	if !writePublicBuildLookupError(writer, err) {
		return
	}
	writeJSON(writer, http.StatusOK, server.publicBuildView(build))
}

func (server *Server) publicBuildLogs(writer http.ResponseWriter, request *http.Request) {
	if !server.publicBuildAvailable(writer) {
		return
	}
	logs, err := server.publicBuilds.Logs(request.Context(), request.PathValue("id"))
	if !writePublicBuildLookupError(writer, err) {
		return
	}
	response := publicBuildLogsResponse{BuildID: request.PathValue("id"), Logs: make([]publicBuildLogResponse, 0, len(logs))}
	for _, entry := range logs {
		response.Logs = append(response.Logs, publicBuildLogResponse(entry))
	}
	writeJSON(writer, http.StatusOK, response)
}

func (server *Server) cancelPublicBuild(writer http.ResponseWriter, request *http.Request) {
	if !server.publicBuildAvailable(writer) {
		return
	}
	build, err := server.publicBuilds.Cancel(request.Context(), request.PathValue("id"))
	if errors.Is(err, publicbuild.ErrNotFound) {
		writeJSON(writer, http.StatusNotFound, map[string]string{"error": "Public Build not found"})
		return
	}
	if errors.Is(err, publicbuild.ErrInvalidTransition) {
		writeJSON(writer, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "cancel Public Build"})
		return
	}
	writeJSON(writer, http.StatusOK, server.publicBuildView(build))
}

func (server *Server) leasePublicBuild(writer http.ResponseWriter, request *http.Request) {
	if !server.publicBuildAvailable(writer) {
		return
	}
	var body publicBuildWorkerLeaseBody
	if err := decodePublicBuildJSON(request, &body); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid Public Build worker lease"})
		return
	}
	if !publicBuildWorkerID.MatchString(body.WorkerID) {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid Public Build worker ID"})
		return
	}
	worker := &publicBuildCapabilityWorker{id: body.WorkerID, capabilities: publicbuild.WorkerCapabilities{
		Integrations: body.Integrations, Platforms: body.Platforms, Recipes: body.Recipes,
	}}
	lease, err := server.publicBuilds.LeaseNext(request.Context(), worker)
	if errors.Is(err, publicbuild.ErrNoWork) {
		writer.WriteHeader(http.StatusNoContent)
		return
	}
	if errors.Is(err, publicbuild.ErrRejected) {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "lease Public Build"})
		return
	}
	writeJSON(writer, http.StatusOK, publicBuildWorkerLeaseResponse{
		LeaseToken: lease.Token, WorkerID: lease.WorkerID, LeasedAt: lease.LeasedAt,
		ExpiresAt: lease.ExpiresAt,
		Build:     server.publicBuildView(lease.Build),
	})
}

func (server *Server) appendPublicBuildLog(writer http.ResponseWriter, request *http.Request) {
	if !server.publicBuildAvailable(writer) {
		return
	}
	var body publicBuildAppendLogBody
	if err := decodePublicBuildJSON(request, &body); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid Public Build log"})
		return
	}
	lease := publicBuildLease(request.PathValue("id"), body.publicBuildLeaseBody)
	if err := server.publicBuilds.AppendLog(request.Context(), lease, body.Message); !writePublicBuildLeaseError(writer, err) {
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (server *Server) completePublicBuild(writer http.ResponseWriter, request *http.Request) {
	if !server.publicBuildAvailable(writer) {
		return
	}
	var body publicBuildCompleteBody
	if err := decodePublicBuildJSON(request, &body); err != nil || body.PublicationIdentity == "" {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid Public Build completion"})
		return
	}
	build, err := server.publicBuilds.Inspect(request.Context(), request.PathValue("id"))
	if !writePublicBuildLookupError(writer, err) {
		return
	}
	publication, err := server.publications.Resolve(request.Context(), body.PublicationIdentity)
	if err != nil || !publicBuildPublicationMatches(server.config, build, body.PublicationIdentity, publication) {
		writeJSON(writer, http.StatusConflict, map[string]string{"error": "Public Cache publication does not match Public Build"})
		return
	}
	entry, err := server.store.Head(request.Context(), artifact.Key{
		Integration: publication.Integration, Project: publication.Project,
		Compatibility: publication.Compatibility, Native: publication.NativeKey,
	})
	if err != nil || entry.Digest != publication.Digest || entry.Size != publication.Size {
		writeJSON(writer, http.StatusConflict, map[string]string{"error": "Public Cache publication artifact is unavailable"})
		return
	}
	if publication.DurationMS > math.MaxInt64/int64(time.Millisecond) {
		writeJSON(writer, http.StatusConflict, map[string]string{"error": "Public Cache publication duration is invalid"})
		return
	}
	if build.State == publicbuild.StateSucceeded {
		if build.Publication == nil || len(build.Publication.Outputs) != 1 ||
			build.Publication.Outputs[0].Name != body.PublicationIdentity {
			writeJSON(writer, http.StatusConflict, map[string]string{"error": "Public Build completion does not match prior publication"})
			return
		}
		writeJSON(writer, http.StatusOK, server.publicBuildView(build))
		return
	}
	permit, err := server.publicBuilds.BeginLeasedPublication(
		request.Context(), publicBuildLease(build.ID, body.publicBuildLeaseBody),
	)
	if err != nil {
		writeJSON(writer, http.StatusConflict, map[string]string{"error": "Public Build cannot publish in its current state"})
		return
	}
	completed, err := server.publicBuilds.CommitPublication(
		request.Context(), permit, publicbuild.Publication{
			Outputs: []publicbuild.OutputDescriptor{{
				Name: body.PublicationIdentity, Digest: "sha256:" + publication.Digest,
				SizeBytes: publication.Size, MediaType: publicBuildMediaType(publication.Integration),
			}},
			ProducerDuration: time.Duration(publication.DurationMS) * time.Millisecond,
		},
	)
	if err != nil {
		_, _ = server.publicBuilds.AbortPublication(context.WithoutCancel(request.Context()), permit, "trusted collection did not finish")
	}
	if !writePublicBuildPublicationError(writer, err) {
		return
	}
	writeJSON(writer, http.StatusOK, server.publicBuildView(completed))
}

func writePublicBuildPublicationError(writer http.ResponseWriter, err error) bool {
	if err == nil {
		return true
	}
	switch {
	case errors.Is(err, publicbuild.ErrNotFound):
		writeJSON(writer, http.StatusNotFound, map[string]string{"error": "Public Build not found"})
	case errors.Is(err, publicbuild.ErrPublicationLost), errors.Is(err, publicbuild.ErrInvalidTransition):
		writeJSON(writer, http.StatusConflict, map[string]string{"error": "Public Build publication is no longer active"})
	case errors.Is(err, publicbuild.ErrRejected):
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": err.Error()})
	default:
		writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "publish Public Build"})
	}
	return false
}

func (server *Server) failPublicBuild(writer http.ResponseWriter, request *http.Request) {
	if !server.publicBuildAvailable(writer) {
		return
	}
	var body publicBuildFailBody
	if err := decodePublicBuildJSON(request, &body); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid Public Build failure"})
		return
	}
	build, err := server.publicBuilds.Fail(
		request.Context(), publicBuildLease(request.PathValue("id"), body.publicBuildLeaseBody), body.Reason,
	)
	if errors.Is(err, publicbuild.ErrRejected) {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if !writePublicBuildLeaseError(writer, err) {
		return
	}
	writeJSON(writer, http.StatusOK, server.publicBuildView(build))
}

func (server *Server) publicBuildAvailable(writer http.ResponseWriter) bool {
	if server.publicBuilds == nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "Public Builds are not configured"})
		return false
	}
	return true
}

func writePublicBuildLookupError(writer http.ResponseWriter, err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, publicbuild.ErrNotFound) {
		writeJSON(writer, http.StatusNotFound, map[string]string{"error": "Public Build not found"})
	} else {
		writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "read Public Build"})
	}
	return false
}

func writePublicBuildLeaseError(writer http.ResponseWriter, err error) bool {
	if err == nil {
		return true
	}
	switch {
	case errors.Is(err, publicbuild.ErrNotFound):
		writeJSON(writer, http.StatusNotFound, map[string]string{"error": "Public Build not found"})
	case errors.Is(err, publicbuild.ErrLeaseLost):
		writeJSON(writer, http.StatusConflict, map[string]string{"error": "Public Build lease is no longer active"})
	case errors.Is(err, publicbuild.ErrRejected):
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": err.Error()})
	default:
		writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "update Public Build"})
	}
	return false
}

func decodePublicBuildJSON(request *http.Request, destination any) error {
	defer request.Body.Close()
	encoded, err := io.ReadAll(io.LimitReader(request.Body, publicBuildRequestLimit+1))
	if err != nil {
		return err
	}
	if len(encoded) == 0 || len(encoded) > publicBuildRequestLimit {
		return errors.New("Public Build request body is empty or too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("Public Build request has trailing JSON")
	}
	return nil
}

func (resources publicBuildResourcesBody) domain() (publicbuild.Resources, error) {
	if resources.TimeoutMilliseconds <= 0 || resources.TimeoutMilliseconds > math.MaxInt64/int64(time.Millisecond) {
		return publicbuild.Resources{}, errors.New("timeoutMilliseconds must be a positive duration")
	}
	return publicbuild.Resources{
		CPUMillis: resources.CPUMillis, MemoryBytes: resources.MemoryBytes,
		DiskBytes: resources.DiskBytes, Timeout: time.Duration(resources.TimeoutMilliseconds) * time.Millisecond,
	}, nil
}

func (server *Server) publicBuildView(build publicbuild.Build) publicBuildResponse {
	response := publicBuildResponse{
		ID: build.ID,
		Request: publicBuildRequestBody{
			Repository: build.Request.Repository, Commit: build.Request.Commit,
			Integration: build.Request.Integration, Target: build.Request.Target,
			RecipeDigest: build.Request.RecipeDigest, Platform: build.Request.Platform,
			Inputs: append([]publicbuild.DeclaredInput(nil), build.Request.Inputs...),
			Resources: publicBuildResourcesBody{
				CPUMillis: build.Request.Resources.CPUMillis, MemoryBytes: build.Request.Resources.MemoryBytes,
				DiskBytes:           build.Request.Resources.DiskBytes,
				TimeoutMilliseconds: build.Request.Resources.Timeout.Milliseconds(),
			},
		},
		State: build.State, WorkerID: build.WorkerID, RequestedAt: build.RequestedAt,
		Failure: build.Failure,
	}
	if !build.StartedAt.IsZero() {
		started := build.StartedAt
		response.StartedAt = &started
	}
	if !build.FinishedAt.IsZero() {
		finished := build.FinishedAt
		response.FinishedAt = &finished
	}
	if build.Publication != nil && len(build.Publication.Outputs) == 1 {
		output := build.Publication.Outputs[0]
		nativeKey := ""
		switch build.Request.Integration {
		case publicbuild.IntegrationBuildKit:
			nativeKey, _ = publicbuild.BuildKitPublicNativeKey(build.Request)
		case publicbuild.IntegrationActions:
			nativeKey, _ = publicbuild.ActionsPublicNativeKey(
				build.Request,
				actionsPublicToolchain,
				server.config.ActionsPublicBuilder,
			)
		}
		response.Publication = &publicBuildPublicationResponse{
			PublicCachePublication: output.Name, NativeKey: nativeKey, Digest: output.Digest,
			SizeBytes: output.SizeBytes, MediaType: output.MediaType,
			ProducerDurationMilliseconds: build.Publication.ProducerDuration.Milliseconds(),
		}
		if build.Request.Integration == publicbuild.IntegrationBuildKit && nativeKey != "" {
			response.Publication.PublicImportSelector = nativeKey + "=" + output.Name
		}
	}
	return response
}

func publicBuildLease(id string, body publicBuildLeaseBody) publicbuild.Lease {
	return publicbuild.Lease{Token: body.LeaseToken, WorkerID: body.WorkerID, Build: publicbuild.Build{ID: id}}
}

func publicBuildPublicationMatches(cfg config.Config, build publicbuild.Build, identity string, publication publictrust.Publication) bool {
	if !publicBuilderImageMatchesConfig(cfg, publication.BuilderImageDigest) {
		return false
	}
	compatibility, err := publicbuild.CompatibilityIdentity(build.Request)
	if err != nil {
		return false
	}
	project, err := publicbuild.PublicationProjectIdentity(
		build.Request.Integration,
		cfg.ProjectID,
		cfg.ActionsRepository,
		build.Request.Repository,
	)
	if err != nil {
		return false
	}
	var expectedNativeKey string
	switch build.Request.Integration {
	case publicbuild.IntegrationBuildKit:
		expectedNativeKey, err = publicbuild.BuildKitPublicNativeKey(build.Request)
	case publicbuild.IntegrationActions:
		if publication.Toolchain != actionsPublicToolchain ||
			publication.Builder != cfg.ActionsPublicBuilder ||
			build.Request.RecipeDigest != cfg.ActionsPublicRecipeDigest {
			return false
		}
		expectedNativeKey, err = publicbuild.ActionsPublicNativeKey(
			build.Request,
			actionsPublicToolchain,
			cfg.ActionsPublicBuilder,
		)
	}
	if err != nil || expectedNativeKey != "" && publication.NativeKey != expectedNativeKey {
		return false
	}
	return publication.Identity() == identity &&
		publication.BuildID == build.ID &&
		publication.Repository == build.Request.Repository &&
		publication.Commit == build.Request.Commit &&
		publication.RecipeDigest == build.Request.RecipeDigest &&
		publication.Target == build.Request.Target &&
		publication.Platform == string(build.Request.Platform) &&
		publication.Integration == string(build.Request.Integration) &&
		publicTrustInputsEqual(publication.Inputs, build.Request.Inputs) &&
		publication.Project == project &&
		publication.Compatibility == compatibility
}

func publicTrustInputs(inputs []publicbuild.DeclaredInput) []publictrust.DeclaredInput {
	result := make([]publictrust.DeclaredInput, len(inputs))
	for index, input := range inputs {
		result[index] = publictrust.DeclaredInput{Name: input.Name, Value: input.Value}
	}
	return result
}

func publicTrustInputsEqual(trustInputs []publictrust.DeclaredInput, buildInputs []publicbuild.DeclaredInput) bool {
	if len(trustInputs) != len(buildInputs) {
		return false
	}
	for index := range trustInputs {
		if trustInputs[index].Name != buildInputs[index].Name || trustInputs[index].Value != buildInputs[index].Value {
			return false
		}
	}
	return true
}

func publicBuildMediaType(integration string) string {
	switch integration {
	case string(publicbuild.IntegrationTurbo):
		return "application/vnd.layercache.turbo"
	case string(publicbuild.IntegrationBuildKit):
		return "application/vnd.oci.image.manifest.v1+json"
	case string(publicbuild.IntegrationActions):
		return "application/vnd.layercache.actions-cache"
	default:
		return "application/octet-stream"
	}
}

type publicBuildCapabilityWorker struct {
	id           string
	capabilities publicbuild.WorkerCapabilities
}

func (worker *publicBuildCapabilityWorker) ID() string {
	return worker.id
}

func (worker *publicBuildCapabilityWorker) Capabilities() publicbuild.WorkerCapabilities {
	return worker.capabilities
}

func (worker *publicBuildCapabilityWorker) Execute(context.Context, publicbuild.Build, publicbuild.LogSink) (publicbuild.Publication, error) {
	return publicbuild.Publication{}, fmt.Errorf("HTTP capability worker cannot execute Public Builds in process")
}

var _ publicbuild.Worker = (*publicBuildCapabilityWorker)(nil)
