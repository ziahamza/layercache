package actionscache_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/actionscache"
)

func TestLookupMissReturnsNoContent(t *testing.T) {
	t.Parallel()

	handler, err := actionscache.NewHandler(actionscache.Config{
		Repository:    "acme/widgets",
		Ref:           "refs/heads/feature",
		DefaultRef:    "refs/heads/main",
		Compatibility: "linux-x64-node24",
	}, actionscache.NewMemoryStorage())
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	response, err := http.Get(server.URL + "/_apis/artifactcache/cache?keys=pnpm-lock-abc,pnpm-&version=v1")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusNoContent)
	}
}

func TestV1UploadCanBeLookedUpAndDownloaded(t *testing.T) {
	t.Parallel()

	handler, err := actionscache.NewHandler(actionscache.Config{
		Repository:    "acme/widgets",
		Ref:           "refs/heads/feature",
		DefaultRef:    "refs/heads/main",
		Compatibility: "linux-x64-node24",
	}, actionscache.NewMemoryStorage())
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	archive := []byte("opaque-actions-cache-archive")
	reserveBody := fmt.Sprintf(`{"key":"pnpm-lock-abc","version":"v1","cacheSize":%d}`, len(archive))
	response, err := http.Post(
		server.URL+"/_apis/artifactcache/caches",
		"application/json",
		strings.NewReader(reserveBody),
	)
	if err != nil {
		t.Fatal(err)
	}
	var reservation struct {
		CacheID int64 `json:"cacheId"`
	}
	if err := json.NewDecoder(response.Body).Decode(&reservation); err != nil {
		response.Body.Close()
		t.Fatalf("decode reservation: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("reserve status = %d, want %d", response.StatusCode, http.StatusCreated)
	}
	if reservation.CacheID <= 0 {
		t.Fatalf("cacheId = %d, want a positive id", reservation.CacheID)
	}

	split := 8
	uploadChunk(t, server.URL, reservation.CacheID, int64(split), archive[split:])
	uploadChunk(t, server.URL, reservation.CacheID, 0, archive[:split])

	commitBody := fmt.Sprintf(`{"size":%d}`, len(archive))
	response, err = http.Post(
		fmt.Sprintf("%s/_apis/artifactcache/caches/%d", server.URL, reservation.CacheID),
		"application/json",
		strings.NewReader(commitBody),
	)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("commit status = %d, want %d", response.StatusCode, http.StatusNoContent)
	}

	response, err = http.Get(server.URL + "/_apis/artifactcache/cache?keys=pnpm-lock-abc,pnpm-&version=v1")
	if err != nil {
		t.Fatal(err)
	}
	var lookup struct {
		CacheKey        string    `json:"cacheKey"`
		Scope           string    `json:"scope"`
		CacheVersion    string    `json:"cacheVersion"`
		CreationTime    time.Time `json:"creationTime"`
		ArchiveLocation string    `json:"archiveLocation"`
	}
	if err := json.NewDecoder(response.Body).Decode(&lookup); err != nil {
		response.Body.Close()
		t.Fatalf("decode lookup: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("lookup status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	if lookup.CacheKey != "pnpm-lock-abc" {
		t.Fatalf("cacheKey = %q, want exact primary key", lookup.CacheKey)
	}
	if lookup.Scope != "refs/heads/feature" {
		t.Fatalf("scope = %q, want current ref", lookup.Scope)
	}
	if lookup.CacheVersion != "v1" {
		t.Fatalf("cacheVersion = %q, want v1", lookup.CacheVersion)
	}
	if lookup.CreationTime.IsZero() {
		t.Fatal("creationTime is empty")
	}
	if response.Header.Get("X-LayerCache-Match") != "exact" {
		t.Fatalf("match = %q, want exact", response.Header.Get("X-LayerCache-Match"))
	}
	if response.Header.Get("X-LayerCache-Ref-Scope") != "current" {
		t.Fatalf("ref scope = %q, want current", response.Header.Get("X-LayerCache-Ref-Scope"))
	}
	if response.Header.Get("X-LayerCache-Source") != "localCache" {
		t.Fatalf("source = %q, want localCache", response.Header.Get("X-LayerCache-Source"))
	}

	response, err = http.Get(lookup.ArchiveLocation)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("download status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	if response.Header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("download Content-Type = %q, want application/octet-stream", response.Header.Get("Content-Type"))
	}
	if response.ContentLength != int64(len(archive)) {
		t.Fatalf("download Content-Length = %d, want %d", response.ContentLength, len(archive))
	}
	if !bytes.Equal(got, archive) {
		t.Fatalf("download = %q, want %q", got, archive)
	}
}

func TestLookupPrefersExactThenOrderedPrefixesAndNewestPartial(t *testing.T) {
	t.Parallel()

	storage := actionscache.NewMemoryStorage()
	server := newCacheServer(t, storage, actionscache.Config{
		Repository:    "acme/widgets",
		Ref:           "refs/heads/feature",
		DefaultRef:    "refs/heads/main",
		Compatibility: "linux-x64-node24",
	})

	saveCache(t, server.URL, "pnpm-first-old", "v1", []byte("old"))
	saveCache(t, server.URL, "pnpm-first-new", "v1", []byte("new"))
	saveCache(t, server.URL, "pnpm-second-newest", "v1", []byte("newest overall"))
	saveCache(t, server.URL, "pnpm-exact", "v1", []byte("exact"))
	saveCache(t, server.URL, "pnpm-exact-newer-prefix", "v1", []byte("newer prefix"))

	response, err := http.Get(server.URL + "/_apis/artifactcache/cache?keys=pnpm-exact,pnpm-first-,pnpm-second-&version=v1")
	if err != nil {
		t.Fatal(err)
	}
	exact := decodeLookup(t, response)
	if exact.CacheKey != "pnpm-exact" {
		t.Fatalf("exact cacheKey = %q, want pnpm-exact", exact.CacheKey)
	}
	if response.Header.Get("X-LayerCache-Match") != "exact" {
		t.Fatalf("exact match = %q, want exact", response.Header.Get("X-LayerCache-Match"))
	}

	response, err = http.Get(server.URL + "/_apis/artifactcache/cache?keys=missing,pnpm-first-,pnpm-second-&version=v1")
	if err != nil {
		t.Fatal(err)
	}
	partial := decodeLookup(t, response)
	if partial.CacheKey != "pnpm-first-new" {
		t.Fatalf("partial cacheKey = %q, want newest result for first ordered prefix", partial.CacheKey)
	}
	if response.Header.Get("X-LayerCache-Match") != "prefix" {
		t.Fatalf("partial match = %q, want prefix", response.Header.Get("X-LayerCache-Match"))
	}
	if response.Header.Get("X-LayerCache-Requested-Key") != "pnpm-first-" {
		t.Fatalf("requested key = %q, want pnpm-first-", response.Header.Get("X-LayerCache-Requested-Key"))
	}
}

func TestLookupUsesAuthenticatedScopeAndFallsBackToDefaultRef(t *testing.T) {
	t.Parallel()

	storage := actionscache.NewMemoryStorage()
	mainServer := newCacheServer(t, storage, actionscache.Config{
		Repository:    "acme/widgets",
		Ref:           "refs/heads/main",
		DefaultRef:    "refs/heads/main",
		Compatibility: "linux-x64-node24",
	})
	saveCache(t, mainServer.URL, "pnpm-main", "v1", []byte("main archive"))

	featureServer := newCacheServer(t, storage, actionscache.Config{
		Repository:    "acme/widgets",
		Ref:           "refs/heads/feature",
		DefaultRef:    "refs/heads/main",
		Compatibility: "linux-x64-node24",
	})
	response, err := http.Get(featureServer.URL + "/_apis/artifactcache/cache?keys=pnpm-main&version=v1&repository=other/repo&ref=refs/heads/other&default-ref=refs/heads/other")
	if err != nil {
		t.Fatal(err)
	}
	lookup := decodeLookup(t, response)
	if lookup.Scope != "refs/heads/main" {
		t.Fatalf("scope = %q, want default ref", lookup.Scope)
	}
	if response.Header.Get("X-LayerCache-Ref-Scope") != "default" {
		t.Fatalf("ref scope = %q, want default", response.Header.Get("X-LayerCache-Ref-Scope"))
	}
	response, err = http.Get(lookup.ArchiveLocation)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || string(archive) != "main archive" {
		t.Fatalf("default-ref archive status = %d body = %q", response.StatusCode, archive)
	}

	for name, config := range map[string]actionscache.Config{
		"repository": {
			Repository:    "other/repository",
			Ref:           "refs/heads/feature",
			DefaultRef:    "refs/heads/main",
			Compatibility: "linux-x64-node24",
		},
		"compatibility": {
			Repository:    "acme/widgets",
			Ref:           "refs/heads/feature",
			DefaultRef:    "refs/heads/main",
			Compatibility: "macos-arm64-node24",
		},
		"default ref": {
			Repository:    "acme/widgets",
			Ref:           "refs/heads/feature",
			DefaultRef:    "refs/heads/trunk",
			Compatibility: "linux-x64-node24",
		},
	} {
		t.Run(name, func(t *testing.T) {
			server := newCacheServer(t, storage, config)
			response, err := http.Get(server.URL + "/_apis/artifactcache/cache?keys=pnpm-main&version=v1")
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != http.StatusNoContent {
				t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusNoContent)
			}
		})
	}

	response, err = http.Get(featureServer.URL + "/_apis/artifactcache/cache?keys=pnpm-main&version=v2")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("version mismatch status = %d, want %d", response.StatusCode, http.StatusNoContent)
	}
}

func TestReserveIsFirstWriterWins(t *testing.T) {
	t.Parallel()

	server := newCacheServer(t, actionscache.NewMemoryStorage(), actionscache.Config{
		Repository:    "acme/widgets",
		Ref:           "refs/heads/main",
		DefaultRef:    "refs/heads/main",
		Compatibility: "linux-x64-node24",
	})
	const contenders = 12
	start := make(chan struct{})
	type result struct {
		status  int
		cacheID int64
		err     error
	}
	results := make(chan result, contenders)
	for range contenders {
		go func() {
			<-start
			response, err := http.Post(
				server.URL+"/_apis/artifactcache/caches",
				"application/json",
				strings.NewReader(`{"key":"shared-key","version":"v1","cacheSize":6}`),
			)
			if err != nil {
				results <- result{err: err}
				return
			}
			defer response.Body.Close()
			var body struct {
				CacheID int64 `json:"cacheId"`
			}
			if response.StatusCode == http.StatusCreated {
				err = json.NewDecoder(response.Body).Decode(&body)
			}
			results <- result{status: response.StatusCode, cacheID: body.CacheID, err: err}
		}()
	}
	close(start)

	winners := 0
	conflicts := 0
	var winnerID int64
	for range contenders {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		switch result.status {
		case http.StatusCreated:
			winners++
			winnerID = result.cacheID
		case http.StatusConflict:
			conflicts++
		default:
			t.Fatalf("reserve status = %d, want 201 or 409", result.status)
		}
	}
	if winners != 1 || conflicts != contenders-1 {
		t.Fatalf("winners = %d and conflicts = %d, want 1 and %d", winners, conflicts, contenders-1)
	}

	uploadChunk(t, server.URL, winnerID, 0, []byte("winner"))
	response, err := http.Post(
		fmt.Sprintf("%s/_apis/artifactcache/caches/%d", server.URL, winnerID),
		"application/json",
		strings.NewReader(`{"size":6}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("commit status = %d, want %d", response.StatusCode, http.StatusNoContent)
	}

	response, err = http.Post(
		server.URL+"/_apis/artifactcache/caches",
		"application/json",
		strings.NewReader(`{"key":"shared-key","version":"v1","cacheSize":6}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("post-commit reserve status = %d, want %d", response.StatusCode, http.StatusConflict)
	}
}

func TestV1KeyValidationRejectsInvalidLookupsAndReservations(t *testing.T) {
	t.Parallel()

	server := newCacheServer(t, actionscache.NewMemoryStorage(), actionscache.Config{
		Repository:    "acme/widgets",
		Ref:           "refs/heads/main",
		DefaultRef:    "refs/heads/main",
		Compatibility: "linux-x64-node24",
	})

	lookupCases := map[string]string{
		"missing keys":    "version=v1",
		"missing version": "keys=valid",
		"empty key":       "keys=valid,,restore&version=v1",
		"too long":        "keys=" + strings.Repeat("x", 513) + "&version=v1",
		"too many":        "keys=" + strings.Join([]string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11"}, ",") + "&version=v1",
	}
	for name, query := range lookupCases {
		t.Run("lookup "+name, func(t *testing.T) {
			response, err := http.Get(server.URL + "/_apis/artifactcache/cache?" + query)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusBadRequest)
			}
		})
	}

	reserveCases := map[string]string{
		"empty key":       `{"key":"","version":"v1","cacheSize":1}`,
		"comma":           `{"key":"invalid,key","version":"v1","cacheSize":1}`,
		"too long":        fmt.Sprintf(`{"key":%q,"version":"v1","cacheSize":1}`, strings.Repeat("x", 513)),
		"missing version": `{"key":"valid","cacheSize":1}`,
		"negative size":   `{"key":"valid","version":"v1","cacheSize":-1}`,
	}
	for name, body := range reserveCases {
		t.Run("reserve "+name, func(t *testing.T) {
			response, err := http.Post(
				server.URL+"/_apis/artifactcache/caches",
				"application/json",
				strings.NewReader(body),
			)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusBadRequest)
			}
		})
	}
}

func TestV1HandlerRejectsOversizeReservationBeforeClaimingTheKey(t *testing.T) {
	t.Parallel()

	server := newCacheServer(t, actionscache.NewMemoryStorage(), actionscache.Config{
		Repository:       "acme/widgets",
		Ref:              "refs/heads/main",
		DefaultRef:       "refs/heads/main",
		Compatibility:    "linux-x64-node24",
		MaxArtifactBytes: 5,
		MaxChunkBytes:    3,
	})

	response, err := http.Post(
		server.URL+"/_apis/artifactcache/caches",
		"application/json",
		strings.NewReader(`{"key":"bounded","version":"v1","cacheSize":6}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversize reserve status = %d, want %d", response.StatusCode, http.StatusBadRequest)
	}

	response, err = http.Post(
		server.URL+"/_apis/artifactcache/caches",
		"application/json",
		strings.NewReader(`{"key":"bounded","version":"v1","cacheSize":5}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("valid reserve after rejection status = %d, want %d", response.StatusCode, http.StatusCreated)
	}
}

func TestV1HandlerBoundsUploadsWhenReserveOmitsCacheSize(t *testing.T) {
	t.Parallel()

	server := newCacheServer(t, actionscache.NewMemoryStorage(), actionscache.Config{
		Repository:       "acme/widgets",
		Ref:              "refs/heads/main",
		DefaultRef:       "refs/heads/main",
		Compatibility:    "linux-x64-node24",
		MaxArtifactBytes: 6,
		MaxChunkBytes:    3,
	})
	response, err := http.Post(
		server.URL+"/_apis/artifactcache/caches",
		"application/json",
		strings.NewReader(`{"key":"unknown-size","version":"v1"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	var reservation struct {
		CacheID int64 `json:"cacheId"`
	}
	if err := json.NewDecoder(response.Body).Decode(&reservation); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("reserve without cacheSize status = %d, want %d", response.StatusCode, http.StatusCreated)
	}

	for _, test := range []struct {
		name  string
		start int64
		body  string
	}{
		{name: "chunk limit", start: 0, body: "four"},
		{name: "artifact limit", start: 4, body: "end"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, requestErr := http.NewRequest(
				http.MethodPatch,
				fmt.Sprintf("%s/_apis/artifactcache/caches/%d", server.URL, reservation.CacheID),
				strings.NewReader(test.body),
			)
			if requestErr != nil {
				t.Fatal(requestErr)
			}
			request.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/*", test.start, test.start+int64(len(test.body))-1))
			result, requestErr := http.DefaultClient.Do(request)
			if requestErr != nil {
				t.Fatal(requestErr)
			}
			result.Body.Close()
			if result.StatusCode != http.StatusBadRequest {
				t.Fatalf("upload status = %d, want %d", result.StatusCode, http.StatusBadRequest)
			}
		})
	}

	response, err = http.Post(
		fmt.Sprintf("%s/_apis/artifactcache/caches/%d", server.URL, reservation.CacheID),
		"application/json",
		strings.NewReader(`{"size":7}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversize commit status = %d, want %d", response.StatusCode, http.StatusBadRequest)
	}

	response, err = http.Get(server.URL + "/_apis/artifactcache/cache?keys=unknown-size&version=v1")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("lookup before valid commit status = %d, want %d", response.StatusCode, http.StatusNoContent)
	}

	uploadChunk(t, server.URL, reservation.CacheID, 0, []byte("abc"))
	uploadChunk(t, server.URL, reservation.CacheID, 3, []byte("def"))
	response, err = http.Post(
		fmt.Sprintf("%s/_apis/artifactcache/caches/%d", server.URL, reservation.CacheID),
		"application/json",
		strings.NewReader(`{"size":6}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("valid commit status = %d, want %d", response.StatusCode, http.StatusNoContent)
	}
}

func TestRangedUploadRejectsMalformedOrIncompleteArchives(t *testing.T) {
	t.Parallel()

	server := newCacheServer(t, actionscache.NewMemoryStorage(), actionscache.Config{
		Repository:    "acme/widgets",
		Ref:           "refs/heads/main",
		DefaultRef:    "refs/heads/main",
		Compatibility: "linux-x64-node24",
	})
	response, err := http.Post(
		server.URL+"/_apis/artifactcache/caches",
		"application/json",
		strings.NewReader(`{"key":"range-key","version":"v1","cacheSize":6}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	var reservation struct {
		CacheID int64 `json:"cacheId"`
	}
	if err := json.NewDecoder(response.Body).Decode(&reservation); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()

	request, err := http.NewRequest(
		http.MethodPatch,
		fmt.Sprintf("%s/_apis/artifactcache/caches/%d", server.URL, reservation.CacheID),
		strings.NewReader("abc"),
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing Content-Range status = %d, want %d", response.StatusCode, http.StatusBadRequest)
	}

	request, err = http.NewRequest(
		http.MethodPatch,
		fmt.Sprintf("%s/_apis/artifactcache/caches/%d", server.URL, reservation.CacheID),
		strings.NewReader("too long"),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Range", "bytes 0-2/*")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("range length status = %d, want %d", response.StatusCode, http.StatusBadRequest)
	}

	uploadChunk(t, server.URL, reservation.CacheID, 3, []byte("ner"))
	commitURL := fmt.Sprintf("%s/_apis/artifactcache/caches/%d", server.URL, reservation.CacheID)
	response, err = http.Post(commitURL, "application/json", strings.NewReader(`{"size":6}`))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("incomplete commit status = %d, want %d", response.StatusCode, http.StatusBadRequest)
	}

	uploadChunk(t, server.URL, reservation.CacheID, 0, []byte("win"))
	uploadChunk(t, server.URL, reservation.CacheID, 0, []byte("win"))
	response, err = http.Post(commitURL, "application/json", strings.NewReader(`{"size":6}`))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("recovered commit status = %d, want %d", response.StatusCode, http.StatusNoContent)
	}
}

func TestArchiveDownloadCannotCrossAuthenticatedScope(t *testing.T) {
	t.Parallel()

	storage := actionscache.NewMemoryStorage()
	owner := newCacheServer(t, storage, actionscache.Config{
		Repository:    "acme/widgets",
		Ref:           "refs/heads/main",
		DefaultRef:    "refs/heads/main",
		Compatibility: "linux-x64-node24",
	})
	cacheID := saveCache(t, owner.URL, "private-key", "v1", []byte("private archive"))

	otherRepository := newCacheServer(t, storage, actionscache.Config{
		Repository:    "other/repository",
		Ref:           "refs/heads/main",
		DefaultRef:    "refs/heads/main",
		Compatibility: "linux-x64-node24",
	})
	response, err := http.Get(fmt.Sprintf(
		"%s/_apis/artifactcache/caches/%d/archive",
		otherRepository.URL,
		cacheID,
	))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-repository download status = %d, want %d", response.StatusCode, http.StatusNotFound)
	}
}

func TestOptionalCompatibilityPathOverridesLocalHostIdentity(t *testing.T) {
	t.Parallel()

	server := newCacheServer(t, actionscache.NewMemoryStorage(), actionscache.Config{
		Repository: "acme/widgets", Ref: "refs/heads/main", DefaultRef: "refs/heads/main",
		Compatibility: "darwin-arm64-schema1",
	})
	linuxBase := server.URL + "/_layercache/compatibility/linux-amd64-schema1"
	saveCache(t, linuxBase, "portable-key", "v1", []byte("linux archive"))

	response, err := http.Get(server.URL + "/_apis/artifactcache/cache?keys=portable-key&version=v1")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("host fallback status = %d, want compatibility miss", response.StatusCode)
	}

	response, err = http.Get(linuxBase + "/_apis/artifactcache/cache?keys=portable-key&version=v1")
	if err != nil {
		t.Fatal(err)
	}
	lookup := decodeLookup(t, response)
	response.Body.Close()
	if !strings.Contains(lookup.ArchiveLocation, "/_layercache/compatibility/linux-amd64-schema1/") {
		t.Fatalf("archive location = %q, want signed compatibility path", lookup.ArchiveLocation)
	}
	download, err := http.Get(lookup.ArchiveLocation)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(download.Body)
	download.Body.Close()
	if readErr != nil || download.StatusCode != http.StatusOK || string(body) != "linux archive" {
		t.Fatalf("signed download status = %d body = %q err = %v", download.StatusCode, body, readErr)
	}
}

type lookupResponse struct {
	CacheKey        string    `json:"cacheKey"`
	Scope           string    `json:"scope"`
	CacheVersion    string    `json:"cacheVersion"`
	CreationTime    time.Time `json:"creationTime"`
	ArchiveLocation string    `json:"archiveLocation"`
}

func newCacheServer(t *testing.T, storage actionscache.StorageIndex, config actionscache.Config) *httptest.Server {
	t.Helper()
	handler, err := actionscache.NewHandler(config, storage)
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func saveCache(t *testing.T, baseURL, key, version string, archive []byte) int64 {
	t.Helper()
	reserveBody, err := json.Marshal(map[string]any{
		"key":       key,
		"version":   version,
		"cacheSize": len(archive),
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.Post(
		baseURL+"/_apis/artifactcache/caches",
		"application/json",
		bytes.NewReader(reserveBody),
	)
	if err != nil {
		t.Fatal(err)
	}
	var reservation struct {
		CacheID int64 `json:"cacheId"`
	}
	if err := json.NewDecoder(response.Body).Decode(&reservation); err != nil {
		response.Body.Close()
		t.Fatalf("decode reservation: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("reserve %q status = %d, want %d", key, response.StatusCode, http.StatusCreated)
	}

	uploadChunk(t, baseURL, reservation.CacheID, 0, archive)
	commitBody := fmt.Sprintf(`{"size":%d}`, len(archive))
	response, err = http.Post(
		fmt.Sprintf("%s/_apis/artifactcache/caches/%d", baseURL, reservation.CacheID),
		"application/json",
		strings.NewReader(commitBody),
	)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("commit %q status = %d, want %d", key, response.StatusCode, http.StatusNoContent)
	}
	return reservation.CacheID
}

func decodeLookup(t *testing.T, response *http.Response) lookupResponse {
	t.Helper()
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("lookup status = %d, want %d: %s", response.StatusCode, http.StatusOK, body)
	}
	var lookup lookupResponse
	if err := json.NewDecoder(response.Body).Decode(&lookup); err != nil {
		t.Fatalf("decode lookup: %v", err)
	}
	return lookup
}

func uploadChunk(t *testing.T, baseURL string, cacheID, start int64, chunk []byte) {
	t.Helper()

	request, err := http.NewRequest(
		http.MethodPatch,
		fmt.Sprintf("%s/_apis/artifactcache/caches/%d", baseURL, cacheID),
		bytes.NewReader(chunk),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/*", start, start+int64(len(chunk))-1))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("upload status = %d, want %d", response.StatusCode, http.StatusNoContent)
	}
}
