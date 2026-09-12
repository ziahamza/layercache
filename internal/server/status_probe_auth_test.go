package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/access"
	"github.com/layercache/layercache/internal/config"
)

func TestPublicBuildStatusProbeDistinguishesInvalidCredential(t *testing.T) {
	t.Parallel()

	const project = "github.com/acme/widgets"
	instance := &Server{config: config.Config{Role: "public", ProjectID: project, LocalToken: "signing-secret"}}
	handlerCalls := 0
	handler := instance.requireToken(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		handlerCalls++
		writer.WriteHeader(http.StatusNoContent)
	}))

	probe := httptest.NewRequest(http.MethodGet, publicBuildStatusProbePath, nil)
	probe.Header.Set("Authorization", "Bearer invalid-token")
	probeResponse := httptest.NewRecorder()
	handler.ServeHTTP(probeResponse, probe)
	if probeResponse.Code != http.StatusUnauthorized {
		t.Fatalf("reserved probe status = %d, want %d", probeResponse.Code, http.StatusUnauthorized)
	}
	if handlerCalls != 0 {
		t.Fatal("invalid credential reached Public Build probe handler")
	}

	now := time.Now().UTC()
	readerToken, err := access.MintCapabilityToken(instance.config.LocalToken, access.Claims{
		Subject: "reader", Project: project, Capabilities: []access.Capability{access.CapabilityRead},
		ExpiresAt: now.Add(time.Hour),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	reader := httptest.NewRequest(http.MethodGet, publicBuildStatusProbePath, nil)
	reader.Header.Set("Authorization", "Bearer "+readerToken)
	readerResponse := httptest.NewRecorder()
	handler.ServeHTTP(readerResponse, reader)
	if readerResponse.Code != http.StatusUnauthorized || handlerCalls != 0 {
		t.Fatalf("read-only probe status = %d, handler calls = %d", readerResponse.Code, handlerCalls)
	}

	writerToken, err := access.MintCapabilityToken(instance.config.LocalToken, access.Claims{
		Subject: "writer", Project: project, Capabilities: []access.Capability{access.CapabilityWrite},
		ExpiresAt: now.Add(time.Hour),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	writerRequest := httptest.NewRequest(http.MethodGet, publicBuildStatusProbePath, nil)
	writerRequest.Header.Set("Authorization", "Bearer "+writerToken)
	writerResponse := httptest.NewRecorder()
	handler.ServeHTTP(writerResponse, writerRequest)
	if writerResponse.Code != http.StatusNoContent || handlerCalls != 1 {
		t.Fatalf("write-capable probe status = %d, handler calls = %d", writerResponse.Code, handlerCalls)
	}

	ordinary := httptest.NewRequest(http.MethodGet, "/v1/public-builds/public-build-123", nil)
	ordinary.Header.Set("Authorization", "Bearer invalid-token")
	ordinaryResponse := httptest.NewRecorder()
	handler.ServeHTTP(ordinaryResponse, ordinary)
	if ordinaryResponse.Code != http.StatusNotFound {
		t.Fatalf("ordinary unauthorized lookup status = %d, want hidden %d", ordinaryResponse.Code, http.StatusNotFound)
	}
}
