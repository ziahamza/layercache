package cloud

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Exercise the real SDK's unknown-length streaming path. The reader stops the
// upload after observing the actual allocated read buffer, without a large body.
func TestS3StageBoundsStreamingBufferByAdmissionLimit(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Query().Has("uploads") {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(w, "<InitiateMultipartUploadResult><Bucket>cache</Bucket><Key>fixture</Key><UploadId>fixture-upload</UploadId></InitiateMultipartUploadResult>")
			return
		}
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		t.Errorf("unexpected S3 operation: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer endpoint.Close()
	client, err := minio.New(strings.TrimPrefix(endpoint.URL, "http://"), &minio.Options{
		Creds:  credentials.NewStaticV4("fixture-access", "fixture-secret", ""),
		Region: "us-east-1", BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	storage := s3BlobStore{client: client, bucket: "cache", namespace: "fixture"}
	reader := &bufferProbe{}
	_, err = storage.Stage(context.Background(), "fixture", reader, 5<<30, "application/octet-stream")
	if !errors.Is(err, errBufferProbe) {
		t.Fatalf("expected probe stop, got %v", err)
	}
	if reader.size == 0 || reader.size > 16<<20 {
		t.Fatalf("streaming buffer = %d bytes; want at most 16 MiB for a 5 GiB admission limit", reader.size)
	}
}

var errBufferProbe = errors.New("stop after observing upload buffer")

type bufferProbe struct{ size int }

func (probe *bufferProbe) Read(buffer []byte) (int, error) {
	probe.size = len(buffer)
	return 0, errBufferProbe
}

func TestS3StageCanceledBeforeBufferAdmission(t *testing.T) {
	if err := stagingBuffers.Acquire(context.Background(), stagingBufferBudget); err != nil {
		t.Fatal(err)
	}
	defer stagingBuffers.Release(stagingBufferBudget)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// No client: a canceled waiter must return before reaching the SDK or body.
	storage := s3BlobStore{}
	_, err := storage.Stage(ctx, "fixture", strings.NewReader("payload"), 5<<30, "application/octet-stream")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled admission, got %v", err)
	}
}
