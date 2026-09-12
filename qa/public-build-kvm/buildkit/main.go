package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/layercache/layercache/internal/publicbuild"
	"github.com/layercache/layercache/internal/publicbuild/sandbox"
	"github.com/layercache/layercache/qa/public-build-kvm/native"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/retry"
)

const (
	target      = "qa"
	buildOutput = "maintained-buildkit-kvm-e2e"
	offlineMode = "offline-only; dependencies must be vendored in source or pinned in the immutable image"
)

type options struct {
	assetRoot     string
	cgroupRoot    string
	repository    string
	staticBusybox string
	qemu          string
	qemuImage     string
	mke2fs        string
	debugfs       string
	sandboxUID    uint
	sandboxGID    uint
}

type sourceFetcher struct {
	busyboxPath string
}

func (fetcher sourceFetcher) Fetch(
	_ context.Context,
	repository string,
	commit string,
	destination string,
	maximum int64,
) error {
	if repository != "https://github.com/layercache/manual-buildkit-qa" || commit != strings.Repeat("a", 40) {
		return errors.New("unexpected sealed source identity")
	}
	dockerfile := "FROM scratch AS qa\n" +
		"COPY busybox /bin/busybox\n" +
		"COPY busybox /bin/sh\n" +
		"COPY README.txt /README.txt\n" +
		"RUN [\"/bin/busybox\",\"sh\",\"-c\",\"printf maintained-buildkit-kvm-e2e > /build-output.txt\"]\n" +
		"LABEL org.layercache.qa=real-qemu-buildkit\n"
	busybox, err := os.ReadFile(fetcher.busyboxPath)
	if err != nil {
		return err
	}
	readme := []byte("maintained BuildKit QEMU/KVM end-to-end QA\n")
	if int64(len(dockerfile)+len(busybox)+len(readme)) > maximum {
		return errors.New("BuildKit QA source exceeds its byte limit")
	}
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return err
	}
	for name, contents := range map[string][]byte{
		"Dockerfile": []byte(dockerfile), "README.txt": readme, "busybox": busybox,
	} {
		mode := os.FileMode(0o400)
		if name == "busybox" {
			mode = 0o500
		}
		if err := os.WriteFile(filepath.Join(destination, name), contents, mode); err != nil {
			return err
		}
	}
	return nil
}

type evidence struct {
	Preflight          sandbox.PreflightReport   `json:"preflight"`
	Publication        publicbuild.Publication   `json:"publication"`
	Collected          sandbox.CollectedOutput   `json:"collected"`
	ProducerDurationMS int64                     `json:"producerDurationMilliseconds"`
	Registry           sandbox.OCIRegistryResult `json:"registry"`
	Layers             int                       `json:"layers"`
	OutputDigestOK     bool                      `json:"outputDigestVerified"`
	NativeKeyOK        bool                      `json:"nativeKeyVerified"`
	RunOutputOK        bool                      `json:"runOutputVerified"`
	GuestLogs          []string                  `json:"guestLogs"`
}

func main() {
	var config options
	flag.StringVar(&config.assetRoot, "asset-root", "", "root containing vmlinuz, rootfs.raw, contract.json, and work/")
	flag.StringVar(&config.cgroupRoot, "cgroup-root", "", "empty delegated cgroup v2 root")
	flag.StringVar(&config.repository, "repository", "", "disposable untagged OCI registry repository")
	flag.StringVar(&config.staticBusybox, "static-busybox", "/bin/busybox", "static BusyBox copied into the sealed source")
	flag.StringVar(&config.qemu, "qemu", native.QEMUPath(), "native QEMU system binary")
	flag.StringVar(&config.qemuImage, "qemu-img", "/usr/bin/qemu-img", "qemu-img binary")
	flag.StringVar(&config.mke2fs, "mke2fs", "/usr/sbin/mke2fs", "trusted host mke2fs binary")
	flag.StringVar(&config.debugfs, "debugfs", "/usr/sbin/debugfs", "trusted host debugfs binary")
	flag.UintVar(&config.sandboxUID, "sandbox-uid", 64000, "dedicated non-root QEMU UID")
	flag.UintVar(&config.sandboxGID, "sandbox-gid", 0, "dedicated QEMU GID with KVM access")
	flag.Parse()
	if err := run(config); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(config options) error {
	if os.Geteuid() != 0 {
		return errors.New("the maintained BuildKit KVM QA harness must run as root")
	}
	if config.assetRoot == "" || config.cgroupRoot == "" || config.repository == "" ||
		config.sandboxUID == 0 || config.sandboxGID == 0 {
		return errors.New("--asset-root, --cgroup-root, --repository, --sandbox-uid, and --sandbox-gid are required")
	}
	if err := sandbox.ValidateOCIRepository(config.repository); err != nil {
		return err
	}
	if err := native.ValidateExecutable(config.staticBusybox, native.Platform(), true); err != nil {
		return err
	}
	recipeDigest, err := publicbuild.MaintainedRecipeDigest(publicbuild.IntegrationBuildKit, target)
	if err != nil {
		return err
	}
	kernelDigest, err := fileDigest(filepath.Join(config.assetRoot, "vmlinuz"))
	if err != nil {
		return err
	}
	rootfsDigest, err := fileDigest(filepath.Join(config.assetRoot, "rootfs.raw"))
	if err != nil {
		return err
	}
	contractDigest, err := fileDigest(filepath.Join(config.assetRoot, "contract.json"))
	if err != nil {
		return err
	}
	worker, err := sandbox.NewQEMUWorker(sandbox.QEMUConfig{
		WorkerID: "buildkit-kvm-e2e",
		QEMUPath: config.qemu, QEMUImagePath: config.qemuImage,
		Mke2fsPath: config.mke2fs, DebugFSPath: config.debugfs,
		KernelPath: filepath.Join(config.assetRoot, "vmlinuz"), KernelSHA256: kernelDigest,
		RootFSPath: filepath.Join(config.assetRoot, "rootfs.raw"), RootFSSHA256: rootfsDigest,
		ContractPath: filepath.Join(config.assetRoot, "contract.json"), ContractSHA256: contractDigest,
		CgroupRoot: config.cgroupRoot, WorkRoot: filepath.Join(config.assetRoot, "work"),
		SandboxUID: uint32(config.sandboxUID), SandboxGID: uint32(config.sandboxGID),
		MaxScratchBytes: 8 << 30, SourceArchiveBytes: 64 << 20,
		ConnectTimeout: 45 * time.Second, ShutdownTimeout: 20 * time.Second,
		SourceFetcher: sourceFetcher{busyboxPath: config.staticBusybox},
	})
	if err != nil {
		return err
	}
	defer worker.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	preflight, err := worker.Preflight(ctx)
	if err != nil {
		return fmt.Errorf("preflight: %w", err)
	}
	if preflight.Network != "none" || preflight.DependencyMode != offlineMode {
		return fmt.Errorf("unexpected isolation report: %#v", preflight)
	}
	request := publicbuild.BuildRequest{
		Repository: "https://github.com/layercache/manual-buildkit-qa",
		Commit:     strings.Repeat("a", 40), Integration: publicbuild.IntegrationBuildKit,
		Target: target, RecipeDigest: recipeDigest, Platform: native.Platform(),
		Resources: publicbuild.Resources{
			CPUMillis: 2000, MemoryBytes: 1 << 30, DiskBytes: 1 << 30,
			Timeout: 4 * time.Minute,
		},
	}
	build := publicbuild.Build{ID: "buildkit-kvm-e2e-build", Request: request}
	var logs []string
	publication, err := worker.Execute(ctx, build, func(_ context.Context, message string) error {
		logs = append(logs, message)
		return nil
	})
	if err != nil {
		return fmt.Errorf("execute maintained BuildKit recipe: %w", err)
	}
	defer worker.Discard(build.ID)
	output, duration, err := worker.Collected(build.ID)
	if err != nil {
		return err
	}
	outputDigestOK, err := verifyCollectedDigest(output)
	if err != nil || !outputDigestOK {
		return fmt.Errorf("verify collected output digest: %w", err)
	}
	expectedKey, err := publicbuild.BuildKitPublicNativeKey(request)
	if err != nil {
		return err
	}
	nativeKeyOK := output.NativeKey == expectedKey
	if !nativeKeyOK {
		return errors.New("host-collected native key mismatch")
	}
	published, err := sandbox.PublishOCIImageLayout(ctx, output, config.repository)
	if err != nil {
		return fmt.Errorf("publish real guest OCI graph: %w", err)
	}
	if published.NativeKey != expectedKey {
		return errors.New("published OCI result changed the native key")
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(published.Manifest, &manifest); err != nil {
		return err
	}
	if len(manifest.Layers) == 0 || manifest.Config.Digest == "" {
		return errors.New("BuildKit cache manifest lacks config or layers")
	}
	runOutputOK, err := pullAndVerifyGraph(ctx, config.repository, published, manifest)
	if err != nil {
		return err
	}
	if !runOutputOK {
		return errors.New("pulled BuildKit graph does not contain the expected real RUN output")
	}
	if len(publication.Outputs) != 1 || publication.Outputs[0].Digest != output.Digest ||
		publication.Outputs[0].Name != output.NativeKey || publication.Outputs[0].SizeBytes != output.SizeBytes {
		return errors.New("publication descriptor does not match collected bytes")
	}
	for _, message := range logs {
		if strings.Contains(strings.ToLower(message), "token") || strings.Contains(strings.ToLower(message), "secret") {
			return errors.New("guest log contained a secret-shaped value")
		}
	}
	result := evidence{
		Preflight: preflight, Publication: publication, Collected: output,
		ProducerDurationMS: duration.Milliseconds(), Registry: published,
		Layers: len(manifest.Layers), OutputDigestOK: outputDigestOK,
		NativeKeyOK: nativeKeyOK, RunOutputOK: runOutputOK, GuestLogs: logs,
	}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Println(string(encoded))
	return err
}

func pullAndVerifyGraph(
	ctx context.Context,
	repository string,
	published sandbox.OCIRegistryResult,
	manifest ocispec.Manifest,
) (bool, error) {
	remoteRepository, err := remote.NewRepository(repository)
	if err != nil {
		return false, err
	}
	if loopbackRegistry(remoteRepository.Reference.Registry) {
		remoteRepository.PlainHTTP = true
	}
	credentialStore, err := credentials.NewStoreFromDocker(credentials.StoreOptions{})
	if err != nil {
		return false, fmt.Errorf("open Docker registry credentials: %w", err)
	}
	remoteRepository.Client = &auth.Client{
		Client: retry.DefaultClient, Cache: auth.NewCache(),
		Credential: credentials.Credential(credentialStore),
	}
	root := ocispec.Descriptor{
		MediaType: published.MediaType, Digest: digest.Digest(published.Digest),
		Size: published.SizeBytes,
	}
	if err := root.Digest.Validate(); err != nil {
		return false, err
	}
	pulled := memory.New()
	copyOptions := oras.DefaultCopyGraphOptions
	copyOptions.MaxMetadataBytes = 4 << 20
	if err := oras.CopyGraph(ctx, remoteRepository, pulled, root, copyOptions); err != nil {
		return false, fmt.Errorf("pull published graph by digest: %w", err)
	}
	pulledManifest, err := content.FetchAll(ctx, pulled, root)
	if err != nil {
		return false, fmt.Errorf("fetch pulled root manifest: %w", err)
	}
	if !bytes.Equal(pulledManifest, published.Manifest) {
		return false, errors.New("pulled root manifest differs from the published manifest")
	}
	for _, descriptor := range append([]ocispec.Descriptor{manifest.Config}, manifest.Layers...) {
		exists, existsErr := pulled.Exists(ctx, descriptor)
		if existsErr != nil {
			return false, fmt.Errorf("inspect pulled descriptor %s: %w", descriptor.Digest, existsErr)
		}
		if !exists {
			return false, fmt.Errorf("pulled graph misses %s", descriptor.Digest)
		}
	}
	for _, descriptor := range manifest.Layers {
		layer, err := content.FetchAll(ctx, pulled, descriptor)
		if err != nil {
			return false, err
		}
		contains, err := layerContainsBuildOutput(layer, descriptor.MediaType)
		if err != nil {
			return false, fmt.Errorf("inspect pulled layer %s: %w", descriptor.Digest, err)
		}
		if contains {
			return true, nil
		}
	}
	return false, nil
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

func layerContainsBuildOutput(layer []byte, mediaType string) (bool, error) {
	if mediaType != ocispec.MediaTypeImageLayerGzip {
		return false, nil
	}
	compressed, err := gzip.NewReader(bytes.NewReader(layer))
	if err != nil {
		return false, err
	}
	defer compressed.Close()
	reader := tar.NewReader(compressed)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if header.Name != "build-output.txt" {
			continue
		}
		encoded, err := io.ReadAll(io.LimitReader(reader, 128))
		return bytes.Equal(encoded, []byte(buildOutput)), err
	}
}

func verifyCollectedDigest(output sandbox.CollectedOutput) (bool, error) {
	file, err := output.Open()
	if err != nil {
		return false, err
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return false, err
	}
	return output.Digest == "sha256:"+hex.EncodeToString(hasher.Sum(nil)), nil
}

func fileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil)), nil
}
