package telemetry

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestOTLPExportUsesStaticRouteAndExcludesRequestSecrets(t *testing.T) {
	var mu sync.Mutex
	var exported bytes.Buffer
	collector := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read OTLP body: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		_, _ = exported.Write(body)
		mu.Unlock()
		writer.Header().Set("Content-Type", "application/x-protobuf")
		writer.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()

	t.Setenv("OTEL_SDK_DISABLED", "false")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", collector.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "")
	t.Setenv("OTEL_TRACES_EXPORTER", "otlp")
	t.Setenv("OTEL_METRICS_EXPORTER", "otlp")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_HEADERS", "")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_HEADERS", "")

	runtime, err := NewFromEnvironment(context.Background(), Options{ServiceName: "layercache-test", Role: "team"})
	if err != nil {
		t.Fatalf("NewFromEnvironment: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/items/{id}", func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	})
	const targetSecret = "cache-key-that-must-not-be-exported"
	const headerSecret = "bearer-that-must-not-be-exported"
	request := httptest.NewRequest(http.MethodGet, "/v1/items/"+targetSecret+"?token="+targetSecret, nil)
	request.Header.Set("Authorization", "Bearer "+headerSecret)
	response := httptest.NewRecorder()
	runtime.Handler(mux).ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("response status = %d", response.Code)
	}
	if err := runtime.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	mu.Lock()
	document := append([]byte(nil), exported.Bytes()...)
	mu.Unlock()
	if len(document) == 0 {
		t.Fatal("collector received no trace or metric export")
	}
	if bytes.Contains(document, []byte(targetSecret)) || bytes.Contains(document, []byte(headerSecret)) {
		t.Fatal("OTLP export contained a request target, query, or authorization value")
	}
	if !bytes.Contains(document, []byte("GET /v1/items/{id}")) {
		t.Fatal("OTLP export did not contain the static route pattern")
	}
}
