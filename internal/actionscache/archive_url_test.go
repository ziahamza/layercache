package actionscache_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/actionscache"
)

func TestArchiveLocationIsShortLivedAndNeedsNoBearerHeader(t *testing.T) {
	t.Parallel()

	const (
		apiToken = "lookup-token"
		archive  = "stock-client-download"
	)
	scope := actionscache.Scope{
		Repository: "acme/widgets", Ref: "refs/heads/main", DefaultRef: "refs/heads/main", Compatibility: "linux-x64",
	}
	storage := actionscache.NewMemoryStorage()
	putArchive(t, storage, scope, "signed-key", "v1", []byte(archive))
	signer, err := actionscache.NewHMACArchiveURLSigner("independent-download-secret")
	if err != nil {
		t.Fatalf("new archive URL signer: %v", err)
	}
	handler, err := actionscache.NewHandler(actionscache.Config{
		Repository: scope.Repository, Ref: scope.Ref, DefaultRef: scope.DefaultRef, Compatibility: scope.Compatibility,
		ArchiveURLSigner: signer,
		ArchiveURLTTL:    500 * time.Millisecond,
	}, storage)
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		// This is the same hook the cloud server uses to leave only a valid,
		// signed archive GET outside bearer authentication.
		if !handler.AuthorizesArchiveDownload(request) && request.Header.Get("Authorization") != "Bearer "+apiToken {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(writer, request)
	}))
	t.Cleanup(server.Close)

	lookupRequest, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		server.URL+"/_apis/artifactcache/cache?keys=signed-key&version=v1", nil)
	if err != nil {
		t.Fatal(err)
	}
	lookupRequest.Header.Set("Authorization", "Bearer "+apiToken)
	response, err := http.DefaultClient.Do(lookupRequest)
	if err != nil {
		t.Fatal(err)
	}
	lookup := decodeLookup(t, response)
	if !strings.Contains(lookup.ArchiveLocation, "expires=") || !strings.Contains(lookup.ArchiveLocation, "signature=") {
		t.Fatalf("archiveLocation = %q, want expiry and signature", lookup.ArchiveLocation)
	}

	// @actions/cache v1 deliberately uses a fresh, unauthenticated client for
	// archiveLocation. A valid signed URL must therefore work without headers.
	response, err = http.Get(lookup.ArchiveLocation)
	if err != nil {
		t.Fatal(err)
	}
	contents, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusOK || string(contents) != archive {
		t.Fatalf("unsigned-client download status=%d body=%q err=%v", response.StatusCode, contents, readErr)
	}

	tampered, err := url.Parse(lookup.ArchiveLocation)
	if err != nil {
		t.Fatal(err)
	}
	query := tampered.Query()
	query.Set("expires", "4102444800")
	tampered.RawQuery = query.Encode()
	response, err = http.Get(tampered.String())
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode == http.StatusOK {
		t.Fatal("tampered archive URL was accepted")
	}

	time.Sleep(600 * time.Millisecond)
	response, err = http.Get(lookup.ArchiveLocation)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode == http.StatusOK {
		t.Fatal("expired archive URL was accepted")
	}
}
