package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/layercache/layercache/internal/artifact"
	"github.com/layercache/layercache/internal/publictrust"
)

type publicResolveRequest struct {
	Integration   string `json:"integration"`
	Project       string `json:"project"`
	Compatibility string `json:"compatibility"`
	NativeKey     string `json:"nativeKey"`
}

type publicResolveResponse struct {
	Envelope    publictrust.Envelope `json:"envelope"`
	ArtifactURL string               `json:"artifactUrl"`
}

type publicRevokeRequest struct {
	publicResolveRequest
	Reason string `json:"reason"`
}

func (server *Server) publicRoutes() {
	if server.config.Role != "public" {
		return
	}
	server.mux.Handle("POST /v1/public/publish", server.requirePublisher(http.HandlerFunc(server.publishPublic)))
	server.mux.HandleFunc("POST /v1/public/resolve", server.resolvePublic)
	server.mux.HandleFunc("GET /v1/public/artifacts/{identity}", server.getPublicArtifact)
	server.mux.Handle("POST /v1/public/revoke", server.requirePublisher(http.HandlerFunc(server.revokePublic)))
}

func (server *Server) requirePublisher(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		got := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
		if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(server.config.PublisherToken)) != 1 {
			writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func (server *Server) publishPublic(writer http.ResponseWriter, request *http.Request) {
	defer request.Body.Close()
	duration, err := strconv.ParseInt(request.Header.Get("x-layercache-duration"), 10, 64)
	if err != nil || duration < 0 {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid producer duration"})
		return
	}
	key := artifact.Key{
		Integration:   request.Header.Get("x-layercache-integration"),
		Project:       request.Header.Get("x-layercache-project"),
		Compatibility: request.Header.Get("x-layercache-compatibility"),
		Native:        request.Header.Get("x-layercache-native-key"),
	}
	metadata := artifact.Metadata{DurationMS: duration, Values: map[string]string{"origin": "public"}}
	entry, created, err := server.store.Put(request.Context(), key, metadata, io.LimitReader(request.Body, server.config.MaxBytes+1))
	if errors.Is(err, artifact.ErrConflict) {
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
		Integration:   key.Integration,
		Project:       key.Project,
		Compatibility: key.Compatibility,
		NativeKey:     key.Native,
		Repository:    request.Header.Get("x-layercache-repository"),
		Commit:        request.Header.Get("x-layercache-commit"),
		RecipeDigest:  request.Header.Get("x-layercache-recipe"),
		Platform:      request.Header.Get("x-layercache-platform"),
		Toolchain:     request.Header.Get("x-layercache-toolchain"),
		Builder:       request.Header.Get("x-layercache-builder"),
		Digest:        entry.Digest,
		Size:          entry.Size,
		DurationMS:    duration,
		BuildID:       request.Header.Get("x-layercache-build-id"),
		IssuedAt:      now,
		ExpiresAt:     now.Add(90 * 24 * time.Hour),
	}
	if err := server.publications.Publish(request.Context(), publication); err != nil {
		if created {
			_ = server.store.Delete(request.Context(), key)
		}
		status := http.StatusBadRequest
		if errors.Is(err, publictrust.ErrAmbiguous) || errors.Is(err, publictrust.ErrRevoked) {
			status = http.StatusConflict
		}
		writeJSON(writer, status, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{
		"identity": publication.Identity(),
		"digest":   "sha256:" + publication.Digest,
		"size":     publication.Size,
	})
}

func (server *Server) resolvePublic(writer http.ResponseWriter, request *http.Request) {
	defer request.Body.Close()
	var lookup publicResolveRequest
	if err := json.NewDecoder(io.LimitReader(request.Body, 64<<10)).Decode(&lookup); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid public lookup"})
		return
	}
	identity := publictrust.Publication{
		Integration: lookup.Integration, Project: lookup.Project,
		Compatibility: lookup.Compatibility, NativeKey: lookup.NativeKey,
	}.Identity()
	publication, err := server.publications.Resolve(request.Context(), identity)
	if err != nil {
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
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	key := artifact.Key{
		Integration: publication.Integration, Project: publication.Project,
		Compatibility: publication.Compatibility, Native: publication.NativeKey,
	}
	entry, file, err := server.store.Get(request.Context(), key)
	if err != nil || entry.Digest != publication.Digest {
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
	identity := publictrust.Publication{
		Integration: revoke.Integration, Project: revoke.Project,
		Compatibility: revoke.Compatibility, NativeKey: revoke.NativeKey,
	}.Identity()
	if err := server.publications.Revoke(request.Context(), identity, revoke.Reason, time.Now().UTC()); err != nil {
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}
