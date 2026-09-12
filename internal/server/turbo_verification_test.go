package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/layercache/layercache/internal/artifact"
	"github.com/layercache/layercache/internal/cloud"
	"github.com/layercache/layercache/internal/config"
)

func TestCloudTurboReadIsVerifiedBeforeHTTPResponse(t *testing.T) {
	t.Parallel()

	good := []byte("verified-cloud-turbo-artifact")
	digest := sha256.Sum256(good)
	key := artifact.Key{
		Integration: "turbo", Project: "github.com/acme/widgets",
		Compatibility: "linux-amd64-schema1", Native: "task-hash",
	}
	entry := artifact.Entry{Key: key, Digest: hex.EncodeToString(digest[:]), Size: int64(len(good))}
	for _, test := range []struct {
		name       string
		open       func() io.ReadCloser
		wantStatus int
		wantBody   []byte
		wantDelete bool
	}{
		{
			name: "normal verified stream",
			open: func() io.ReadCloser {
				return io.NopCloser(bytes.NewReader(good))
			},
			wantStatus: http.StatusOK, wantBody: good,
		},
		{
			name: "full corrupt stream with late error",
			open: func() io.ReadCloser {
				return &lateCorruptReadCloser{reader: bytes.NewReader(bytes.Repeat([]byte("x"), len(good)))}
			},
			wantStatus: http.StatusNotFound, wantDelete: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			local, err := artifact.Open(context.Background(), root, 1<<20, 0)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = local.Close() })
			store := &turboStreamingStore{entry: entry, open: test.open}
			instance := &Server{
				config: config.Config{Role: "team", DataDir: root, MaxBytes: 1 << 20},
				store:  store, localStore: local, cloudStore: &cloud.Store{},
			}
			request := httptest.NewRequest(http.MethodGet, "/v8/artifacts/"+key.Native, nil)
			response := httptest.NewRecorder()

			instance.getTurbo(response, request, key, false)

			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body=%q", response.Code, test.wantStatus, response.Body.String())
			}
			if !bytes.Equal(response.Body.Bytes(), test.wantBody) {
				t.Fatalf("body = %q, want %q", response.Body.Bytes(), test.wantBody)
			}
			if store.deleted != test.wantDelete {
				t.Fatalf("deleted = %t, want %t", store.deleted, test.wantDelete)
			}
			stagingFiles, err := os.ReadDir(filepath.Join(root, "staging"))
			if err != nil {
				t.Fatal(err)
			}
			if len(stagingFiles) != 0 {
				t.Fatalf("verification staging retained %d files after response", len(stagingFiles))
			}
		})
	}
}

type turboStreamingStore struct {
	cacheStore
	entry   artifact.Entry
	open    func() io.ReadCloser
	deleted bool
}

func (store *turboStreamingStore) Get(context.Context, artifact.Key) (artifact.Entry, io.ReadCloser, error) {
	return store.entry, store.open(), nil
}

func (store *turboStreamingStore) Delete(context.Context, artifact.Key) error {
	store.deleted = true
	return nil
}

type lateCorruptReadCloser struct {
	reader *bytes.Reader
}

func (reader *lateCorruptReadCloser) Read(buffer []byte) (int, error) {
	count, _ := reader.reader.Read(buffer)
	if reader.reader.Len() == 0 {
		return count, artifact.ErrCorrupt
	}
	return count, nil
}

func (*lateCorruptReadCloser) Close() error { return nil }
