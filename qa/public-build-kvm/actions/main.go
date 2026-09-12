//go:build linux

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
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/layercache/layercache/internal/publicbuild"
	"github.com/layercache/layercache/internal/publicbuild/sandbox"
	"github.com/layercache/layercache/internal/publicbuild/sandbox/actionsjob"
	"github.com/layercache/layercache/qa/public-build-kvm/native"
)

const (
	target      = ".github/workflows/public-cache.yml#public-cache"
	cacheKey    = "fixture-key"
	cachePath   = "cache/"
	payload     = "public-cache-payload-from-real-kvm"
	toolchain   = "actions/cache@6.2.0"
	builder     = "layercache-actions-builder-v1"
	offlineMode = "offline-only; dependencies must be vendored in source or pinned in the immutable image"
)

type options struct {
	assetRoot  string
	cgroupRoot string
	qemu       string
	qemuImage  string
	mke2fs     string
	debugfs    string
	sandboxUID uint
	sandboxGID uint
}

type sourceFetcher struct {
	recipeDigest string
	platform     publicbuild.Platform
}

func (fetcher sourceFetcher) Fetch(
	_ context.Context,
	repository string,
	commit string,
	destination string,
	maximum int64,
) error {
	if repository != "https://github.com/acme/project" || commit != strings.Repeat("a", 40) {
		return errors.New("unexpected sealed source identity")
	}
	runner := "ubuntu-24.04"
	if fetcher.platform == publicbuild.PlatformLinuxARM64 {
		runner = "ubuntu-24.04-arm"
	}
	workflow := fmt.Sprintf(`name: Maintained Public Build
on: workflow_dispatch
jobs:
  public-cache:
    runs-on: %s
    steps:
      - name: Checkout sealed source
        uses: actions/checkout@cccccccccccccccccccccccccccccccccccccccc
        with:
          persist-credentials: false
      - name: Build cache
        shell: sh
        run: |
          test "$GITHUB_JOB" = public-cache
          test "$GITHUB_WORKFLOW_REF" = acme/project/.github/workflows/public-cache.yml@refs/heads/main
          mkdir -p cache
          printf '%s' > cache/artifact.txt
      - name: Publish cache
        uses: layercache/layercache/action/cache@dddddddddddddddddddddddddddddddddddddddd
        with:
          path: cache/
          key: fixture-key
          compatibility: %s
          public-cache-mode: verified
          public-recipe-digest: %s
          public-platform: %s
          public-toolchain: actions/cache@6.2.0
          public-builder: layercache-actions-builder-v1
`, runner, payload, actionsCompatibility(fetcher.platform), fetcher.recipeDigest, fetcher.platform)
	if int64(len(workflow)) > maximum {
		return errors.New("workflow exceeds source limit")
	}
	directory := filepath.Join(destination, ".github", "workflows")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(directory, "public-cache.yml"), []byte(workflow), 0o600)
}

type evidence struct {
	Preflight          sandbox.PreflightReport        `json:"preflight"`
	Publication        publicbuild.Publication        `json:"publication"`
	Collected          sandbox.CollectedOutput        `json:"collected"`
	ProducerDurationMS int64                          `json:"producerDurationMilliseconds"`
	ArchiveMembers     map[string]string              `json:"archiveMembers"`
	OutputDigestOK     bool                           `json:"outputDigestVerified"`
	NativeKeyOK        bool                           `json:"nativeKeyVerified"`
	GuestLogs          []string                       `json:"guestLogs"`
	Capabilities       publicbuild.WorkerCapabilities `json:"capabilities"`
}

func main() {
	var config options
	flag.StringVar(&config.assetRoot, "asset-root", "", "root containing vmlinuz, rootfs.raw, contract.json, and work/")
	flag.StringVar(&config.cgroupRoot, "cgroup-root", "", "empty delegated cgroup v2 root")
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
		return errors.New("the maintained Actions KVM QA harness must run as root")
	}
	if config.assetRoot == "" || config.cgroupRoot == "" || config.sandboxUID == 0 || config.sandboxGID == 0 {
		return errors.New("--asset-root, --cgroup-root, --sandbox-uid, and --sandbox-gid are required")
	}
	recipeDigest, err := publicbuild.MaintainedRecipeDigest(publicbuild.IntegrationActions, target)
	if err != nil {
		return err
	}
	version := actionsjob.CacheVersion([]string{cachePath}, "gzip")
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
		WorkerID: "actions-kvm-e2e",
		QEMUPath: config.qemu, QEMUImagePath: config.qemuImage,
		Mke2fsPath: config.mke2fs, DebugFSPath: config.debugfs,
		KernelPath:      filepath.Join(config.assetRoot, "vmlinuz"),
		KernelSHA256:    kernelDigest,
		RootFSPath:      filepath.Join(config.assetRoot, "rootfs.raw"),
		RootFSSHA256:    rootfsDigest,
		ContractPath:    filepath.Join(config.assetRoot, "contract.json"),
		ContractSHA256:  contractDigest,
		CgroupRoot:      config.cgroupRoot,
		WorkRoot:        filepath.Join(config.assetRoot, "work"),
		SandboxUID:      uint32(config.sandboxUID),
		SandboxGID:      uint32(config.sandboxGID),
		MaxScratchBytes: 2 << 30, SourceArchiveBytes: 64 << 20,
		ConnectTimeout: 30 * time.Second, ShutdownTimeout: 15 * time.Second,
		SourceFetcher: sourceFetcher{recipeDigest: recipeDigest, platform: native.Platform()},
	})
	if err != nil {
		return err
	}
	defer worker.Close()
	if err := worker.Contract().CoversServerRecipes([]string{recipeDigest}); err != nil {
		return fmt.Errorf("production recipe admission: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	preflight, err := worker.Preflight(ctx)
	if err != nil {
		return fmt.Errorf("preflight: %w", err)
	}
	if preflight.Network != "none" || preflight.DependencyMode != offlineMode {
		return fmt.Errorf("unexpected isolation report: %#v", preflight)
	}
	request := publicbuild.BuildRequest{
		Repository: "https://github.com/acme/project", Commit: strings.Repeat("a", 40),
		Integration: publicbuild.IntegrationActions, Target: target,
		RecipeDigest: recipeDigest, Platform: native.Platform(),
		Inputs: []publicbuild.DeclaredInput{
			{Name: "actions.key", Value: cacheKey},
			{Name: "actions.ref", Value: "refs/heads/main"},
			{Name: "actions.version", Value: version},
			{Name: "compatibility", Value: actionsCompatibility(native.Platform())},
		},
		Resources: publicbuild.Resources{
			CPUMillis: 1000, MemoryBytes: 512 << 20, DiskBytes: 256 << 20,
			Timeout: 2 * time.Minute,
		},
	}
	build := publicbuild.Build{ID: "actions-kvm-e2e-build", Request: request}
	var logs []string
	publication, err := worker.Execute(ctx, build, func(_ context.Context, message string) error {
		logs = append(logs, message)
		return nil
	})
	if err != nil {
		return fmt.Errorf("execute maintained Actions recipe: %w", err)
	}
	defer worker.Discard(build.ID)
	collected, duration, err := worker.Collected(build.ID)
	if err != nil {
		return err
	}
	file, err := collected.Open()
	if err != nil {
		return err
	}
	encoded, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return errors.Join(readErr, closeErr)
	}
	digest := sha256.Sum256(encoded)
	digestOK := collected.Digest == "sha256:"+hex.EncodeToString(digest[:])
	if !digestOK {
		return errors.New("host-collected output digest mismatch")
	}
	expectedKey, err := publicbuild.ActionsPublicNativeKey(request, toolchain, builder)
	if err != nil {
		return err
	}
	nativeKeyOK := collected.NativeKey == expectedKey
	if !nativeKeyOK {
		return errors.New("host-collected native key mismatch")
	}
	members, err := readArchive(encoded)
	if err != nil {
		return err
	}
	if members["cache/artifact.txt"] != payload {
		return fmt.Errorf("archive payload = %q", members["cache/artifact.txt"])
	}
	if len(publication.Outputs) != 1 || publication.Outputs[0].Digest != collected.Digest ||
		publication.Outputs[0].Name != collected.NativeKey || publication.Outputs[0].SizeBytes != collected.SizeBytes {
		return errors.New("publication descriptor does not match collected bytes")
	}
	for _, message := range logs {
		if strings.Contains(strings.ToLower(message), "token") || strings.Contains(strings.ToLower(message), "secret") {
			return errors.New("guest log contained a secret-shaped value")
		}
	}
	result := evidence{
		Preflight: preflight, Publication: publication, Collected: collected,
		ProducerDurationMS: duration.Milliseconds(), ArchiveMembers: members,
		OutputDigestOK: digestOK, NativeKeyOK: nativeKeyOK, GuestLogs: logs,
		Capabilities: worker.Capabilities(),
	}
	encodedEvidence, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Println(string(encodedEvidence))
	return err
}

func actionsCompatibility(platform publicbuild.Platform) string {
	return strings.ReplaceAll(string(platform), "/", "-") + "-schema1"
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

func readArchive(encoded []byte) (map[string]string, error) {
	compressed, err := gzip.NewReader(bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	defer compressed.Close()
	result := make(map[string]string)
	reader := tar.NewReader(compressed)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return result, nil
		}
		if err != nil {
			return nil, err
		}
		if header.Typeflag == tar.TypeReg {
			body, err := io.ReadAll(reader)
			if err != nil {
				return nil, err
			}
			result[header.Name] = string(body)
		}
	}
}
