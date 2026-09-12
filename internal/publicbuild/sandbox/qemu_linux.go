//go:build linux

package sandbox

import (
	"bufio"
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
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/layercache/layercache/internal/publicbuild"
	"golang.org/x/sys/unix"
)

const (
	minimumSourceImageBytes = 64 << 20
	maximumProtocolLine     = 64 << 10
	maximumLogBytes         = 8 << 20
	qemuConsoleBytes        = 1 << 20
	cpuPeriodMicros         = 100_000
	qemuMemoryOverhead      = 512 << 20
	scratchMetadataBytes    = 256 << 20
)

var (
	ansiEscape     = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07]*(?:\x07|\x1b\\))`)
	unsafeLogValue = regexp.MustCompile(`(?i)(authorization|bearer|cookie|password|secret|token)([=:][[:space:]]*|[[:space:]]+)([^[:space:]]+)`)
)

type PreflightReport struct {
	WorkerID           string               `json:"workerId"`
	Platform           publicbuild.Platform `json:"platform"`
	QEMUPath           string               `json:"qemuPath"`
	KernelSHA256       string               `json:"kernelSha256"`
	RootFSSHA256       string               `json:"rootfsSha256"`
	ContractSHA256     string               `json:"contractSha256"`
	BuilderImageDigest string               `json:"builderImageDigest"`
	Protocol           string               `json:"protocol"`
	CgroupRoot         string               `json:"cgroupRoot"`
	SandboxUID         uint32               `json:"sandboxUid"`
	SandboxGID         uint32               `json:"sandboxGid"`
	Network            string               `json:"network"`
	SourceTransport    string               `json:"sourceTransport"`
	OutputTransport    string               `json:"outputTransport"`
	DependencyMode     string               `json:"dependencyMode"`
	MaxScratchBytes    int64                `json:"maxScratchBytes"`
}

func (worker *QEMUWorker) Preflight(ctx context.Context) (PreflightReport, error) {
	if worker == nil {
		return PreflightReport{}, errors.New("Public Build worker is nil")
	}
	config := worker.config
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		return PreflightReport{}, fmt.Errorf("QEMU Public Build workers do not support host architecture %s", runtime.GOARCH)
	}
	wantPlatform := publicbuild.Platform("linux/" + runtime.GOARCH)
	if worker.contract.Platform != wantPlatform {
		return PreflightReport{}, fmt.Errorf("guest platform %s does not match native KVM host %s", worker.contract.Platform, wantPlatform)
	}
	if os.Geteuid() != 0 {
		return PreflightReport{}, errors.New("QEMU Public Build worker must start as root so QEMU can enter its dedicated UID before boot")
	}
	if config.SandboxUID == 0 || config.SandboxGID == 0 {
		return PreflightReport{}, errors.New("dedicated non-root QEMU sandbox UID and GID are required")
	}
	if config.QEMUPath == "" || config.QEMUImagePath == "" || config.Mke2fsPath == "" || config.DebugFSPath == "" {
		return PreflightReport{}, errors.New("qemu-system, qemu-img, mke2fs, and debugfs paths are required")
	}
	if config.KernelPath == "" || config.RootFSPath == "" {
		return PreflightReport{}, errors.New("immutable kernel and rootfs paths are required")
	}
	if config.WorkRoot == "" || config.CgroupRoot == "" {
		return PreflightReport{}, errors.New("worker scratch and delegated cgroup roots are required")
	}
	if config.MaxScratchBytes <= 0 {
		return PreflightReport{}, errors.New("positive aggregate Public Build scratch limit is required")
	}
	for label, path := range map[string]string{
		"QEMU": config.QEMUPath, "qemu-img": config.QEMUImagePath, "mke2fs": config.Mke2fsPath,
		"debugfs": config.DebugFSPath,
	} {
		if err := verifyTrustedExecutable(path, label); err != nil {
			return PreflightReport{}, err
		}
	}
	if err := verifyImmutableAsset(config.KernelPath, config.KernelSHA256, "kernel"); err != nil {
		return PreflightReport{}, err
	}
	if err := verifyImmutableAsset(config.RootFSPath, config.RootFSSHA256, "rootfs"); err != nil {
		return PreflightReport{}, err
	}
	if err := verifyImmutableAsset(config.ContractPath, config.ContractSHA256, "guest contract"); err != nil {
		return PreflightReport{}, err
	}
	if err := verifyKVM(config.SandboxGID); err != nil {
		return PreflightReport{}, err
	}
	if err := verifyQEMUFeatures(ctx, config.QEMUPath, worker.contract.Platform); err != nil {
		return PreflightReport{}, err
	}
	if err := verifyRawRootFS(ctx, config.QEMUImagePath, config.RootFSPath); err != nil {
		return PreflightReport{}, err
	}
	if err := verifyGuestImage(ctx, config.DebugFSPath, config.RootFSPath, config.ContractPath, worker.contract); err != nil {
		return PreflightReport{}, err
	}
	if err := verifyWorkRoot(config.WorkRoot, config.SandboxGID); err != nil {
		return PreflightReport{}, err
	}
	if err := verifyCgroupDelegation(config.CgroupRoot); err != nil {
		return PreflightReport{}, err
	}
	worker.startupCleanupOnce.Do(func() {
		worker.startupLock, worker.startupCleanupError = acquireWorkerLock(config.WorkRoot, workerDirectoryIdentity(config.WorkerID))
		if worker.startupCleanupError != nil {
			return
		}
		worker.startupCleanupError = errors.Join(
			reclaimStaleScratch(config.WorkRoot, worker.scratchPrefix, config.SandboxGID),
			reclaimStaleCgroups(config.CgroupRoot, worker.cgroupPrefix),
		)
	})
	if worker.startupCleanupError != nil {
		return PreflightReport{}, fmt.Errorf("reclaim stale Public Build state: %w", worker.startupCleanupError)
	}
	return PreflightReport{
		WorkerID: config.WorkerID, Platform: worker.contract.Platform,
		QEMUPath: config.QEMUPath, KernelSHA256: config.KernelSHA256,
		RootFSSHA256: config.RootFSSHA256, ContractSHA256: config.ContractSHA256,
		BuilderImageDigest: worker.builderImageDigest,
		Protocol:           worker.contract.Protocol, CgroupRoot: config.CgroupRoot,
		SandboxUID: config.SandboxUID, SandboxGID: config.SandboxGID,
		Network: "none", SourceTransport: "immutable ext4 block device, read-only",
		OutputTransport: "bounded virtio-serial stream to trusted host collector",
		DependencyMode:  "offline-only; dependencies must be vendored in source or pinned in the immutable image",
		MaxScratchBytes: config.MaxScratchBytes,
	}, nil
}

func (worker *QEMUWorker) Execute(ctx context.Context, build publicbuild.Build, logs publicbuild.LogSink) (publicbuild.Publication, error) {
	if _, err := worker.Preflight(ctx); err != nil {
		return publicbuild.Publication{}, err
	}
	if build.ID == "" || build.Request.Platform != worker.contract.Platform {
		return publicbuild.Publication{}, errors.New("leased Public Build does not match worker platform")
	}
	if build.Request.Resources.CPUMillis <= 0 || build.Request.Resources.MemoryBytes <= 0 ||
		build.Request.Resources.DiskBytes <= 0 || build.Request.Resources.Timeout <= 0 {
		return publicbuild.Publication{}, errors.New("leased Public Build has invalid resource limits")
	}
	recipe, err := worker.contract.Recipe(build)
	if err != nil {
		return publicbuild.Publication{}, err
	}
	if _, _, err := worker.Collected(build.ID); err == nil {
		return publicbuild.Publication{}, errors.New("leased Public Build already has an uncollected result")
	}
	scratchBound, err := worker.scratchBound(build, recipe)
	if err != nil {
		return publicbuild.Publication{}, err
	}
	if scratchBound > worker.config.MaxScratchBytes {
		return publicbuild.Publication{}, fmt.Errorf(
			"Public Build worst-case scratch use %d exceeds worker limit %d",
			scratchBound, worker.config.MaxScratchBytes,
		)
	}
	if err := requireScratchSpace(worker.config.WorkRoot, scratchBound); err != nil {
		return publicbuild.Publication{}, err
	}
	executionContext, cancel := context.WithTimeout(ctx, build.Request.Resources.Timeout)
	defer cancel()
	workDirectory, err := os.MkdirTemp(worker.config.WorkRoot, worker.scratchPrefix)
	if err != nil {
		return publicbuild.Publication{}, fmt.Errorf("create Public Build scratch directory: %w", err)
	}
	cleanup := func() error { return os.RemoveAll(workDirectory) }
	keepResult := false
	defer func() {
		if !keepResult {
			_ = cleanup()
		}
	}()
	if err := os.Chmod(workDirectory, 0o710); err != nil {
		return publicbuild.Publication{}, fmt.Errorf("protect Public Build scratch directory: %w", err)
	}
	if err := os.Chown(workDirectory, 0, int(worker.config.SandboxGID)); err != nil {
		return publicbuild.Publication{}, fmt.Errorf("assign Public Build scratch directory: %w", err)
	}
	sourceDirectory := filepath.Join(workDirectory, "source")
	sourceLimit := min(worker.config.SourceArchiveBytes, build.Request.Resources.DiskBytes)
	if err := worker.config.SourceFetcher.Fetch(
		executionContext, build.Request.Repository, build.Request.Commit, sourceDirectory, sourceLimit,
	); err != nil {
		return publicbuild.Publication{}, err
	}
	paths, err := worker.prepareDisks(executionContext, build, workDirectory, sourceDirectory, sourceLimit)
	if err != nil {
		return publicbuild.Publication{}, err
	}
	if err := os.RemoveAll(sourceDirectory); err != nil {
		return publicbuild.Publication{}, fmt.Errorf("remove extracted source after sealing its image: %w", err)
	}
	controlDirectory := filepath.Join(workDirectory, "run")
	if err := os.Mkdir(controlDirectory, 0o700); err != nil {
		return publicbuild.Publication{}, fmt.Errorf("create QEMU control directory: %w", err)
	}
	if err := os.Chown(controlDirectory, int(worker.config.SandboxUID), int(worker.config.SandboxGID)); err != nil {
		return publicbuild.Publication{}, fmt.Errorf("assign QEMU control directory: %w", err)
	}
	paths.controlSocket = filepath.Join(controlDirectory, "control.sock")
	cgroup, err := createBuildCgroup(worker.config.CgroupRoot, worker.cgroupPrefix, build, worker.contract.MaxProcesses)
	if err != nil {
		return publicbuild.Publication{}, err
	}
	defer cgroup.Close()
	started := time.Now()
	output, err := worker.runQEMU(executionContext, build, recipe, paths, cgroup, logs)
	if err != nil {
		return publicbuild.Publication{}, err
	}
	if err := validateNativeKey(build.Request, worker.contract, output.NativeKey); err != nil {
		return publicbuild.Publication{}, err
	}
	duration := time.Since(started)
	result := collectedResult{output: output, cleanup: cleanup, duration: duration}
	if err := worker.remember(build.ID, result); err != nil {
		return publicbuild.Publication{}, err
	}
	keepResult = true
	return publicbuild.Publication{
		Outputs: []publicbuild.OutputDescriptor{{
			Name: output.NativeKey, Digest: output.Digest,
			SizeBytes: output.SizeBytes, MediaType: output.MediaType,
		}},
		ProducerDuration: duration,
	}, nil
}

type qemuPaths struct {
	rootOverlay   string
	sourceImage   string
	workImage     string
	output        string
	controlSocket string
}

func (worker *QEMUWorker) prepareDisks(
	ctx context.Context,
	build publicbuild.Build,
	workDirectory string,
	sourceDirectory string,
	sourceLimit int64,
) (qemuPaths, error) {
	paths := qemuPaths{
		rootOverlay: filepath.Join(workDirectory, "root.qcow2"),
		sourceImage: filepath.Join(workDirectory, "source.raw"),
		workImage:   filepath.Join(workDirectory, "work.qcow2"),
		output:      filepath.Join(workDirectory, "output.bin"),
	}
	if err := runTrustedCommand(ctx, worker.config.QEMUImagePath, "create", "-q", "-f", "qcow2", "-F", "raw", "-b", worker.config.RootFSPath, paths.rootOverlay); err != nil {
		return qemuPaths{}, fmt.Errorf("create ephemeral rootfs overlay: %w", err)
	}
	if err := runTrustedCommand(ctx, worker.config.QEMUImagePath, "create", "-q", "-f", "qcow2", paths.workImage, strconv.FormatInt(build.Request.Resources.DiskBytes, 10)); err != nil {
		return qemuPaths{}, fmt.Errorf("create bounded ephemeral work disk: %w", err)
	}
	sourceSize, err := sourceImageSize(sourceDirectory)
	if err != nil {
		return qemuPaths{}, err
	}
	if sourceSize > sourceLimit {
		return qemuPaths{}, fmt.Errorf("immutable source filesystem needs %d bytes, exceeding its %d byte limit", sourceSize, sourceLimit)
	}
	sourceFile, err := os.OpenFile(paths.sourceImage, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return qemuPaths{}, fmt.Errorf("create immutable source image: %w", err)
	}
	truncateErr := sourceFile.Truncate(sourceSize)
	closeErr := sourceFile.Close()
	if truncateErr != nil {
		return qemuPaths{}, fmt.Errorf("size immutable source image: %w", truncateErr)
	}
	if closeErr != nil {
		return qemuPaths{}, fmt.Errorf("close immutable source image: %w", closeErr)
	}
	if err := runTrustedCommand(ctx, worker.config.Mke2fsPath, "-q", "-t", "ext4", "-F", "-m", "0", "-O", "^has_journal", "-d", sourceDirectory, paths.sourceImage); err != nil {
		return qemuPaths{}, fmt.Errorf("format immutable source image: %w", err)
	}
	for _, path := range []string{paths.rootOverlay, paths.workImage} {
		if err := os.Chown(path, int(worker.config.SandboxUID), int(worker.config.SandboxGID)); err != nil {
			return qemuPaths{}, fmt.Errorf("assign QEMU disk %q: %w", filepath.Base(path), err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return qemuPaths{}, fmt.Errorf("protect QEMU disk %q: %w", filepath.Base(path), err)
		}
	}
	// The source device is read-only in both layers: QEMU rejects guest writes,
	// and its unprivileged host process cannot reopen the sealed image writable.
	if err := os.Chown(paths.sourceImage, 0, int(worker.config.SandboxGID)); err != nil {
		return qemuPaths{}, fmt.Errorf("assign immutable source image: %w", err)
	}
	if err := os.Chmod(paths.sourceImage, 0o440); err != nil {
		return qemuPaths{}, fmt.Errorf("protect immutable source image: %w", err)
	}
	return paths, nil
}

func sourceImageSize(sourceDirectory string) (int64, error) {
	var bytesUsed int64
	var entries int64
	err := filepath.WalkDir(sourceDirectory, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		entries++
		if entry.Type().IsRegular() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			bytesUsed += info.Size()
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("measure immutable source tree: %w", err)
	}
	requested := bytesUsed + entries*16_384 + 16<<20
	if requested < minimumSourceImageBytes {
		requested = minimumSourceImageBytes
	}
	const block = int64(4 << 20)
	return ((requested + block - 1) / block) * block, nil
}

func (worker *QEMUWorker) scratchBound(build publicbuild.Build, recipe RecipeContract) (int64, error) {
	rootFS, err := os.Stat(worker.config.RootFSPath)
	if err != nil {
		return 0, fmt.Errorf("inspect immutable rootfs for scratch accounting: %w", err)
	}
	sourceLimit := min(worker.config.SourceArchiveBytes, build.Request.Resources.DiskBytes)
	outputLimit := min(recipe.MaxBytes, build.Request.Resources.DiskBytes)
	values := []int64{
		rootFS.Size(), build.Request.Resources.DiskBytes,
		sourceLimit, sourceLimit, outputLimit, scratchMetadataBytes,
	}
	var total int64
	for _, value := range values {
		if value < 0 || total > int64(^uint64(0)>>1)-value {
			return 0, errors.New("Public Build scratch bound overflows int64")
		}
		total += value
	}
	return total, nil
}

func requireScratchSpace(root string, required int64) error {
	var status unix.Statfs_t
	if err := unix.Statfs(root, &status); err != nil {
		return fmt.Errorf("inspect Public Build scratch filesystem: %w", err)
	}
	if status.Bsize <= 0 || status.Bavail > ^uint64(0)/uint64(status.Bsize) {
		return errors.New("Public Build scratch filesystem reported invalid free space")
	}
	available := status.Bavail * uint64(status.Bsize)
	if uint64(required) > available {
		return fmt.Errorf("Public Build scratch needs %d bytes but filesystem has %d available", required, available)
	}
	return nil
}

type protocolEvent struct {
	Type      string `json:"type"`
	Protocol  string `json:"protocol,omitempty"`
	Message   string `json:"message,omitempty"`
	NativeKey string `json:"nativeKey,omitempty"`
	MediaType string `json:"mediaType,omitempty"`
	SizeBytes int64  `json:"sizeBytes,omitempty"`
}

type executeRequest struct {
	Type         string                      `json:"type"`
	Protocol     string                      `json:"protocol"`
	BuildID      string                      `json:"buildId"`
	Repository   string                      `json:"repository"`
	Commit       string                      `json:"commit"`
	Integration  publicbuild.Integration     `json:"integration"`
	Target       string                      `json:"target"`
	RecipeDigest string                      `json:"recipeDigest"`
	Inputs       []publicbuild.DeclaredInput `json:"inputs,omitempty"`
	Resources    executeRequestResources     `json:"resources"`
}

type executeRequestResources struct {
	CPUMillis    int64 `json:"cpuMillis"`
	MemoryBytes  int64 `json:"memoryBytes"`
	DiskBytes    int64 `json:"diskBytes"`
	TimeoutMS    int64 `json:"timeoutMilliseconds"`
	MaxProcesses int64 `json:"maxProcesses"`
}

func (worker *QEMUWorker) runQEMU(
	ctx context.Context,
	build publicbuild.Build,
	recipe RecipeContract,
	paths qemuPaths,
	cgroup *buildCgroup,
	logs publicbuild.LogSink,
) (CollectedOutput, error) {
	arguments, err := worker.qemuArguments(build, paths)
	if err != nil {
		return CollectedOutput{}, err
	}
	commandContext, cancel := context.WithCancel(ctx)
	defer cancel()
	command := exec.CommandContext(commandContext, worker.config.QEMUPath, arguments...)
	command.Stdin = nil
	console := &limitedWriter{remaining: qemuConsoleBytes}
	command.Stdout = console
	command.Stderr = console
	command.Env = trustedEnvironment(filepath.Dir(paths.controlSocket))
	command.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{
			Uid: worker.config.SandboxUID, Gid: worker.config.SandboxGID,
			Groups: []uint32{worker.config.SandboxGID},
		},
		Setsid: true, Pdeathsig: syscall.SIGKILL,
		UseCgroupFD: true, CgroupFD: cgroup.fd,
	}
	if err := command.Start(); err != nil {
		return CollectedOutput{}, fmt.Errorf("start isolated QEMU guest: %w", err)
	}
	done := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = command.Wait()
		close(done)
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			if command.Process != nil {
				_ = command.Process.Kill()
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
			}
		}
	}()
	connection, err := connectGuest(ctx, paths.controlSocket, worker.config.ConnectTimeout, done, func() error { return waitErr })
	if err != nil {
		return CollectedOutput{}, fmt.Errorf("connect immutable guest agent: %w; QEMU console: %s", err, console.String())
	}
	defer connection.Close()
	reader := bufio.NewReaderSize(connection, maximumProtocolLine)
	hello, err := readProtocolEvent(reader)
	if err != nil {
		cancel()
		return CollectedOutput{}, fmt.Errorf("read guest handshake: %w; QEMU console: %s", err, console.String())
	}
	if hello.Type != "hello" || hello.Protocol != worker.contract.Protocol {
		cancel()
		return CollectedOutput{}, errors.New("immutable guest does not support the pinned Public Build protocol")
	}
	request := executeRequest{
		Type: "execute", Protocol: worker.contract.Protocol, BuildID: build.ID,
		Repository: build.Request.Repository, Commit: build.Request.Commit,
		Integration: build.Request.Integration, Target: build.Request.Target,
		RecipeDigest: build.Request.RecipeDigest,
		Inputs:       append([]publicbuild.DeclaredInput(nil), build.Request.Inputs...),
		Resources: executeRequestResources{
			CPUMillis:    build.Request.Resources.CPUMillis,
			MemoryBytes:  build.Request.Resources.MemoryBytes,
			DiskBytes:    build.Request.Resources.DiskBytes,
			TimeoutMS:    build.Request.Resources.Timeout.Milliseconds(),
			MaxProcesses: worker.contract.MaxProcesses,
		},
	}
	if err := writeProtocolMessage(connection, request); err != nil {
		cancel()
		return CollectedOutput{}, fmt.Errorf("send Public Build request to guest: %w", err)
	}
	var output CollectedOutput
	logBytes := 0
	for {
		event, err := readProtocolEvent(reader)
		if err != nil {
			cancel()
			return CollectedOutput{}, fmt.Errorf("read Public Build guest event: %w; QEMU console: %s", err, console.String())
		}
		switch event.Type {
		case "log":
			message := sanitizeGuestLog(event.Message)
			logBytes += len(message)
			if logBytes > maximumLogBytes {
				cancel()
				return CollectedOutput{}, fmt.Errorf("guest logs exceed %d bytes", maximumLogBytes)
			}
			if logs != nil && message != "" {
				if err := logs(ctx, message); err != nil {
					cancel()
					return CollectedOutput{}, fmt.Errorf("append Public Build log: %w", err)
				}
			}
		case "output":
			if output.Path != "" {
				cancel()
				return CollectedOutput{}, errors.New("guest emitted more than one output")
			}
			output, err = collectGuestOutput(reader, paths.output, event, recipe, build.Request.Resources.DiskBytes)
			if err != nil {
				cancel()
				return CollectedOutput{}, err
			}
		case "complete":
			if output.Path == "" {
				cancel()
				return CollectedOutput{}, errors.New("guest completed without its declared output")
			}
			if err := waitForCleanShutdown(done, func() error { return waitErr }, worker.config.ShutdownTimeout); err != nil {
				cancel()
				return CollectedOutput{}, fmt.Errorf("guest did not shut down cleanly after completion: %w", err)
			}
			return output, nil
		case "failed":
			cancel()
			return CollectedOutput{}, fmt.Errorf("guest build failed: %s", sanitizeGuestLog(event.Message))
		default:
			cancel()
			return CollectedOutput{}, fmt.Errorf("guest emitted unknown protocol event %q", event.Type)
		}
	}
}

func (worker *QEMUWorker) qemuArguments(build publicbuild.Build, paths qemuPaths) ([]string, error) {
	if strings.ContainsAny(paths.controlSocket, ",\x00\r\n") {
		return nil, errors.New("QEMU control path contains an unsupported character")
	}
	virtualCPUs := (build.Request.Resources.CPUMillis + 999) / 1000
	if virtualCPUs < 1 {
		virtualCPUs = 1
	}
	machine := "microvm"
	serial := "ttyS0"
	if worker.contract.Platform == publicbuild.PlatformLinuxARM64 {
		machine = "virt,gic-version=host"
		serial = "ttyAMA0"
	}
	rootFile, _ := json.Marshal(map[string]any{"driver": "file", "filename": paths.rootOverlay, "node-name": "root-file"})
	rootBlock, _ := json.Marshal(map[string]any{"driver": "qcow2", "file": "root-file", "node-name": "root"})
	sourceFile, _ := json.Marshal(map[string]any{"driver": "file", "filename": paths.sourceImage, "node-name": "source", "read-only": true})
	workFile, _ := json.Marshal(map[string]any{"driver": "file", "filename": paths.workImage, "node-name": "work-file"})
	workBlock, _ := json.Marshal(map[string]any{"driver": "qcow2", "file": "work-file", "node-name": "work"})
	kernelArguments := strings.Join([]string{
		"console=" + serial, "panic=1", "oops=panic", "reboot=t", "root=/dev/vda", "rw",
		"rootfstype=ext4",
		"init=" + worker.contract.Agent,
		"layercache.protocol=" + worker.contract.Protocol,
		"layercache.control=" + worker.contract.ControlPort,
		"layercache.source=/dev/vdb", "layercache.work=/dev/vdc", "random.trust_cpu=off",
	}, " ")
	return []string{
		"-machine", machine,
		"-accel", "kvm",
		"-cpu", "host",
		"-smp", strconv.FormatInt(virtualCPUs, 10),
		"-m", strconv.FormatInt(build.Request.Resources.MemoryBytes, 10) + "b",
		"-kernel", worker.config.KernelPath,
		"-append", kernelArguments,
		"-nodefaults", "-no-user-config", "-no-reboot",
		"-display", "none", "-monitor", "none", "-serial", "stdio",
		"-nic", "none",
		"-sandbox", "on,obsolete=deny,elevateprivileges=deny,spawn=deny,resourcecontrol=deny",
		"-overcommit", "mem-lock=off",
		"-blockdev", string(rootFile), "-blockdev", string(rootBlock),
		"-device", "virtio-blk-device,drive=root",
		"-blockdev", string(sourceFile), "-device", "virtio-blk-device,drive=source",
		"-blockdev", string(workFile), "-blockdev", string(workBlock),
		"-device", "virtio-blk-device,drive=work",
		"-chardev", "socket,id=control,path=" + paths.controlSocket + ",server=on,wait=off",
		"-device", "virtio-serial-device",
		"-device", "virtserialport,chardev=control,name=" + worker.contract.ControlPort,
		"-object", "rng-random,id=rng0,filename=/dev/urandom",
		"-device", "virtio-rng-device,rng=rng0,max-bytes=1024,period=1000",
	}, nil
}

func connectGuest(
	ctx context.Context,
	socketPath string,
	timeout time.Duration,
	done <-chan struct{},
	waitError func() error,
) (net.Conn, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		connection, err := net.DialTimeout("unix", socketPath, 100*time.Millisecond)
		if err == nil {
			return connection, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-done:
			err := waitError()
			if err == nil {
				return nil, errors.New("QEMU exited before the guest protocol became available")
			}
			return nil, err
		case <-deadline.C:
			return nil, fmt.Errorf("control socket was unavailable after %s", timeout)
		case <-ticker.C:
		}
	}
}

func readProtocolEvent(reader *bufio.Reader) (protocolEvent, error) {
	line, err := reader.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) || len(line) > maximumProtocolLine {
		return protocolEvent{}, fmt.Errorf("guest protocol line exceeds %d bytes", maximumProtocolLine)
	}
	if err != nil {
		return protocolEvent{}, err
	}
	var event protocolEvent
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&event); err != nil {
		return protocolEvent{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return protocolEvent{}, errors.New("guest protocol event contains trailing JSON")
	}
	if event.Type == "" {
		return protocolEvent{}, errors.New("guest protocol event type is empty")
	}
	return event, nil
}

func writeProtocolMessage(writer io.Writer, message any) error {
	encoded, err := json.Marshal(message)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	for len(encoded) > 0 {
		written, writeErr := writer.Write(encoded)
		if writeErr != nil {
			return writeErr
		}
		if written <= 0 {
			return io.ErrShortWrite
		}
		encoded = encoded[written:]
	}
	return nil
}

func collectGuestOutput(
	reader io.Reader,
	outputPath string,
	event protocolEvent,
	recipe RecipeContract,
	diskLimit int64,
) (CollectedOutput, error) {
	if event.NativeKey == "" || len(event.NativeKey) > 1024 || strings.ContainsAny(event.NativeKey, "\x00\r\n") {
		return CollectedOutput{}, errors.New("guest output has an invalid native cache key")
	}
	format := recipe.ResultFormat
	if format == "" {
		format = ResultFormatNativeBlob
	}
	wantTransportMediaType := recipe.MediaType
	if format == ResultFormatOCIImageLayout {
		wantTransportMediaType = OCIImageLayoutTarMediaType
	}
	if event.MediaType != wantTransportMediaType {
		return CollectedOutput{}, errors.New("guest output media type does not match the maintained recipe")
	}
	maximum := min(recipe.MaxBytes, diskLimit)
	if event.SizeBytes <= 0 || event.SizeBytes > maximum {
		return CollectedOutput{}, fmt.Errorf("guest output exceeds its %d byte declaration", maximum)
	}
	file, err := os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return CollectedOutput{}, fmt.Errorf("stage trusted Public Build collection: %w", err)
	}
	hasher := sha256.New()
	copyErr := copyExactly(io.MultiWriter(file, hasher), reader, event.SizeBytes)
	syncErr := file.Sync()
	closeErr := file.Close()
	if copyErr != nil {
		return CollectedOutput{}, fmt.Errorf("collect declared Public Build output: %w", copyErr)
	}
	if syncErr != nil {
		return CollectedOutput{}, fmt.Errorf("sync declared Public Build output: %w", syncErr)
	}
	if closeErr != nil {
		return CollectedOutput{}, fmt.Errorf("close declared Public Build output: %w", closeErr)
	}
	return CollectedOutput{
		NativeKey: event.NativeKey, MediaType: event.MediaType,
		ResultFormat: format, ResultMediaType: recipe.MediaType,
		Digest:    "sha256:" + hex.EncodeToString(hasher.Sum(nil)),
		SizeBytes: event.SizeBytes, Path: outputPath,
	}, nil
}

func waitForCleanShutdown(done <-chan struct{}, waitError func() error, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return waitError()
	case <-timer.C:
		return errors.New("shutdown deadline exceeded")
	}
}

func sanitizeGuestLog(message string) string {
	message = ansiEscape.ReplaceAllString(message, "")
	message = unsafeLogValue.ReplaceAllString(message, "$1$2[REDACTED]")
	message = strings.Map(func(character rune) rune {
		if character == '\n' || character == '\t' || character >= 0x20 {
			return character
		}
		return -1
	}, message)
	message = strings.TrimSpace(message)
	if len(message) > 4096 {
		message = message[:4096]
	}
	return message
}

func verifyTrustedExecutable(path, label string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect %s executable: %w", label, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return fmt.Errorf("%s path must be a regular executable", label)
	}
	if stat.Uid != 0 || info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s executable must be root-owned and not group- or other-writable", label)
	}
	return verifyTrustedParents(path, label+" executable")
}

func verifyImmutableAsset(path, expectedDigest, label string) error {
	if !digestPattern.MatchString(expectedDigest) {
		return fmt.Errorf("%s SHA-256 must be a lowercase sha256 digest", label)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect immutable %s: %w", label, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() {
		return fmt.Errorf("immutable %s must be a regular file", label)
	}
	if stat.Uid != 0 || info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("immutable %s must be root-owned and not group- or other-writable", label)
	}
	if info.Mode().Perm()&0o004 == 0 {
		return fmt.Errorf("immutable %s must be readable by the dedicated QEMU account", label)
	}
	if err := verifyTrustedParents(path, "immutable "+label); err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open immutable %s: %w", label, err)
	}
	hasher := sha256.New()
	_, copyErr := io.Copy(hasher, file)
	closeErr := file.Close()
	if copyErr != nil {
		return fmt.Errorf("hash immutable %s: %w", label, copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close immutable %s: %w", label, closeErr)
	}
	if "sha256:"+hex.EncodeToString(hasher.Sum(nil)) != expectedDigest {
		return fmt.Errorf("immutable %s SHA-256 does not match the configured digest", label)
	}
	return nil
}

func verifyTrustedParents(filePath, label string) error {
	absolute, err := filepath.Abs(filePath)
	if err != nil {
		return fmt.Errorf("resolve %s path: %w", label, err)
	}
	for parent := filepath.Dir(absolute); ; parent = filepath.Dir(parent) {
		info, err := os.Lstat(parent)
		if err != nil {
			return fmt.Errorf("inspect %s parent %q: %w", label, parent, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("%s parent %q is not a real directory", label, parent)
		}
		if info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("%s parent %q is group- or other-writable", label, parent)
		}
		if info.Mode().Perm()&0o001 == 0 {
			return fmt.Errorf("%s parent %q is not traversable by the dedicated QEMU account", label, parent)
		}
		if parent == filepath.Dir(parent) {
			break
		}
	}
	return nil
}

func verifyKVM(sandboxGID uint32) error {
	file, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open /dev/kvm: %w", err)
	}
	info, statErr := file.Stat()
	closeErr := file.Close()
	if statErr != nil {
		return fmt.Errorf("inspect /dev/kvm: %w", statErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close /dev/kvm: %w", closeErr)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode()&os.ModeDevice == 0 {
		return errors.New("/dev/kvm is not a device")
	}
	groupAccess := stat.Gid == sandboxGID && info.Mode().Perm()&0o060 == 0o060
	otherAccess := info.Mode().Perm()&0o006 == 0o006
	if !groupAccess && !otherAccess {
		return fmt.Errorf("dedicated QEMU GID %d cannot read and write /dev/kvm", sandboxGID)
	}
	return nil
}

func verifyQEMUFeatures(ctx context.Context, qemuPath string, platform publicbuild.Platform) error {
	machine := "microvm"
	if platform == publicbuild.PlatformLinuxARM64 {
		machine = "virt"
	}
	machineOutput, err := trustedCommandOutput(ctx, qemuPath, "-machine", "help")
	if err != nil {
		return fmt.Errorf("inspect QEMU machines: %w", err)
	}
	if !bytes.Contains(machineOutput, []byte(machine)) {
		return fmt.Errorf("QEMU does not provide required %s machine support", machine)
	}
	deviceOutput, err := trustedCommandOutput(ctx, qemuPath, "-device", "help")
	if err != nil {
		return fmt.Errorf("inspect QEMU devices: %w", err)
	}
	for _, device := range []string{"virtio-blk-device", "virtio-serial-device", "virtserialport", "virtio-rng-device"} {
		if !bytes.Contains(deviceOutput, []byte(device)) {
			return fmt.Errorf("QEMU does not provide required %s support", device)
		}
	}
	return nil
}

func verifyRawRootFS(ctx context.Context, qemuImagePath, rootFSPath string) error {
	output, err := trustedCommandOutput(ctx, qemuImagePath, "info", "--output=json", "--force-share", rootFSPath)
	if err != nil {
		return fmt.Errorf("inspect immutable rootfs image: %w", err)
	}
	var information struct {
		Format      string `json:"format"`
		VirtualSize int64  `json:"virtual-size"`
	}
	if err := json.Unmarshal(output, &information); err != nil {
		return fmt.Errorf("decode immutable rootfs image information: %w", err)
	}
	if information.Format != "raw" || information.VirtualSize <= 0 {
		return errors.New("immutable rootfs must be a non-empty raw image")
	}
	return nil
}

func verifyGuestImage(ctx context.Context, debugFSPath, rootFSPath, contractPath string, contract GuestContract) error {
	embedded, err := trustedCommandOutput(ctx, debugFSPath, "-R", "cat "+embeddedContractPath, rootFSPath)
	if err != nil {
		return fmt.Errorf("read embedded guest contract: %w", err)
	}
	expected, err := os.ReadFile(contractPath)
	if err != nil {
		return fmt.Errorf("read pinned guest contract: %w", err)
	}
	if !bytes.Equal(embedded, expected) {
		return errors.New("guest rootfs embedded contract does not match the pinned contract")
	}
	for _, executablePath := range contract.RequiredExecutables() {
		stat, err := trustedCommandOutput(ctx, debugFSPath, "-R", "stat "+executablePath, rootFSPath)
		if err != nil {
			return fmt.Errorf("inspect immutable guest executable %s: %w", executablePath, err)
		}
		if !bytes.Contains(stat, []byte("Inode:")) || !bytes.Contains(stat, []byte("Type: regular")) {
			return fmt.Errorf("immutable guest rootfs does not contain regular executable %s", executablePath)
		}
		modeMatch := regexp.MustCompile(`Mode:\s+0*([0-7]{3,4})`).FindSubmatch(stat)
		if len(modeMatch) != 2 {
			return fmt.Errorf("immutable guest executable %s mode is unreadable", executablePath)
		}
		mode, err := strconv.ParseUint(string(modeMatch[1]), 8, 16)
		if err != nil || mode&0o111 == 0 {
			return fmt.Errorf("immutable guest executable %s is not executable", executablePath)
		}
	}
	return nil
}

func verifyWorkRoot(path string, sandboxGID uint32) error {
	if strings.ContainsAny(path, "\x00\r\n,") {
		return errors.New("worker scratch root contains an unsupported character")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect worker scratch root: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || stat.Uid != 0 ||
		stat.Gid != sandboxGID || info.Mode().Perm() != 0o710 {
		return fmt.Errorf("worker scratch root must be a root-owned 0710 directory in dedicated QEMU GID %d", sandboxGID)
	}
	return verifyTrustedParents(path, "worker scratch root")
}

func acquireWorkerLock(root, identity string) (*os.File, error) {
	name := ".layercache-worker-" + identity + ".lock"
	file, err := os.OpenFile(filepath.Join(root, name), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open worker identity lock: %w", err)
	}
	closeWithError := func(err error) (*os.File, error) {
		_ = file.Close()
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		return closeWithError(fmt.Errorf("inspect worker identity lock: %w", err))
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || stat.Uid != 0 || info.Mode().Perm()&0o077 != 0 {
		return closeWithError(errors.New("worker identity lock must be a root-owned private regular file"))
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return closeWithError(errors.New("another Public Build worker is active with the same worker ID"))
	}
	return file, nil
}

func reclaimStaleScratch(root, prefix string, sandboxGID uint32) error {
	return reclaimStaleScratchOwned(root, prefix, 0, sandboxGID)
}

func reclaimStaleScratchOwned(root, prefix string, ownerUID, sandboxGID uint32) error {
	return reclaimWorkerDirectories(root, prefix, func(rootHandle *os.Root, name string) error {
		info, err := os.Lstat(filepath.Join(root, name))
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || stat.Uid != ownerUID ||
			(stat.Gid != 0 && stat.Gid != sandboxGID) || info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("stale scratch directory %q has unsafe ownership or mode", name)
		}
		if err := rootHandle.RemoveAll(name); err != nil {
			return fmt.Errorf("remove stale scratch directory %q: %w", name, err)
		}
		return nil
	})
}

func reclaimStaleCgroups(root, prefix string) error {
	return reclaimWorkerDirectories(root, prefix, func(rootHandle *os.Root, name string) error {
		info, err := os.Lstat(filepath.Join(root, name))
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || stat.Uid != 0 || info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("stale cgroup %q has unsafe ownership or mode", name)
		}
		if err := rootHandle.Remove(name); err != nil {
			return fmt.Errorf("remove stale cgroup %q: %w", name, err)
		}
		return nil
	})
}

func reclaimWorkerDirectories(root, prefix string, remove func(*os.Root, string) error) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	rootHandle, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer rootHandle.Close()
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || !safeTemporarySuffix(strings.TrimPrefix(name, prefix)) {
			continue
		}
		if err := remove(rootHandle, name); err != nil {
			return err
		}
	}
	return nil
}

func safeTemporarySuffix(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' && character < 'A' || character > 'Z' && character < 'a' || character > 'z' {
			return false
		}
	}
	return true
}

func trustedCommandOutput(ctx context.Context, commandPath string, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, commandPath, arguments...)
	command.Env = trustedEnvironment("")
	var stdout limitedWriter
	stdout.remaining = 8 << 20
	var stderr limitedWriter
	stderr.remaining = 1 << 20
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, stderr.String())
	}
	if stdout.truncated || stderr.truncated {
		return nil, fmt.Errorf("trusted command output exceeded its bound: %s", stderr.String())
	}
	return stdout.Bytes(), nil
}

func runTrustedCommand(ctx context.Context, commandPath string, arguments ...string) error {
	_, err := trustedCommandOutput(ctx, commandPath, arguments...)
	return err
}

func trustedEnvironment(temporaryDirectory string) []string {
	environment := []string{"LANG=C", "LC_ALL=C", "PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
	if temporaryDirectory != "" {
		environment = append(environment, "TMPDIR="+temporaryDirectory)
	}
	return environment
}

type limitedWriter struct {
	buffer    bytes.Buffer
	remaining int64
	truncated bool
}

func (writer *limitedWriter) Write(data []byte) (int, error) {
	length := len(data)
	remainingBefore := writer.remaining
	if writer.remaining > 0 {
		keep := int64(length)
		if keep > writer.remaining {
			keep = writer.remaining
		}
		_, _ = writer.buffer.Write(data[:keep])
		writer.remaining -= keep
	}
	if int64(length) > remainingBefore {
		writer.truncated = true
	}
	return length, nil
}

func (writer *limitedWriter) Bytes() []byte { return append([]byte(nil), writer.buffer.Bytes()...) }

func (writer *limitedWriter) String() string {
	data := writer.buffer.Bytes()
	trimmed := len(data) > 4096
	if len(data) > 4096 {
		data = data[len(data)-4096:]
	}
	value := sanitizeGuestLog(string(data))
	if writer.truncated || trimmed {
		value = "[earlier console output truncated] " + value
	}
	return value
}

type buildCgroup struct {
	path string
	fd   int
}

func verifyCgroupDelegation(root string) error {
	info, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("inspect delegated cgroup root: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || stat.Uid != 0 || info.Mode().Perm()&0o022 != 0 {
		return errors.New("delegated cgroup root must be a root-owned real directory that is not group- or other-writable")
	}
	controllers, err := os.ReadFile(filepath.Join(root, "cgroup.controllers"))
	if err != nil {
		return fmt.Errorf("read delegated cgroup controllers: %w", err)
	}
	available := make(map[string]struct{})
	for _, controller := range strings.Fields(string(controllers)) {
		available[controller] = struct{}{}
	}
	for _, controller := range []string{"cpu", "memory", "pids"} {
		if _, found := available[controller]; !found {
			return fmt.Errorf("delegated cgroup root does not expose %s controller", controller)
		}
	}
	probe, err := os.MkdirTemp(root, "layercache-preflight-")
	if err != nil {
		return fmt.Errorf("create delegated cgroup: %w", err)
	}
	defer os.Remove(probe)
	for _, file := range []string{"cpu.max", "memory.max", "pids.max"} {
		if _, err := os.Stat(filepath.Join(probe, file)); err != nil {
			return fmt.Errorf("delegated cgroup does not activate %s: %w", file, err)
		}
	}
	return nil
}

func createBuildCgroup(root string, prefix string, build publicbuild.Build, guestProcesses int64) (*buildCgroup, error) {
	directory, err := os.MkdirTemp(root, prefix)
	if err != nil {
		return nil, fmt.Errorf("create Public Build cgroup: %w", err)
	}
	cleanup := func() { _ = os.Remove(directory) }
	quota := build.Request.Resources.CPUMillis * cpuPeriodMicros / 1000
	if quota <= 0 {
		quota = 1
	}
	memoryLimit := build.Request.Resources.MemoryBytes
	if memoryLimit > int64(^uint64(0)>>1)-qemuMemoryOverhead {
		cleanup()
		return nil, errors.New("Public Build memory limit overflows QEMU allowance")
	}
	memoryLimit += qemuMemoryOverhead
	virtualCPUs := (build.Request.Resources.CPUMillis + 999) / 1000
	hostProcesses := guestProcesses + virtualCPUs + 64
	settings := map[string]string{
		"cpu.max":    fmt.Sprintf("%d %d", quota, cpuPeriodMicros),
		"memory.max": strconv.FormatInt(memoryLimit, 10),
		"pids.max":   strconv.FormatInt(hostProcesses, 10),
	}
	if _, err := os.Stat(filepath.Join(directory, "memory.swap.max")); err == nil {
		settings["memory.swap.max"] = "0"
	}
	for name, value := range settings {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(value), 0o600); err != nil {
			cleanup()
			return nil, fmt.Errorf("set Public Build cgroup %s: %w", name, err)
		}
	}
	fd, err := unix.Open(directory, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("open Public Build cgroup: %w", err)
	}
	return &buildCgroup{path: directory, fd: fd}, nil
}

func (cgroup *buildCgroup) Close() error {
	if cgroup == nil {
		return nil
	}
	closeErr := unix.Close(cgroup.fd)
	removeErr := os.Remove(cgroup.path)
	if closeErr != nil {
		return closeErr
	}
	return removeErr
}
