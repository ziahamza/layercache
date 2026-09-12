package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCLIHTTPClientHasBoundedConnectionAndResponseTimes(t *testing.T) {
	t.Parallel()

	client := newCLIHTTPClient(controlRequestTimeout)
	if client.Timeout != controlRequestTimeout {
		t.Fatalf("client timeout = %s, want %s", client.Timeout, controlRequestTimeout)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	if transport.DialContext == nil || transport.ResponseHeaderTimeout <= 0 || transport.TLSHandshakeTimeout <= 0 {
		t.Fatalf("CLI transport has unbounded phase: %+v", transport)
	}
}

func TestCLIHTTPClientDoesNotReplayCredentialBodiesAcrossRedirects(t *testing.T) {
	replayed := make(chan string, 1)
	sink := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		replayed <- string(body)
	}))
	t.Cleanup(sink.Close)
	source := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Location", sink.URL)
		writer.WriteHeader(http.StatusTemporaryRedirect)
	}))
	t.Cleanup(source.Close)

	response, err := newCLIHTTPClient(time.Second).Post(
		source.URL, "application/json", strings.NewReader(`{"githubToken":"must-not-leak"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("redirect status = %d", response.StatusCode)
	}
	select {
	case body := <-replayed:
		t.Fatalf("credential body was replayed to redirect target: %s", body)
	case <-time.After(100 * time.Millisecond):
	}
}
