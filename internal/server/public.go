package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/layercache/layercache/internal/artifact"
	"github.com/layercache/layercache/internal/config"
	"github.com/layercache/layercache/internal/publicbuild"
	"github.com/layercache/layercache/internal/publictrust"
)

type publicResolveRequest = publictrust.CacheCoordinate

type publicResolveResponse struct {
	Envelope    publictrust.Envelope `json:"envelope"`
	ArtifactURL string               `json:"artifactUrl"`
}

type publicRevokeRequest struct {
	publictrust.CacheCoordinate
	Reason string `json:"reason"`
}

const publicPublicationPinNamespace = "public-publication"

func publicArtifactKey(publication publictrust.Publication) artifact.Key {
	return artifact.Key{
		Integration: publication.Integration, Project: publication.Project,
		Compatibility: publication.Compatibility, Native: publication.NativeKey,
	}
}

func publicArtifactPin(publication publictrust.Publication) artifact.Pin {
	return artifact.Pin{
		Owner: publication.CacheIdentity(), Key: publicArtifactKey(publication),
		Digest: publication.Digest, Size: publication.Size,
	}
}

func (server *Server) reconcilePublicPins(ctx context.Context, registry publicationRegistry) error {
	publications, err := registry.ActivePublications(ctx)
	if err != nil {
		return err
	}
	pins := make([]artifact.Pin, 0, len(publications))
	for _, publication := range publications {
		pins = append(pins, publicArtifactPin(publication))
	}
	if _, err := server.store.ReconcilePins(ctx, publicPublicationPinNamespace, pins); err != nil {
		return fmt.Errorf("reconcile Public Cache artifact pins: %w", err)
	}
	return nil
}

func (server *Server) pinPublicArtifact(ctx context.Context, publication publictrust.Publication) error {
	if err := server.store.Pin(ctx, publicPublicationPinNamespace, publicArtifactPin(publication)); err != nil {
		return fmt.Errorf("pin Public Cache artifact: %w", err)
	}
	return nil
}

func (server *Server) releasePublicArtifact(ctx context.Context, cacheIdentity string, key artifact.Key) error {
	return releasePublicArtifactFromStore(ctx, server.store, cacheIdentity, key)
}

func releasePublicArtifactFromStore(
	ctx context.Context,
	store cacheStore,
	cacheIdentity string,
	key artifact.Key,
) error {
	if store == nil {
		return errors.New("Public Cache artifact store is unavailable")
	}
	unpinErr := store.Unpin(ctx, publicPublicationPinNamespace, cacheIdentity)
	deleteErr := store.Delete(ctx, key)
	if errors.Is(deleteErr, artifact.ErrNotFound) {
		deleteErr = nil
	}
	return errors.Join(unpinErr, deleteErr)
}

func retireInvalidPublicPublication(
	ctx context.Context,
	registry publicationRegistry,
	store cacheStore,
	publication publictrust.Publication,
) error {
	err := registry.Retire(
		ctx,
		publication.CacheIdentity(),
		publication.Identity(),
		"Public Cache publication lost its completed build or artifact",
		time.Now().UTC(),
	)
	if err != nil {
		if errors.Is(err, publictrust.ErrNotFound) {
			return nil
		}
		if !errors.Is(err, publictrust.ErrRevoked) && !errors.Is(err, publictrust.ErrAmbiguous) {
			return err
		}
	}
	return releasePublicArtifactFromStore(
		ctx, store, publication.CacheIdentity(), publicArtifactKey(publication),
	)
}

func verifiedPublicArtifact(
	ctx context.Context,
	store cacheStore,
	publication publictrust.Publication,
) (bool, error) {
	if store == nil {
		return false, errors.New("Public Cache artifact store is unavailable")
	}
	entry, body, err := store.Get(ctx, publicArtifactKey(publication))
	if errors.Is(err, artifact.ErrNotFound) || errors.Is(err, artifact.ErrCorrupt) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if entry.Digest != publication.Digest || entry.Size != publication.Size {
		return false, body.Close()
	}
	_, readErr := io.Copy(io.Discard, body)
	closeErr := body.Close()
	if errors.Is(readErr, artifact.ErrNotFound) || errors.Is(readErr, artifact.ErrCorrupt) ||
		errors.Is(closeErr, artifact.ErrNotFound) || errors.Is(closeErr, artifact.ErrCorrupt) {
		return false, nil
	}
	if err := errors.Join(readErr, closeErr); err != nil {
		return false, err
	}
	return true, nil
}

func (server *Server) verifiedPublicPublication(
	ctx context.Context,
	publication publictrust.Publication,
) (bool, error) {
	if server.publicBuilds != nil {
		build, err := server.publicBuilds.Inspect(ctx, publication.BuildID)
		if errors.Is(err, publicbuild.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("inspect Public Build for publication: %w", err)
		}
		if !completedPublicBuildMatchesRetry(build, publication) {
			return false, nil
		}
	}
	return verifiedPublicArtifact(ctx, server.store, publication)
}

func exactActivePublication(active, intended publictrust.Publication) bool {
	return active.Identity() == intended.Identity() &&
		active.CacheIdentity() == intended.CacheIdentity() &&
		active.DurationMS == intended.DurationMS
}

func definitivelyNoActivePublication(err error) bool {
	return errors.Is(err, publictrust.ErrNotFound) ||
		errors.Is(err, publictrust.ErrExpired) ||
		errors.Is(err, publictrust.ErrRevoked) ||
		errors.Is(err, publictrust.ErrAmbiguous)
}

func (server *Server) publicRoutes() {
	if server.config.Role != "public" {
		return
	}
	server.mux.Handle("POST /v1/public/publish", server.requirePublicCredential(server.config.PublicCollectorToken, http.HandlerFunc(server.publishPublic)))
	server.mux.HandleFunc("POST /v1/public/resolve", server.resolvePublic)
	server.mux.HandleFunc("GET /v1/public/artifacts/{identity}", server.getPublicArtifact)
	server.mux.Handle("POST /v1/public/revoke", server.requireToken(http.HandlerFunc(server.revokePublic)))
}

func (server *Server) requirePublicCredential(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		got := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
		if got == "" || token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			server.recordAuthorizationDenial(request)
			writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func (server *Server) publishPublic(writer http.ResponseWriter, request *http.Request) {
	defer request.Body.Close()
	if server.publicBuilds == nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "Public Builds are not configured"})
		return
	}
	duration, err := strconv.ParseInt(request.Header.Get("x-layercache-duration"), 10, 64)
	if err != nil || duration < 0 ||
		duration > int64((time.Duration(1<<63-1))/time.Millisecond) {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid producer duration"})
		return
	}
	key := artifact.Key{
		Integration:   request.Header.Get("x-layercache-integration"),
		Project:       request.Header.Get("x-layercache-project"),
		Compatibility: request.Header.Get("x-layercache-compatibility"),
		Native:        request.Header.Get("x-layercache-native-key"),
	}
	buildID := request.Header.Get("x-layercache-build-id")
	build, err := server.publicBuilds.Inspect(request.Context(), buildID)
	if err != nil {
		writeJSON(writer, http.StatusConflict, map[string]string{"error": "Public Cache publication has no active Public Build"})
		return
	}
	if !publicBuildPublicationHeadersMatch(server.config, build, key, request.Header) {
		writeJSON(writer, http.StatusConflict, map[string]string{"error": "Public Cache publication does not match Public Build"})
		return
	}
	lease := publicbuild.Lease{
		Token: request.Header.Get("x-layercache-lease-token"), WorkerID: request.Header.Get("x-layercache-worker-id"),
		Build: publicbuild.Build{ID: buildID},
	}
	permit, permitErr := server.publicBuilds.BeginLeasedPublication(request.Context(), lease)
	retry := errors.Is(permitErr, publicbuild.ErrPublicationLost) && build.State == publicbuild.StateSucceeded
	if permitErr != nil && !retry {
		writeJSON(writer, http.StatusConflict, map[string]string{"error": "Public Build cannot publish in its current state"})
		return
	}
	publicationCommitted := retry
	if !retry {
		defer func() {
			if !publicationCommitted {
				_, _ = server.publicBuilds.AbortPublication(
					context.WithoutCancel(request.Context()), permit, "trusted collection did not finish",
				)
			}
		}()
	}
	metadata := artifact.Metadata{DurationMS: duration, Values: map[string]string{"origin": "public"}}
	hash := sha256.New()
	maximum, limitErr := server.cacheLimit(request.Context())
	if limitErr != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "cache quota unavailable"})
		return
	}
	trackedBody := io.TeeReader(io.LimitReader(request.Body, maximum+1), hash)
	entry, created, err := server.store.Put(request.Context(), key, metadata, trackedBody)
	if errors.Is(err, artifact.ErrConflict) {
		if retry {
			// A completed build is immutable. A stale or corrupted retry must not
			// make its valid publication ambiguous or remove the winning bytes.
			writeJSON(writer, http.StatusConflict, map[string]string{"error": "Public Build retry does not match its completed publication"})
			return
		}
		incomingDigest := hex.EncodeToString(hash.Sum(nil))
		coordinate := publictrust.CacheCoordinate{
			Integration: key.Integration, Project: key.Project,
			Compatibility: key.Compatibility, NativeKey: key.Native,
		}
		markErr := server.publications.MarkAmbiguous(
			request.Context(), coordinate.Identity(), incomingDigest,
		)
		if markErr != nil && !definitivelyNoActivePublication(markErr) {
			writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "record Public Cache ambiguity"})
			return
		}
		if err := server.releasePublicArtifact(
			context.WithoutCancel(request.Context()), coordinate.Identity(), key,
		); err != nil {
			writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "clean ambiguous Public Cache artifact"})
			return
		}
		writeJSON(writer, http.StatusConflict, map[string]string{"error": "public identity already has different bytes"})
		return
	}
	if errors.Is(err, artifact.ErrQuota) {
		writeJSON(writer, http.StatusInsufficientStorage, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	now := time.Now().UTC()
	publication := publictrust.Publication{
		Integration:        key.Integration,
		Project:            key.Project,
		Compatibility:      key.Compatibility,
		NativeKey:          key.Native,
		Repository:         request.Header.Get("x-layercache-repository"),
		Commit:             request.Header.Get("x-layercache-commit"),
		RecipeDigest:       request.Header.Get("x-layercache-recipe"),
		Target:             request.Header.Get("x-layercache-target"),
		Platform:           request.Header.Get("x-layercache-platform"),
		Inputs:             publicTrustInputs(build.Request.Inputs),
		Toolchain:          request.Header.Get("x-layercache-toolchain"),
		Builder:            request.Header.Get("x-layercache-builder"),
		BuilderImageDigest: request.Header.Get("x-layercache-builder-image-digest"),
		Digest:             entry.Digest,
		Size:               entry.Size,
		DurationMS:         duration,
		BuildID:            buildID,
		IssuedAt:           now,
		ExpiresAt:          now.Add(24 * time.Hour),
	}
	if err := publictrust.ValidatePublication(publication); err != nil {
		if created {
			_ = server.store.Delete(context.WithoutCancel(request.Context()), key)
		}
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if retry && !completedPublicBuildMatchesRetry(build, publication) {
		if created {
			_ = server.store.Delete(context.WithoutCancel(request.Context()), key)
		}
		writeJSON(writer, http.StatusConflict, map[string]string{"error": "Public Build retry does not match its completed publication"})
		return
	}
	if err := server.pinPublicArtifact(request.Context(), publication); err != nil {
		// Pin commits can themselves have an ambiguous outcome. Preserve the
		// complete entry rather than risking deletion of bytes needed by an
		// already active publication.
		writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "retain Public Cache artifact"})
		return
	}
	completion := publicbuild.Publication{
		Outputs: []publicbuild.OutputDescriptor{{
			Name: publication.Identity(), Digest: "sha256:" + publication.Digest,
			SizeBytes: publication.Size, MediaType: publicBuildMediaType(publication.Integration),
		}},
		ProducerDuration: time.Duration(publication.DurationMS) * time.Millisecond,
	}
	if !retry {
		_, commitErr := server.publicBuilds.CommitPublication(request.Context(), permit, completion)
		if commitErr != nil {
			current, inspectErr := server.publicBuilds.Inspect(
				context.WithoutCancel(request.Context()), buildID,
			)
			if inspectErr == nil && completedPublicBuildMatchesRetry(current, publication) {
				// A commit response can be lost after the durable state changed.
				publicationCommitted = true
			} else {
				if inspectErr == nil {
					_ = server.releasePublicArtifact(
						context.WithoutCancel(request.Context()), publication.CacheIdentity(), key,
					)
				}
				writeJSON(writer, http.StatusConflict, map[string]string{"error": "Public Build completion failed"})
				return
			}
		} else {
			publicationCommitted = true
		}
	}
	publishErr := error(nil)
	if registry, ok := server.publications.(publicationActorRegistry); ok {
		publishErr = registry.PublishAs(request.Context(), "public-collector", publication)
	} else {
		publishErr = server.publications.Publish(request.Context(), publication)
	}
	if publishErr != nil {
		recoveryContext := context.WithoutCancel(request.Context())
		active, activeErr := server.publications.Resolve(recoveryContext, publication.CacheIdentity())
		if activeErr == nil && exactActivePublication(active, publication) {
			// The registry commit completed even though its response was lost.
			publishErr = nil
		} else {
			if errors.Is(activeErr, publictrust.ErrNotFound) ||
				errors.Is(activeErr, publictrust.ErrAmbiguous) ||
				errors.Is(activeErr, publictrust.ErrRevoked) ||
				errors.Is(activeErr, publictrust.ErrExpired) {
				_ = server.releasePublicArtifact(recoveryContext, publication.CacheIdentity(), key)
			}
			status := http.StatusInternalServerError
			if errors.Is(publishErr, publictrust.ErrAmbiguous) ||
				errors.Is(publishErr, publictrust.ErrRevoked) ||
				errors.Is(publishErr, publictrust.ErrIdentity) {
				status = http.StatusConflict
			}
			writeJSON(writer, status, map[string]string{"error": publishErr.Error()})
			return
		}
	}
	status := http.StatusCreated
	if retry || !created {
		status = http.StatusOK
	}
	writeJSON(writer, status, map[string]any{
		"identity":      publication.Identity(),
		"cacheIdentity": publication.CacheIdentity(),
		"digest":        "sha256:" + publication.Digest,
		"size":          publication.Size,
	})
}

func completedPublicBuildMatchesRetry(build publicbuild.Build, publication publictrust.Publication) bool {
	if build.State != publicbuild.StateSucceeded || build.Publication == nil || len(build.Publication.Outputs) != 1 {
		return false
	}
	output := build.Publication.Outputs[0]
	return output.Name == publication.Identity() &&
		output.Digest == "sha256:"+publication.Digest &&
		output.SizeBytes == publication.Size &&
		output.MediaType == publicBuildMediaType(publication.Integration) &&
		build.Publication.ProducerDuration.Milliseconds() == publication.DurationMS
}

func publicBuildPublicationHeadersMatch(cfg config.Config, build publicbuild.Build, key artifact.Key, header http.Header) bool {
	compatibility, err := publicbuild.CompatibilityIdentity(build.Request)
	if err != nil {
		return false
	}
	if !publicBuilderImageMatchesConfig(cfg, header.Get("x-layercache-builder-image-digest")) {
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
		if header.Get("x-layercache-toolchain") != actionsPublicToolchain ||
			header.Get("x-layercache-builder") != cfg.ActionsPublicBuilder ||
			build.Request.RecipeDigest != cfg.ActionsPublicRecipeDigest {
			return false
		}
		expectedNativeKey, err = publicbuild.ActionsPublicNativeKey(
			build.Request,
			actionsPublicToolchain,
			cfg.ActionsPublicBuilder,
		)
	}
	if err != nil || expectedNativeKey != "" && key.Native != expectedNativeKey {
		return false
	}
	return build.ID == header.Get("x-layercache-build-id") &&
		build.Request.Repository == header.Get("x-layercache-repository") &&
		build.Request.Commit == header.Get("x-layercache-commit") &&
		build.Request.RecipeDigest == header.Get("x-layercache-recipe") &&
		build.Request.Target == header.Get("x-layercache-target") &&
		string(build.Request.Platform) == header.Get("x-layercache-platform") &&
		string(build.Request.Integration) == key.Integration &&
		project == key.Project && compatibility == key.Compatibility
}

func publicBuilderImageMatchesConfig(cfg config.Config, digest string) bool {
	if publicbuild.ValidateBuilderImageDigest(digest) != nil {
		return false
	}
	configured := cfg.PublicBuildKernelSHA256 != "" || cfg.PublicBuildRootFSSHA256 != "" || cfg.PublicBuildContractSHA256 != ""
	if !configured {
		return true
	}
	expected, err := publicbuild.BuilderImageDigest(
		cfg.PublicBuildKernelSHA256,
		cfg.PublicBuildRootFSSHA256,
		cfg.PublicBuildContractSHA256,
	)
	return err == nil && digest == expected
}

func (server *Server) resolvePublic(writer http.ResponseWriter, request *http.Request) {
	defer request.Body.Close()
	var lookup publicResolveRequest
	if err := json.NewDecoder(io.LimitReader(request.Body, 64<<10)).Decode(&lookup); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid public lookup"})
		return
	}
	identity := lookup.Identity()
	publication, err := server.publications.Resolve(request.Context(), identity)
	if err != nil {
		if errors.Is(err, publictrust.ErrExpired) {
			if cleanupErr := server.releasePublicArtifact(
				context.WithoutCancel(request.Context()),
				publication.CacheIdentity(),
				publicArtifactKey(publication),
			); cleanupErr != nil {
				writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "expire Public Cache artifact"})
				return
			}
		}
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	available, err := server.verifiedPublicPublication(request.Context(), publication)
	if err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "verify Public Cache publication"})
		return
	}
	if !available {
		if err := retireInvalidPublicPublication(
			context.WithoutCancel(request.Context()), server.publications, server.store, publication,
		); err != nil {
			writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "retire invalid Public Cache publication"})
			return
		}
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	now := time.Now().UTC()
	publication.IssuedAt = now
	publication.ExpiresAt = now.Add(24 * time.Hour)
	envelope, err := publictrust.Sign(server.publicPrivate, publication)
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "sign Public Cache publication"})
		return
	}
	writeJSON(writer, http.StatusOK, publicResolveResponse{
		Envelope:    envelope,
		ArtifactURL: "/v1/public/artifacts/" + identity,
	})
}

func (server *Server) getPublicArtifact(writer http.ResponseWriter, request *http.Request) {
	identity := request.PathValue("identity")
	publication, err := server.publications.Resolve(request.Context(), identity)
	if err != nil {
		if errors.Is(err, publictrust.ErrExpired) {
			if cleanupErr := server.releasePublicArtifact(
				context.WithoutCancel(request.Context()),
				publication.CacheIdentity(),
				publicArtifactKey(publication),
			); cleanupErr != nil {
				writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "expire Public Cache artifact"})
				return
			}
		}
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	available, err := server.verifiedPublicPublication(request.Context(), publication)
	if err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "verify Public Cache publication"})
		return
	}
	if !available {
		if err := retireInvalidPublicPublication(
			context.WithoutCancel(request.Context()), server.publications, server.store, publication,
		); err != nil {
			writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "retire invalid Public Cache publication"})
			return
		}
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	key := publicArtifactKey(publication)
	entry, file, err := server.store.Get(request.Context(), key)
	if err != nil || entry.Digest != publication.Digest || entry.Size != publication.Size {
		if file != nil {
			_ = file.Close()
		}
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	defer file.Close()
	writer.Header().Set("Content-Type", "application/octet-stream")
	writer.Header().Set("Content-Length", strconv.FormatInt(entry.Size, 10))
	writer.Header().Set("x-layercache-digest", "sha256:"+entry.Digest)
	writer.WriteHeader(http.StatusOK)
	_, _ = io.Copy(writer, file)
}

func (server *Server) revokePublic(writer http.ResponseWriter, request *http.Request) {
	defer request.Body.Close()
	var revoke publicRevokeRequest
	if err := json.NewDecoder(io.LimitReader(request.Body, 64<<10)).Decode(&revoke); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid revocation"})
		return
	}
	identity := revoke.CacheCoordinate.Identity()
	actor := requestActor(request)
	var err error
	if registry, ok := server.publications.(publicationActorRegistry); ok {
		err = registry.RevokeAs(request.Context(), actor, identity, revoke.Reason, time.Now().UTC())
	} else {
		err = server.publications.Revoke(request.Context(), identity, revoke.Reason, time.Now().UTC())
	}
	if err != nil {
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	if err := server.releasePublicArtifact(context.WithoutCancel(request.Context()), identity, artifact.Key{
		Integration: revoke.Integration, Project: revoke.Project,
		Compatibility: revoke.Compatibility, Native: revoke.NativeKey,
	}); err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "remove revoked Public Cache artifact"})
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}
