package cloud

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type s3BlobStore struct {
	client    *minio.Client
	bucket    string
	namespace string
}

func newS3BlobStore(ctx context.Context, config Config, namespace string) (*s3BlobStore, error) {
	endpoint, _ := url.Parse(config.S3Endpoint)
	var creds *credentials.Credentials
	if config.S3AccessKey != "" {
		creds = credentials.NewStaticV4(config.S3AccessKey, config.S3SecretKey, "")
	} else {
		creds = credentials.NewChainCredentials([]credentials.Provider{
			&credentials.EnvAWS{},
			&credentials.FileAWSCredentials{},
			&credentials.IAM{Region: config.S3Region},
		})
	}
	transport, err := minio.DefaultTransport(endpoint.Scheme == "https")
	if err != nil {
		return nil, err
	}
	transport.ResponseHeaderTimeout = 30 * time.Second
	transport.IdleConnTimeout = 90 * time.Second
	lookup := minio.BucketLookupAuto
	if config.S3PathStyle {
		lookup = minio.BucketLookupPath
	}
	client, err := minio.New(endpoint.Host, &minio.Options{
		Creds: creds, Secure: endpoint.Scheme == "https", Region: config.S3Region,
		BucketLookup: lookup, Transport: transport, MaxRetries: 3,
	})
	if err != nil {
		return nil, err
	}
	exists, err := client.BucketExists(ctx, config.S3Bucket)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, errors.New("configured cloud S3 bucket does not exist")
	}
	return &s3BlobStore{client: client, bucket: config.S3Bucket, namespace: namespace}, nil
}

func (storage *s3BlobStore) Stage(
	ctx context.Context,
	key string,
	body io.Reader,
	maximum int64,
	mediaType string,
) (stagedBlob, error) {
	if maximum < 0 || maximum == int64(^uint64(0)>>1) {
		return stagedBlob{}, errors.New("invalid staged blob size limit")
	}
	hasher := sha256.New()
	limited := &io.LimitedReader{R: body, N: maximum + 1}
	reader := io.TeeReader(limited, hasher)
	info, err := storage.client.PutObject(ctx, storage.bucket, key, reader, -1, minio.PutObjectOptions{
		ContentType: mediaType,
		UserMetadata: map[string]string{
			"layercache-namespace": storage.namespace,
			"layercache-state":     "staged",
		},
	})
	if err != nil {
		return stagedBlob{}, err
	}
	if info.Size > maximum {
		_ = storage.Delete(context.WithoutCancel(ctx), key)
		return stagedBlob{}, ErrQuota
	}
	return stagedBlob{
		Key: key, Digest: hex.EncodeToString(hasher.Sum(nil)), Size: info.Size, MediaType: mediaType,
	}, nil
}

func (storage *s3BlobStore) Commit(ctx context.Context, staged stagedBlob, destination string) error {
	_, err := storage.client.CopyObject(ctx, minio.CopyDestOptions{
		Bucket: storage.bucket, Object: destination, ReplaceMetadata: true,
		UserMetadata: map[string]string{
			"layercache-namespace": storage.namespace,
			"layercache-state":     "committed",
			"sha256":               staged.Digest,
			"size":                 strconv.FormatInt(staged.Size, 10),
		},
		ContentType: staged.MediaType,
	}, minio.CopySrcOptions{Bucket: storage.bucket, Object: staged.Key})
	return err
}

func (storage *s3BlobStore) Open(
	ctx context.Context,
	key string,
	expectedDigest string,
	expectedSize int64,
) (io.ReadCloser, error) {
	info, err := storage.client.StatObject(ctx, storage.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if s3NotFound(err) {
			return nil, ErrCorrupt
		}
		return nil, err
	}
	if info.Size != expectedSize {
		return nil, ErrCorrupt
	}
	storedDigest := info.Metadata.Get("X-Amz-Meta-Sha256")
	if storedDigest == "" {
		storedDigest = info.UserMetadata["sha256"]
	}
	if storedDigest != "" && storedDigest != expectedDigest {
		return nil, ErrCorrupt
	}
	object, err := storage.client.GetObject(ctx, storage.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	return &verifiedObject{
		body: object, hash: sha256.New(), expectedDigest: expectedDigest, expectedSize: expectedSize,
	}, nil
}

func (storage *s3BlobStore) Stat(ctx context.Context, key string) (blobInfo, error) {
	info, err := storage.client.StatObject(ctx, storage.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if s3NotFound(err) {
			return blobInfo{}, ErrNotFound
		}
		return blobInfo{}, err
	}
	digest := info.Metadata.Get("X-Amz-Meta-Sha256")
	if digest == "" {
		digest = info.UserMetadata["sha256"]
	}
	return blobInfo{Key: info.Key, Digest: digest, Size: info.Size, LastModified: info.LastModified.UTC()}, nil
}

func (storage *s3BlobStore) Delete(ctx context.Context, key string) error {
	err := storage.client.RemoveObject(ctx, storage.bucket, key, minio.RemoveObjectOptions{})
	if s3NotFound(err) {
		return nil
	}
	return err
}

func (storage *s3BlobStore) List(ctx context.Context, prefix string) <-chan blobListResult {
	results := make(chan blobListResult)
	go func() {
		defer close(results)
		for object := range storage.client.ListObjects(ctx, storage.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
			result := blobListResult{Info: blobInfo{
				Key: object.Key, Size: object.Size, LastModified: object.LastModified.UTC(),
			}}
			if object.Err != nil {
				result.Err = object.Err
			}
			select {
			case results <- result:
			case <-ctx.Done():
				return
			}
		}
	}()
	return results
}

func s3NotFound(err error) bool {
	if err == nil {
		return false
	}
	response := minio.ToErrorResponse(err)
	return response.StatusCode == http.StatusNotFound || response.Code == "NoSuchKey" || response.Code == "NoSuchObject"
}

type verifiedObject struct {
	body           io.ReadCloser
	hash           hash.Hash
	expectedDigest string
	expectedSize   int64
	read           int64
}

func (object *verifiedObject) Read(buffer []byte) (int, error) {
	count, err := object.body.Read(buffer)
	if count > 0 {
		object.read += int64(count)
		_, _ = object.hash.Write(buffer[:count])
	}
	if err == io.EOF {
		actual := hex.EncodeToString(object.hash.Sum(nil))
		if object.read != object.expectedSize || !strings.EqualFold(actual, object.expectedDigest) {
			return count, ErrCorrupt
		}
	}
	return count, err
}

func (object *verifiedObject) Close() error {
	return object.body.Close()
}

func (object *verifiedObject) String() string {
	return fmt.Sprintf("verified S3 object (%d bytes read)", object.read)
}
