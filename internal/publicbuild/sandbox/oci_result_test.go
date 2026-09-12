package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
)

func TestManualPublishOCIImageLayout(t *testing.T) {
	repository := os.Getenv("LAYERCACHE_OCI_REGISTRY_QA")
	if repository == "" {
		t.Skip("set LAYERCACHE_OCI_REGISTRY_QA to a disposable OCI repository")
	}
	output, rootDigest, manifest := writeOCIResultFixture(t, false)
	result, err := PublishOCIImageLayout(context.Background(), output, repository)
	if err != nil {
		t.Fatal(err)
	}
	if result.Digest != rootDigest || !bytes.Equal(result.Manifest, manifest) {
		t.Fatalf("published OCI root = %s %q, want %s %q", result.Digest, result.Manifest, rootDigest, manifest)
	}

	// Exercise the consumer-facing invariant, not just the push response: the
	// exact digest returned to Public Build callers must pull a complete graph.
	remoteRepository, err := newOCIRepository(repository)
	if err != nil {
		t.Fatal(err)
	}
	if loopbackRegistry(remoteRepository.Reference.Registry) {
		remoteRepository.PlainHTTP = true
	}
	root := ocispec.Descriptor{
		MediaType: result.MediaType,
		Digest:    digest.Digest(result.Digest),
		Size:      result.SizeBytes,
	}
	pulled := memory.New()
	copyOptions := oras.DefaultCopyGraphOptions
	copyOptions.MaxMetadataBytes = maximumOCIManifestBytes
	if err := oras.CopyGraph(context.Background(), remoteRepository, pulled, root, copyOptions); err != nil {
		t.Fatalf("pull published OCI graph by digest: %v", err)
	}
	pulledManifest, err := content.FetchAll(context.Background(), pulled, root)
	if err != nil {
		t.Fatalf("read pulled root manifest: %v", err)
	}
	if !bytes.Equal(pulledManifest, manifest) {
		t.Fatal("pulled root manifest differs from the guest-validated manifest")
	}
	var imageManifest ocispec.Manifest
	if err := json.Unmarshal(pulledManifest, &imageManifest); err != nil {
		t.Fatalf("decode pulled root manifest: %v", err)
	}
	for _, descriptor := range append([]ocispec.Descriptor{imageManifest.Config}, imageManifest.Layers...) {
		exists, err := pulled.Exists(context.Background(), descriptor)
		if err != nil || !exists {
			t.Fatalf("pulled graph is missing %s: exists=%v err=%v", descriptor.Digest, exists, err)
		}
	}
}

func TestValidateOCIImageLayoutReturnsExactRootManifest(t *testing.T) {
	t.Parallel()
	output, rootDigest, manifest := writeOCIResultFixture(t, false)
	result, err := validateOCIImageLayout(output)
	if err != nil {
		t.Fatal(err)
	}
	defer result.cleanup()
	if result.root.Digest.String() != rootDigest || !bytes.Equal(result.manifest, manifest) {
		t.Fatalf("validated root = %s %q, want %s %q", result.root.Digest, result.manifest, rootDigest, manifest)
	}
}

func TestValidateOCIImageLayoutRejectsDescriptorDigestMismatch(t *testing.T) {
	t.Parallel()
	output, _, _ := writeOCIResultFixture(t, true)
	_, err := validateOCIImageLayout(output)
	if err == nil || !strings.Contains(err.Error(), "descriptor digest") {
		t.Fatalf("descriptor mismatch error = %v", err)
	}
}

func writeOCIResultFixture(t *testing.T, corruptLayer bool) (CollectedOutput, string, []byte) {
	t.Helper()
	config := []byte(`{"created":"2026-08-31T00:00:00Z"}`)
	layer := []byte("buildkit-cache-layer")
	configDescriptor := fixtureDescriptor("application/vnd.buildkit.cacheconfig.v0", config)
	layerDescriptor := fixtureDescriptor(ocispec.MediaTypeImageLayer, layer)
	manifest, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     ocispec.MediaTypeImageManifest,
		"config":        configDescriptor,
		"layers":        []ocispec.Descriptor{layerDescriptor},
	})
	if err != nil {
		t.Fatal(err)
	}
	root := fixtureDescriptor(ocispec.MediaTypeImageManifest, manifest)
	index, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     ocispec.MediaTypeImageIndex,
		"manifests":     []ocispec.Descriptor{root},
	})
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		ocispec.ImageLayoutFile:                             []byte(`{"imageLayoutVersion":"1.0.0"}`),
		ocispec.ImageIndexFile:                              index,
		"blobs/sha256/" + root.Digest.Encoded():             manifest,
		"blobs/sha256/" + configDescriptor.Digest.Encoded(): config,
		"blobs/sha256/" + layerDescriptor.Digest.Encoded():  layer,
	}
	if corruptLayer {
		files["blobs/sha256/"+layerDescriptor.Digest.Encoded()] = []byte("same-size-wrong-data")
	}
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	for _, directory := range []string{"blobs", "blobs/sha256"} {
		if err := writer.WriteHeader(&tar.Header{Name: directory, Typeflag: tar.TypeDir, Mode: 0o700}); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{
		ocispec.ImageLayoutFile,
		ocispec.ImageIndexFile,
		"blobs/sha256/" + root.Digest.Encoded(),
		"blobs/sha256/" + configDescriptor.Digest.Encoded(),
		"blobs/sha256/" + layerDescriptor.Digest.Encoded(),
	} {
		content := files[name]
		if err := writer.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "result.tar")
	if err := os.WriteFile(path, archive.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(archive.Bytes())
	return CollectedOutput{
		NativeKey: "buildkit-fixture", MediaType: OCIImageLayoutTarMediaType,
		ResultFormat: ResultFormatOCIImageLayout, ResultMediaType: ocispec.MediaTypeImageManifest,
		Digest: "sha256:" + hex.EncodeToString(digest[:]), SizeBytes: int64(archive.Len()), Path: path,
	}, root.Digest.String(), manifest
}

func fixtureDescriptor(mediaType string, content []byte) ocispec.Descriptor {
	sum := sha256.Sum256(content)
	return ocispec.Descriptor{
		MediaType: mediaType, Digest: digest.Digest("sha256:" + hex.EncodeToString(sum[:])), Size: int64(len(content)),
	}
}
