package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/layercache/layercache/internal/publicbuild"
	"github.com/layercache/layercache/internal/publicbuild/sandbox"
	"github.com/layercache/layercache/qa/public-build-kvm/native"
)

const (
	repository       = "https://github.com/layercache/turbo-kvm-fixture"
	commit           = "0123456789abcdef0123456789abcdef01234567"
	target           = "@qa/app#build"
	expectedOutput   = "layercache-real-turbo-2.10.12-kvm\n"
	maximumExpansion = 64 << 20
	offlineMode      = "offline-only; dependencies must be vendored in source or pinned in the immutable image"
)

type options struct {
	assetRoot  string
	cgroupRoot string
	turbo      string
	qemu       string
	qemuImage  string
	mke2fs     string
	debugfs    string
	sandboxUID uint
	sandboxGID uint
}

type sourceFetcher struct{}

func (sourceFetcher) Fetch(
	_ context.Context,
	requestedRepository string,
	requestedCommit string,
	destination string,
	maximum int64,
) error {
	if requestedRepository != repository || requestedCommit != commit {
		return errors.New("unexpected sealed source identity")
	}
	return writeFixture(destination, maximum)
}

type fixtureFile struct {
	path string
	body string
}

var fixtureFiles = []fixtureFile{
	{
		path: "package.json",
		body: `{
  "name": "layercache-turbo-kvm-fixture",
  "private": true,
  "packageManager": "npm@11.6.2",
  "workspaces": ["packages/app"]
}
`,
	},
	{
		path: "package-lock.json",
		body: `{
  "name": "layercache-turbo-kvm-fixture",
  "lockfileVersion": 3,
  "requires": true,
  "packages": {
    "": {
      "name": "layercache-turbo-kvm-fixture",
      "workspaces": ["packages/app"]
    },
    "node_modules/@qa/app": {
      "resolved": "packages/app",
      "link": true
    },
    "packages/app": {
      "name": "@qa/app",
      "version": "1.0.0"
    }
  }
}
`,
	},
	{
		path: "turbo.json",
		body: `{
  "$schema": "https://turbo.build/schema.json",
  "tasks": {
    "build": {
      "cache": true,
      "persistent": false,
      "outputs": ["dist/**"]
    }
  }
}
`,
	},
	{
		path: "packages/app/package.json",
		body: `{
  "name": "@qa/app",
  "version": "1.0.0",
  "private": true,
  "scripts": {"build": "node build.js"}
}
`,
	},
	{
		path: "packages/app/build.js",
		body: `"use strict";

const fs = require("node:fs");
fs.mkdirSync("dist", { recursive: true });
fs.writeFileSync("dist/output.txt", "layercache-real-turbo-2.10.12-kvm\n", { mode: 0o644 });
console.log("created deterministic KVM fixture output");
`,
	},
}

func writeFixture(destination string, maximum int64) error {
	var size int64
	for _, file := range fixtureFiles {
		size += int64(len(file.body))
	}
	if size > maximum {
		return fmt.Errorf("Turbo QA fixture exceeds source limit %d", maximum)
	}
	for _, file := range fixtureFiles {
		filePath := filepath.Join(destination, filepath.FromSlash(file.path))
		if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filePath, []byte(file.body), 0o644); err != nil {
			return err
		}
	}
	return nil
}

type evidence struct {
	Preflight          sandbox.PreflightReport        `json:"preflight"`
	Publication        publicbuild.Publication        `json:"publication"`
	Collected          sandbox.CollectedOutput        `json:"collected"`
	ProducerDurationMS int64                          `json:"producerDurationMilliseconds"`
	PlannedNativeKey   string                         `json:"plannedNativeKey"`
	ArchiveMembers     map[string]string              `json:"archiveMembers"`
	RestoreLog         string                         `json:"restoreLog"`
	OutputDigestOK     bool                           `json:"outputDigestVerified"`
	NativeKeyOK        bool                           `json:"nativeKeyVerified"`
	RestoreOK          bool                           `json:"realTurboRestoreVerified"`
	GuestLogs          []string                       `json:"guestLogs"`
	Capabilities       publicbuild.WorkerCapabilities `json:"capabilities"`
}

func main() {
	var config options
	flag.StringVar(&config.assetRoot, "asset-root", "", "root containing vmlinuz, rootfs.raw, contract.json, turbo, and work/")
	flag.StringVar(&config.cgroupRoot, "cgroup-root", "", "empty delegated cgroup v2 root")
	flag.StringVar(&config.turbo, "turbo", "", "Turbo 2.10.12 binary used for the independent plan and restore")
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
		return errors.New("the maintained Turbo KVM QA harness must run as root")
	}
	if config.assetRoot == "" || config.cgroupRoot == "" ||
		config.sandboxUID == 0 || config.sandboxGID == 0 {
		return errors.New("--asset-root, --cgroup-root, --sandbox-uid, and --sandbox-gid are required")
	}
	if config.turbo == "" {
		config.turbo = filepath.Join(config.assetRoot, "turbo")
	}
	if err := native.ValidateExecutable(config.turbo, native.Platform(), false); err != nil {
		return err
	}
	compatibility := strings.ReplaceAll(string(native.Platform()), "/", "-") + "-node@24-schema1"
	recipeDigest, err := publicbuild.MaintainedRecipeDigest(publicbuild.IntegrationTurbo, target)
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
		WorkerID: "turbo-kvm-e2e",
		QEMUPath: config.qemu, QEMUImagePath: config.qemuImage,
		Mke2fsPath: config.mke2fs, DebugFSPath: config.debugfs,
		KernelPath: filepath.Join(config.assetRoot, "vmlinuz"), KernelSHA256: kernelDigest,
		RootFSPath: filepath.Join(config.assetRoot, "rootfs.raw"), RootFSSHA256: rootfsDigest,
		ContractPath: filepath.Join(config.assetRoot, "contract.json"), ContractSHA256: contractDigest,
		CgroupRoot: config.cgroupRoot, WorkRoot: filepath.Join(config.assetRoot, "work"),
		SandboxUID: uint32(config.sandboxUID), SandboxGID: uint32(config.sandboxGID),
		MaxScratchBytes: 4 << 30, SourceArchiveBytes: 64 << 20,
		ConnectTimeout: 30 * time.Second, ShutdownTimeout: 20 * time.Second,
		SourceFetcher: sourceFetcher{},
	})
	if err != nil {
		return err
	}
	defer worker.Close()
	if err := worker.Contract().CoversServerRecipes([]string{recipeDigest}); err != nil {
		return fmt.Errorf("production recipe admission: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	preflight, err := worker.Preflight(ctx)
	if err != nil {
		return fmt.Errorf("preflight: %w", err)
	}
	if preflight.Network != "none" || preflight.DependencyMode != offlineMode {
		return fmt.Errorf("unexpected isolation report: %#v", preflight)
	}
	plannedKey, err := planNativeKey(ctx, config.turbo)
	if err != nil {
		return err
	}
	request := publicbuild.BuildRequest{
		Repository: repository, Commit: commit,
		Integration: publicbuild.IntegrationTurbo, Target: target,
		RecipeDigest: recipeDigest, Platform: native.Platform(),
		Inputs: []publicbuild.DeclaredInput{{Name: "compatibility", Value: compatibility}},
		Resources: publicbuild.Resources{
			CPUMillis: 2000, MemoryBytes: 1536 << 20, DiskBytes: 512 << 20,
			Timeout: 3 * time.Minute,
		},
	}
	if got, err := publicbuild.CompatibilityIdentity(request); err != nil || got != compatibility {
		return fmt.Errorf("validate admitted compatibility = %q: %w", got, err)
	}
	build := publicbuild.Build{ID: "turbo-kvm-e2e-build", Request: request}
	var logs []string
	publication, err := worker.Execute(ctx, build, func(_ context.Context, message string) error {
		logs = append(logs, message)
		return nil
	})
	if err != nil {
		return fmt.Errorf("execute maintained Turbo recipe: %w", err)
	}
	defer worker.Discard(build.ID)
	output, duration, err := worker.Collected(build.ID)
	if err != nil {
		return err
	}
	encoded, err := readCollected(output)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(encoded)
	digestOK := output.Digest == "sha256:"+hex.EncodeToString(digest[:]) && output.SizeBytes == int64(len(encoded))
	if !digestOK {
		return errors.New("host-collected output digest mismatch")
	}
	nativeKeyOK := output.NativeKey == plannedKey
	if !nativeKeyOK {
		return fmt.Errorf("guest native key = %q, independently planned %q", output.NativeKey, plannedKey)
	}
	members, err := readTurboArchive(encoded)
	if err != nil {
		return err
	}
	if members["packages/app/dist/output.txt"] != expectedOutput {
		return fmt.Errorf("archive output = %q", members["packages/app/dist/output.txt"])
	}
	if !strings.Contains(members["packages/app/.turbo/turbo-build.log"], "created deterministic KVM fixture output") {
		return errors.New("archive lacks the genuine Turbo task log")
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
	restoreLog, err := verifyRealTurboRestore(ctx, config.turbo, output.NativeKey, encoded)
	if err != nil {
		return err
	}
	result := evidence{
		Preflight: preflight, Publication: publication, Collected: output,
		ProducerDurationMS: duration.Milliseconds(), PlannedNativeKey: plannedKey,
		ArchiveMembers: members, RestoreLog: restoreLog,
		OutputDigestOK: digestOK, NativeKeyOK: nativeKeyOK, RestoreOK: true,
		GuestLogs: logs, Capabilities: worker.Capabilities(),
	}
	encodedEvidence, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Println(string(encodedEvidence))
	return err
}

func fileDigest(filePath string) (string, error) {
	file, err := os.Open(filePath)
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

func readCollected(output sandbox.CollectedOutput) ([]byte, error) {
	file, err := output.Open()
	if err != nil {
		return nil, err
	}
	encoded, readErr := io.ReadAll(file)
	return encoded, errors.Join(readErr, file.Close())
}

type turboPlan struct {
	Tasks []struct {
		TaskID                 string   `json:"taskId"`
		Hash                   string   `json:"hash"`
		Dependencies           []string `json:"dependencies"`
		ResolvedTaskDefinition struct {
			Cache      *bool `json:"cache"`
			Persistent bool  `json:"persistent"`
		} `json:"resolvedTaskDefinition"`
	} `json:"tasks"`
}

func planNativeKey(ctx context.Context, turbo string) (string, error) {
	fixture, err := os.MkdirTemp("", "layercache-turbo-kvm-plan.")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(fixture)
	if err := writeFixture(fixture, 64<<20); err != nil {
		return "", err
	}
	command := exec.CommandContext(ctx, turbo, "run", target, "--dry=json", "--cache=remote:rw")
	command.Dir = fixture
	command.Env = turboEnvironment(filepath.Join(fixture, "home"), nil)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("independently plan Turbo fixture: %w: %s", err, stderr.String())
	}
	decoder := json.NewDecoder(&stdout)
	var plan turboPlan
	if err := decoder.Decode(&plan); err != nil {
		return "", fmt.Errorf("decode independent Turbo plan: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return "", errors.New("independent Turbo plan contains trailing JSON")
	}
	if len(plan.Tasks) != 1 || plan.Tasks[0].TaskID != target ||
		plan.Tasks[0].Hash == "" || len(plan.Tasks[0].Hash) > 1024 ||
		strings.ContainsAny(plan.Tasks[0].Hash, "/\x00\r\n") ||
		len(plan.Tasks[0].Dependencies) != 0 ||
		plan.Tasks[0].ResolvedTaskDefinition.Cache == nil ||
		!*plan.Tasks[0].ResolvedTaskDefinition.Cache ||
		plan.Tasks[0].ResolvedTaskDefinition.Persistent {
		return "", errors.New("independent Turbo plan is not one dependency-free cacheable task")
	}
	return plan.Tasks[0].Hash, nil
}

func readTurboArchive(encoded []byte) (map[string]string, error) {
	decoder, err := zstd.NewReader(bytes.NewReader(encoded),
		zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(maximumExpansion))
	if err != nil {
		return nil, err
	}
	defer decoder.Close()
	reader := tar.NewReader(io.LimitReader(decoder, maximumExpansion+1))
	result := make(map[string]string)
	seen := make(map[string]struct{})
	var total int64
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		clean := path.Clean(header.Name)
		if clean == "." || clean == ".." ||
			strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
			return nil, fmt.Errorf("unsafe Turbo archive member %q", header.Name)
		}
		if _, duplicate := seen[clean]; duplicate {
			return nil, fmt.Errorf("duplicate Turbo archive member %q", clean)
		}
		seen[clean] = struct{}{}
		switch header.Typeflag {
		case tar.TypeDir:
			if header.Name != clean+"/" {
				return nil, fmt.Errorf("non-canonical Turbo archive directory %q", header.Name)
			}
			continue
		case tar.TypeReg, tar.TypeRegA:
			if header.Name != clean {
				return nil, fmt.Errorf("non-canonical Turbo archive file %q", header.Name)
			}
		default:
			return nil, fmt.Errorf("unexpected Turbo archive member type for %q", header.Name)
		}
		contents, err := io.ReadAll(io.LimitReader(reader, maximumExpansion-total+1))
		if err != nil {
			return nil, err
		}
		total += int64(len(contents))
		if total > maximumExpansion {
			return nil, errors.New("Turbo archive expands beyond the QA limit")
		}
		result[clean] = string(contents)
	}
	return result, nil
}

type restoreHandler struct {
	key      string
	artifact []byte
	gets     atomic.Int64
	invalid  atomic.Int64
}

func (handler *restoreHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	switch {
	case request.URL.Path == "/v8/artifacts/status":
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"status":"enabled"}`))
	case request.URL.Path == "/v8/artifacts/events":
		_, _ = io.Copy(io.Discard, io.LimitReader(request.Body, 1<<20))
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"accepted":true}`))
	case request.URL.Path == "/v8/artifacts/"+handler.key &&
		(request.Method == http.MethodGet || request.Method == http.MethodHead):
		writer.Header().Set("Content-Type", "application/octet-stream")
		writer.Header().Set("Content-Length", fmt.Sprint(len(handler.artifact)))
		writer.Header().Set("x-artifact-duration", "500")
		if request.Method == http.MethodGet {
			handler.gets.Add(1)
			_, _ = writer.Write(handler.artifact)
		}
	default:
		handler.invalid.Add(1)
		writer.WriteHeader(http.StatusNotFound)
	}
}

func verifyRealTurboRestore(ctx context.Context, turbo, key string, artifact []byte) (string, error) {
	fixture, err := os.MkdirTemp("", "layercache-turbo-kvm-restore.")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(fixture)
	if err := writeFixture(fixture, 64<<20); err != nil {
		return "", err
	}
	fakeBin := filepath.Join(fixture, "fake-bin")
	if err := os.Mkdir(fakeBin, 0o755); err != nil {
		return "", err
	}
	marker := filepath.Join(fixture, "npm-was-invoked")
	fakeNPM := "#!/bin/sh\nprintf invoked >\"$QA_NPM_MARKER\"\nexit 86\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "npm"), []byte(fakeNPM), 0o755); err != nil {
		return "", err
	}
	handler := &restoreHandler{key: key, artifact: artifact}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 2 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	restoreCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	command := exec.CommandContext(restoreCtx, turbo,
		"run", target, "--summarize", "--cache=remote:r")
	command.Dir = fixture
	command.Env = turboEnvironment(filepath.Join(fixture, "home"), []string{
		"PATH=" + fakeBin + ":/usr/bin:/bin",
		"QA_NPM_MARKER=" + marker,
		"TURBO_API=http://" + listener.Addr().String(),
		"TURBO_TOKEN=public-build-guest",
		"TURBO_TEAM=layercache-public-build",
	})
	combined, commandErr := command.CombinedOutput()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
	shutdownErr := server.Shutdown(shutdownCtx)
	shutdownCancel()
	serveErr := <-done
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	if err := errors.Join(commandErr, shutdownErr, serveErr); err != nil {
		return "", fmt.Errorf("restore through real Turbo 2.10.12: %w: %s", err, string(combined))
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		return "", errors.New("real Turbo restore invoked the deliberately failing npm task")
	}
	restored, err := os.ReadFile(filepath.Join(fixture, "packages", "app", "dist", "output.txt"))
	if err != nil || string(restored) != expectedOutput {
		return "", fmt.Errorf("real Turbo restored output = %q: %w", restored, err)
	}
	log := string(combined)
	if handler.gets.Load() == 0 || handler.invalid.Load() != 0 ||
		!strings.Contains(strings.ToLower(log), "cache hit") {
		return "", fmt.Errorf("real Turbo did not prove an exact remote hit: %s", log)
	}
	return log, nil
}

func turboEnvironment(home string, additions []string) []string {
	blocked := map[string]struct{}{
		"HOME": {}, "PATH": {}, "QA_NPM_MARKER": {},
		"TURBO_API": {}, "TURBO_TOKEN": {}, "TURBO_TEAM": {},
		"TURBO_TELEMETRY_DISABLED": {},
	}
	overrides := make(map[string]struct{}, len(additions))
	for _, entry := range additions {
		name, _, _ := strings.Cut(entry, "=")
		overrides[name] = struct{}{}
	}
	environment := make([]string, 0, len(os.Environ())+len(additions)+3)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if _, omit := blocked[name]; !omit {
			environment = append(environment, entry)
		}
	}
	defaults := []string{
		"HOME=" + home,
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"TURBO_TELEMETRY_DISABLED=1",
	}
	for _, entry := range defaults {
		name, _, _ := strings.Cut(entry, "=")
		if _, replaced := overrides[name]; !replaced {
			environment = append(environment, entry)
		}
	}
	return append(environment, additions...)
}
