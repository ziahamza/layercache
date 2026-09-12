package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/retry"
)

const (
	maximumOCILayoutEntries = 32_768
	maximumOCIManifestBytes = 4 << 20
)

// OCIRegistryResult is the exact root manifest which the trusted collector
// pushed to the configured Public Build registry. Manifest is suitable for
// publication through the signed Public Cache metadata path.
type OCIRegistryResult struct {
	NativeKey string
	MediaType string
	Digest    string
	SizeBytes int64
	Manifest  []byte
}

type validatedOCIResult struct {
	layoutPath string
	root       ocispec.Descriptor
	manifest   []byte
	cleanup    func() error
}

// PublishOCIImageLayout validates a complete guest-produced OCI image layout,
// copies its reachable graph to a registry using the host Docker credential
// configuration, then reads the root manifest back by digest. No registry
// credential crosses the guest protocol.
func PublishOCIImageLayout(
	ctx context.Context,
	output CollectedOutput,
	repository string,
) (OCIRegistryResult, error) {
	if output.ResultFormat != ResultFormatOCIImageLayout ||
		output.MediaType != OCIImageLayoutTarMediaType ||
		output.ResultMediaType != ocispec.MediaTypeImageManifest {
		return OCIRegistryResult{}, errors.New("BuildKit output is not a complete OCI image layout result")
	}
	destination, err := newOCIRepository(repository)
	if err != nil {
		return OCIRegistryResult{}, err
	}
	result, err := validateOCIImageLayout(output)
	if err != nil {
		return OCIRegistryResult{}, err
	}
	defer result.cleanup()

	source, err := oci.New(result.layoutPath)
	if err != nil {
		return OCIRegistryResult{}, fmt.Errorf("open validated OCI image layout: %w", err)
	}
	if loopbackRegistry(destination.Reference.Registry) {
		destination.PlainHTTP = true
	}
	credentialStore, err := credentials.NewStoreFromDocker(credentials.StoreOptions{})
	if err != nil {
		return OCIRegistryResult{}, fmt.Errorf("open Docker registry credentials: %w", err)
	}
	destination.Client = &auth.Client{
		Client: retry.DefaultClient, Cache: auth.NewCache(),
		Credential: credentials.Credential(credentialStore),
	}
	copyOptions := oras.DefaultCopyGraphOptions
	copyOptions.Concurrency = 3
	copyOptions.MaxMetadataBytes = maximumOCIManifestBytes
	if err := oras.CopyGraph(ctx, source, destination, result.root, copyOptions); err != nil {
		return OCIRegistryResult{}, fmt.Errorf("push complete Public Build OCI result: %w", err)
	}
	remoteRoot, err := destination.Fetch(ctx, result.root)
	if err != nil {
		return OCIRegistryResult{}, fmt.Errorf("read pushed OCI root manifest: %w", err)
	}
	remoteManifest, readErr := io.ReadAll(io.LimitReader(remoteRoot, maximumOCIManifestBytes+1))
	closeErr := remoteRoot.Close()
	if readErr != nil {
		return OCIRegistryResult{}, fmt.Errorf("read pushed OCI root manifest: %w", readErr)
	}
	if closeErr != nil {
		return OCIRegistryResult{}, fmt.Errorf("close pushed OCI root manifest: %w", closeErr)
	}
	if len(remoteManifest) > maximumOCIManifestBytes || !bytes.Equal(remoteManifest, result.manifest) {
		return OCIRegistryResult{}, errors.New("registry root manifest does not match the validated guest result")
	}
	return OCIRegistryResult{
		NativeKey: output.NativeKey, MediaType: result.root.MediaType,
		Digest: result.root.Digest.String(), SizeBytes: result.root.Size,
		Manifest: append([]byte(nil), result.manifest...),
	}, nil
}

// ValidateOCIRepository rejects tags and digests because Public Build results
// are addressed only by the exact root descriptor returned by the guest.
func ValidateOCIRepository(repository string) error {
	_, err := newOCIRepository(repository)
	return err
}

func newOCIRepository(repository string) (*remote.Repository, error) {
	if strings.TrimSpace(repository) != repository || repository == "" || strings.Contains(repository, "@") {
		return nil, errors.New("BuildKit Public Cache repository must be an untagged OCI repository")
	}
	destination, err := remote.NewRepository(repository)
	if err != nil || destination.Reference.Reference != "" {
		return nil, errors.New("BuildKit Public Cache repository must be an untagged OCI repository")
	}
	return destination, nil
}

func validateOCIImageLayout(output CollectedOutput) (validatedOCIResult, error) {
	archive, err := output.Open()
	if err != nil {
		return validatedOCIResult{}, fmt.Errorf("open collected OCI image layout: %w", err)
	}
	defer archive.Close()
	layoutPath, err := os.MkdirTemp(filepath.Dir(output.Path), "oci-layout-")
	if err != nil {
		return validatedOCIResult{}, fmt.Errorf("create OCI image layout staging directory: %w", err)
	}
	cleanup := func() error { return os.RemoveAll(layoutPath) }
	fail := func(err error) (validatedOCIResult, error) {
		_ = cleanup()
		return validatedOCIResult{}, err
	}
	if err := extractOCIImageLayout(layoutPath, archive, output.SizeBytes); err != nil {
		return fail(err)
	}
	root, manifest, err := inspectOCIImageLayout(layoutPath)
	if err != nil {
		return fail(err)
	}
	return validatedOCIResult{
		layoutPath: layoutPath, root: root, manifest: manifest, cleanup: cleanup,
	}, nil
}

func extractOCIImageLayout(destination string, source io.Reader, maximumBytes int64) error {
	if maximumBytes <= 0 {
		return errors.New("OCI image layout byte limit must be positive")
	}
	root, err := os.OpenRoot(destination)
	if err != nil {
		return fmt.Errorf("open OCI image layout staging directory: %w", err)
	}
	defer root.Close()
	reader := tar.NewReader(source)
	seen := make(map[string]struct{})
	entries := 0
	var expanded int64
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read OCI image layout tar: %w", err)
		}
		entries++
		if entries > maximumOCILayoutEntries {
			return fmt.Errorf("OCI image layout exceeds %d entries", maximumOCILayoutEntries)
		}
		name := path.Clean(header.Name)
		if name != header.Name || name == "." || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") {
			return fmt.Errorf("OCI image layout contains unsafe path %q", header.Name)
		}
		if !validOCILayoutPath(name, header.Typeflag == tar.TypeDir) {
			return fmt.Errorf("OCI image layout contains unexpected path %q", name)
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("OCI image layout repeats path %q", name)
		}
		seen[name] = struct{}{}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o700); err != nil {
				return fmt.Errorf("create OCI image layout directory %q: %w", name, err)
			}
		case tar.TypeReg, 0:
			if header.Size < 0 || expanded > maximumBytes-header.Size {
				return fmt.Errorf("OCI image layout exceeds %d expanded bytes", maximumBytes)
			}
			expanded += header.Size
			parent := path.Dir(name)
			if parent != "." {
				if err := root.MkdirAll(parent, 0o700); err != nil {
					return fmt.Errorf("create OCI image layout directory %q: %w", parent, err)
				}
			}
			file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return fmt.Errorf("create OCI image layout file %q: %w", name, err)
			}
			copyErr := copyExactly(file, reader, header.Size)
			closeErr := file.Close()
			if copyErr != nil {
				return fmt.Errorf("extract OCI image layout file %q: %w", name, copyErr)
			}
			if closeErr != nil {
				return fmt.Errorf("close OCI image layout file %q: %w", name, closeErr)
			}
		default:
			return fmt.Errorf("OCI image layout path %q uses forbidden tar type %d", name, header.Typeflag)
		}
	}
	if entries == 0 {
		return errors.New("OCI image layout is empty")
	}
	return nil
}

func validOCILayoutPath(name string, directory bool) bool {
	if directory {
		return name == "blobs" || name == "blobs/sha256"
	}
	if name == ocispec.ImageLayoutFile || name == ocispec.ImageIndexFile {
		return true
	}
	const prefix = "blobs/sha256/"
	hexDigest := strings.TrimPrefix(name, prefix)
	if len(hexDigest) != 64 || prefix+hexDigest != name || hexDigest != strings.ToLower(hexDigest) {
		return false
	}
	_, err := hex.DecodeString(hexDigest)
	return err == nil
}

func inspectOCIImageLayout(layoutPath string) (ocispec.Descriptor, []byte, error) {
	layoutBytes, err := readBoundedRegularFile(filepath.Join(layoutPath, ocispec.ImageLayoutFile), 4096)
	if err != nil {
		return ocispec.Descriptor{}, nil, fmt.Errorf("read OCI image layout marker: %w", err)
	}
	var layout ocispec.ImageLayout
	if json.Unmarshal(layoutBytes, &layout) != nil || layout.Version != ocispec.ImageLayoutVersion {
		return ocispec.Descriptor{}, nil, errors.New("OCI image layout marker is invalid")
	}
	indexBytes, err := readBoundedRegularFile(filepath.Join(layoutPath, ocispec.ImageIndexFile), maximumOCIManifestBytes)
	if err != nil {
		return ocispec.Descriptor{}, nil, fmt.Errorf("read OCI image index: %w", err)
	}
	var index ocispec.Index
	if err := json.Unmarshal(indexBytes, &index); err != nil || index.SchemaVersion != 2 || len(index.Manifests) != 1 {
		return ocispec.Descriptor{}, nil, errors.New("OCI image index must contain one schema-2 root manifest")
	}
	root := index.Manifests[0]
	if root.MediaType != ocispec.MediaTypeImageManifest || len(root.URLs) != 0 || len(root.Data) != 0 {
		return ocispec.Descriptor{}, nil, errors.New("OCI image layout root is not an OCI image manifest")
	}
	manifestBytes, err := validateOCIDescriptor(layoutPath, root, true)
	if err != nil {
		return ocispec.Descriptor{}, nil, err
	}
	if len(manifestBytes) > maximumOCIManifestBytes {
		return ocispec.Descriptor{}, nil, errors.New("OCI root manifest is too large")
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil || manifest.SchemaVersion != 2 ||
		manifest.Subject != nil || manifest.ArtifactType != "" ||
		manifest.MediaType != "" && manifest.MediaType != ocispec.MediaTypeImageManifest {
		return ocispec.Descriptor{}, nil, errors.New("OCI root manifest is invalid")
	}
	reachable := map[string]ocispec.Descriptor{root.Digest.String(): root}
	descriptors := append([]ocispec.Descriptor{manifest.Config}, manifest.Layers...)
	if len(descriptors) == 0 || len(descriptors) > maximumOCILayoutEntries-1 {
		return ocispec.Descriptor{}, nil, errors.New("OCI root manifest has an invalid descriptor count")
	}
	for _, descriptor := range descriptors {
		if descriptor.MediaType == "" || len(descriptor.URLs) != 0 || len(descriptor.Data) != 0 {
			return ocispec.Descriptor{}, nil, errors.New("OCI result must contain every descriptor locally")
		}
		if previous, duplicate := reachable[descriptor.Digest.String()]; duplicate {
			if previous.Size != descriptor.Size || previous.MediaType != descriptor.MediaType {
				return ocispec.Descriptor{}, nil, errors.New("OCI result repeats a digest with conflicting descriptors")
			}
			continue
		}
		if _, err := validateOCIDescriptor(layoutPath, descriptor, false); err != nil {
			return ocispec.Descriptor{}, nil, err
		}
		reachable[descriptor.Digest.String()] = descriptor
	}
	blobs, err := filepath.Glob(filepath.Join(layoutPath, "blobs", "sha256", "*"))
	if err != nil {
		return ocispec.Descriptor{}, nil, err
	}
	if len(blobs) != len(reachable) {
		return ocispec.Descriptor{}, nil, errors.New("OCI image layout contains unreferenced or missing blobs")
	}
	return root, manifestBytes, nil
}

func readBoundedRegularFile(filePath string, maximum int64) ([]byte, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maximum {
		return nil, fmt.Errorf("file %q is not a bounded regular file", filepath.Base(filePath))
	}
	encoded, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(encoded)) > maximum {
		return nil, fmt.Errorf("file %q exceeds %d bytes", filepath.Base(filePath), maximum)
	}
	return encoded, nil
}

func validateOCIDescriptor(layoutPath string, descriptor ocispec.Descriptor, keepContent bool) ([]byte, error) {
	if descriptor.Digest.Algorithm() != digest.SHA256 || descriptor.Digest.Validate() != nil || descriptor.Size < 0 {
		return nil, errors.New("OCI result contains an invalid descriptor")
	}
	blobPath := filepath.Join(layoutPath, "blobs", "sha256", descriptor.Digest.Encoded())
	file, err := os.Open(blobPath)
	if err != nil {
		return nil, fmt.Errorf("open OCI blob %s: %w", descriptor.Digest, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != descriptor.Size {
		return nil, fmt.Errorf("OCI blob %s does not match its descriptor size", descriptor.Digest)
	}
	hasher := sha256.New()
	var content []byte
	if keepContent {
		if descriptor.Size > maximumOCIManifestBytes {
			return nil, errors.New("OCI root manifest is too large")
		}
		content, err = io.ReadAll(io.TeeReader(file, hasher))
	} else {
		_, err = io.Copy(hasher, file)
	}
	if err != nil {
		return nil, fmt.Errorf("read OCI blob %s: %w", descriptor.Digest, err)
	}
	got := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	if got != descriptor.Digest.String() {
		return nil, fmt.Errorf("OCI blob %s does not match its descriptor digest", descriptor.Digest)
	}
	return content, nil
}

func loopbackRegistry(registry string) bool {
	host := registry
	if parsedHost, _, err := net.SplitHostPort(registry); err == nil {
		host = parsedHost
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}
