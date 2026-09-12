//go:build linux

// The Public Build guest agent is a standalone static init binary. Build it
// with CGO_ENABLED=0 and install it at the Agent path in the pinned guest
// contract. It receives no credentials and has no network device.
package main

import (
	"archive/tar"
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
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/layercache/layercache/internal/publicbuild"
	"github.com/layercache/layercache/internal/publicbuild/sandbox"
	"github.com/layercache/layercache/internal/publicbuild/sandbox/actionsjob"
	"golang.org/x/sys/unix"
)

const (
	guestContractPath = "/etc/layercache/public-build-contract.json"
	sourceMountPath   = "/source"
	workMountPath     = "/work"
	maximumMessage    = 64 << 10
	maximumToolLog    = 256 << 10
	maximumTurboPlan  = 4 << 20
	untrustedUID      = 65534
	untrustedGID      = 65534
)

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
	Resources    struct {
		CPUMillis    int64 `json:"cpuMillis"`
		MemoryBytes  int64 `json:"memoryBytes"`
		DiskBytes    int64 `json:"diskBytes"`
		TimeoutMS    int64 `json:"timeoutMilliseconds"`
		MaxProcesses int64 `json:"maxProcesses"`
	} `json:"resources"`
}

type guestOutput struct {
	nativeKey string
	mediaType string
	path      string
	size      int64
}

func main() {
	if err := runGuest(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "Public Build guest:", cleanMessage(err.Error()))
	}
	shutdownGuest()
}

func runGuest() error {
	if err := mountGuestFilesystems(); err != nil {
		return err
	}
	contract, err := loadEmbeddedContract()
	if err != nil {
		return err
	}
	if err := applyGuestProcessLimit(contract.MaxProcesses); err != nil {
		return err
	}
	control, err := openControlPort(contract.ControlPort, 15*time.Second)
	if err != nil {
		return err
	}
	defer control.Close()
	if err := writeJSONLine(control, map[string]string{"type": "hello", "protocol": contract.Protocol}); err != nil {
		return err
	}
	request, err := readExecuteRequest(control)
	if err != nil {
		_ = writeFailed(control, err)
		return err
	}
	recipe, err := contract.Recipe(publicbuild.Build{Request: publicbuild.BuildRequest{
		Repository: request.Repository, Commit: request.Commit, Integration: request.Integration,
		Target: request.Target, RecipeDigest: request.RecipeDigest, Platform: contract.Platform,
		Inputs: append([]publicbuild.DeclaredInput(nil), request.Inputs...),
	}})
	if err != nil || recipe.Executor == "" {
		if err == nil {
			err = errors.New("leased recipe has no maintained guest executor")
		}
		_ = writeFailed(control, err)
		return err
	}
	if request.Resources.TimeoutMS <= 0 || request.Resources.MaxProcesses != contract.MaxProcesses {
		err := errors.New("execute request resource contract is invalid")
		_ = writeFailed(control, err)
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(request.Resources.TimeoutMS)*time.Millisecond)
	defer cancel()
	workspace := filepath.Join(workMountPath, "source")
	if err := copySourceTree(sourceMountPath, workspace); err != nil {
		_ = writeFailed(control, err)
		return err
	}
	var output guestOutput
	switch recipe.Executor {
	case sandbox.ExecutorTurboCaptureV1:
		output, err = runTurboRecipe(ctx, request, recipe, workspace)
	case sandbox.ExecutorBuildKitOCIV1:
		output, err = runBuildKitRecipe(ctx, request, recipe, contract, workspace)
	case sandbox.ExecutorActionsJobV2:
		output, err = runActionsRecipe(ctx, request, recipe, contract, workspace)
	default:
		err = errors.New("guest contract names an unsupported executor")
	}
	if err != nil {
		_ = writeFailed(control, err)
		return err
	}
	if err := streamOutput(control, output, recipe.MaxBytes); err != nil {
		_ = writeFailed(control, err)
		return err
	}
	return writeJSONLine(control, map[string]string{"type": "complete"})
}

func applyGuestProcessLimit(maximum int64) error {
	if maximum <= 0 {
		return errors.New("guest process limit must be positive")
	}
	const cgroupPath = "/sys/fs/cgroup"
	if err := os.MkdirAll(cgroupPath, 0o755); err != nil {
		return fmt.Errorf("create guest cgroup mount: %w", err)
	}
	if err := unix.Mount("none", cgroupPath, "cgroup2", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil && !errors.Is(err, unix.EBUSY) {
		return fmt.Errorf("mount guest cgroup v2: %w", err)
	}
	controllers, err := os.ReadFile(filepath.Join(cgroupPath, "cgroup.controllers"))
	if err != nil {
		return fmt.Errorf("read guest cgroup controllers: %w", err)
	}
	if !slices.Contains(strings.Fields(string(controllers)), "pids") {
		return errors.New("guest cgroup v2 does not provide the pids controller")
	}
	// The root cgroup cannot be limited portably, and many kernels reject a
	// pids.max write there. Move init into one fresh leaf before enabling the
	// controller for that leaf. Every later guest process inherits this cgroup.
	processCgroup := filepath.Join(cgroupPath, "layercache")
	if err := os.Mkdir(processCgroup, 0o755); err != nil {
		return fmt.Errorf("create guest process cgroup: %w", err)
	}
	if err := os.WriteFile(
		filepath.Join(processCgroup, "cgroup.procs"),
		[]byte(strconv.Itoa(os.Getpid())+"\n"),
		0o600,
	); err != nil {
		return fmt.Errorf("move guest init into process cgroup: %w", err)
	}
	if err := os.WriteFile(
		filepath.Join(cgroupPath, "cgroup.subtree_control"),
		[]byte("+pids\n"),
		0o600,
	); err != nil {
		return fmt.Errorf("enable guest pids controller: %w", err)
	}
	if err := os.WriteFile(
		filepath.Join(processCgroup, "pids.max"),
		[]byte(strconv.FormatInt(maximum, 10)+"\n"),
		0o600,
	); err != nil {
		return fmt.Errorf("apply guest pids limit: %w", err)
	}
	return nil
}

func mountGuestFilesystems() error {
	for _, directory := range []string{"/proc", "/sys", "/dev", sourceMountPath, workMountPath} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			return err
		}
	}
	_ = unix.Mount("proc", "/proc", "proc", 0, "")
	_ = unix.Mount("sysfs", "/sys", "sysfs", 0, "")
	_ = unix.Mount("devtmpfs", "/dev", "devtmpfs", 0, "mode=0755")
	if err := unix.Mount("/dev/vdb", sourceMountPath, "ext4", unix.MS_RDONLY|unix.MS_NODEV|unix.MS_NOSUID, ""); err != nil {
		return fmt.Errorf("mount immutable source: %w", err)
	}
	format := exec.Command(sandbox.GuestMKFSPath, "-t", "ext4", "-F", "-m", "0", "/dev/vdc")
	format.Env = guestEnvironment(workMountPath)
	if output, err := format.CombinedOutput(); err != nil {
		return fmt.Errorf("format ephemeral work disk: %w: %s", err, cleanMessage(string(output)))
	}
	if err := unix.Mount("/dev/vdc", workMountPath, "ext4", unix.MS_NODEV|unix.MS_NOSUID, ""); err != nil {
		return fmt.Errorf("mount ephemeral work disk: %w", err)
	}
	return nil
}

type interfaceFlagController interface {
	flags(string) (uint16, error)
	setFlags(string, uint16) error
}

type ioctlInterfaceFlagController struct {
	fd int
}

func (controller ioctlInterfaceFlagController) flags(name string) (uint16, error) {
	request, err := unix.NewIfreq(name)
	if err != nil {
		return 0, err
	}
	if err := unix.IoctlIfreq(controller.fd, unix.SIOCGIFFLAGS, request); err != nil {
		return 0, err
	}
	return request.Uint16(), nil
}

func (controller ioctlInterfaceFlagController) setFlags(name string, flags uint16) error {
	request, err := unix.NewIfreq(name)
	if err != nil {
		return err
	}
	request.SetUint16(flags)
	return unix.IoctlIfreq(controller.fd, unix.SIOCSIFFLAGS, request)
}

func enableGuestLoopback() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open guest network control socket: %w", err)
	}
	defer unix.Close(fd)
	if err := enableLoopback(ioctlInterfaceFlagController{fd: fd}); err != nil {
		return fmt.Errorf("bring guest loopback up: %w", err)
	}
	return nil
}

func enableLoopback(controller interfaceFlagController) error {
	flags, err := controller.flags("lo")
	if err != nil {
		return fmt.Errorf("read loopback flags: %w", err)
	}
	if flags&uint16(unix.IFF_LOOPBACK) == 0 {
		return errors.New("lo is not a loopback interface")
	}
	if flags&uint16(unix.IFF_UP) != 0 {
		return nil
	}
	if err := controller.setFlags("lo", flags|uint16(unix.IFF_UP)); err != nil {
		return fmt.Errorf("set loopback flags: %w", err)
	}
	return nil
}

func loadEmbeddedContract() (sandbox.GuestContract, error) {
	encoded, err := os.ReadFile(guestContractPath)
	if err != nil {
		return sandbox.GuestContract{}, fmt.Errorf("read embedded guest contract: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return sandbox.LoadGuestContract(guestContractPath, "sha256:"+hex.EncodeToString(digest[:]))
}

func openControlPort(name string, timeout time.Duration) (*os.File, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		matches, _ := filepath.Glob("/sys/class/virtio-ports/vport*/name")
		for _, namePath := range matches {
			encoded, err := os.ReadFile(namePath)
			if err != nil || strings.TrimSpace(string(encoded)) != name {
				continue
			}
			device := filepath.Join("/dev", filepath.Base(filepath.Dir(namePath)))
			file, err := os.OpenFile(device, os.O_RDWR, 0)
			if err == nil {
				return file, nil
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	return nil, errors.New("virtio control port did not appear")
}

func readExecuteRequest(reader io.Reader) (executeRequest, error) {
	buffered := bufio.NewReaderSize(reader, maximumMessage)
	line, err := buffered.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) || len(line) > maximumMessage {
		return executeRequest{}, errors.New("execute request is too large")
	}
	if err != nil {
		return executeRequest{}, fmt.Errorf("read execute request: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	var request executeRequest
	if err := decoder.Decode(&request); err != nil {
		return executeRequest{}, fmt.Errorf("decode execute request: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return executeRequest{}, errors.New("execute request contains trailing JSON")
	}
	if request.Type != "execute" || request.Protocol != sandbox.ProtocolV1 || request.BuildID == "" {
		return executeRequest{}, errors.New("execute request does not match the guest protocol")
	}
	return request, nil
}

func copySourceTree(source, destination string) error {
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return err
	}
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == source {
			return nil
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.Mkdir(target, 0o755)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			linkTarget, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := sandbox.ValidateSourceSymlink(filepath.ToSlash(relative), filepath.ToSlash(linkTarget)); err != nil {
				return fmt.Errorf("source symlink %q: %w", relative, err)
			}
			return os.Symlink(linkTarget, target)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("source path %q is not a regular file or directory", relative)
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if info.Mode()&0o111 != 0 {
			mode = 0o755
		}
		output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			input.Close()
			return err
		}
		_, copyErr := io.Copy(output, input)
		return errors.Join(copyErr, input.Close(), output.Close())
	})
}

type turboCapture struct {
	mu          sync.Mutex
	path        string
	expectedKey string
	nativeKey   string
	err         error
	busy        bool
	maxBytes    int64
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

func runTurboRecipe(
	ctx context.Context,
	request executeRequest,
	recipe sandbox.RecipeContract,
	workspace string,
) (guestOutput, error) {
	if err := enableGuestLoopback(); err != nil {
		return guestOutput{}, err
	}
	homeDirectory, temporaryDirectory, cleanup, err := prepareUntrustedWorkspace(workspace, "turbo")
	if err != nil {
		return guestOutput{}, err
	}
	defer cleanup()
	turboEnvironment := []string{
		"HOME=" + homeDirectory,
		"LANG=C",
		"LC_ALL=C",
		"PATH=/opt/layercache/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"TMPDIR=" + temporaryDirectory,
		"TURBO_TELEMETRY_DISABLED=1",
	}
	planOutput := &boundedBuffer{remaining: maximumTurboPlan}
	planLog := &boundedLog{remaining: maximumToolLog}
	planCommand := exec.CommandContext(
		ctx,
		sandbox.GuestTurboPath,
		"run",
		request.Target,
		"--dry=json",
		"--cache=remote:rw",
	)
	planCommand.Dir = workspace
	planCommand.Env = turboEnvironment
	planCommand.Stdout = planOutput
	planCommand.Stderr = planLog
	if err := runUntrustedCommand(planCommand); err != nil {
		return guestOutput{}, fmt.Errorf("inspect maintained Turbo task graph: %w: %s", err, planLog.String())
	}
	expectedKey, err := validateTurboPlan(planOutput.Bytes(), request.Target)
	if err != nil {
		return guestOutput{}, err
	}
	capture := &turboCapture{
		path:        filepath.Join(workMountPath, "turbo-artifact.bin"),
		expectedKey: expectedKey,
		maxBytes:    recipe.MaxBytes,
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return guestOutput{}, err
	}
	server := &http.Server{Handler: capture, ReadHeaderTimeout: 2 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	defer func() {
		_ = server.Close()
		<-done
	}()
	endpoint := "http://" + listener.Addr().String()
	command := exec.CommandContext(
		ctx,
		sandbox.GuestTurboPath,
		"run",
		request.Target,
		"--summarize",
		"--cache=remote:rw",
	)
	command.Dir = workspace
	command.Env = append(append([]string(nil), turboEnvironment...),
		"TURBO_API="+endpoint,
		"TURBO_TOKEN=public-build-guest",
		"TURBO_TEAM=layercache-public-build",
	)
	log := &boundedLog{remaining: maximumToolLog}
	command.Stdout = log
	command.Stderr = log
	if err := runUntrustedCommand(command); err != nil {
		return guestOutput{}, fmt.Errorf("maintained Turbo task failed: %w: %s", err, log.String())
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	if capture.err != nil {
		return guestOutput{}, capture.err
	}
	if capture.busy || capture.nativeKey == "" {
		return guestOutput{}, errors.New("maintained Turbo task did not finish exactly one cache artifact")
	}
	info, err := os.Stat(capture.path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 {
		return guestOutput{}, errors.New("maintained Turbo task produced no regular cache artifact")
	}
	return guestOutput{
		nativeKey: capture.nativeKey, mediaType: recipe.MediaType,
		path: capture.path, size: info.Size(),
	}, nil
}

func validateTurboPlan(encoded []byte, target string) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	var plan turboPlan
	if err := decoder.Decode(&plan); err != nil {
		return "", fmt.Errorf("decode maintained Turbo task graph: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return "", errors.New("maintained Turbo task graph contains trailing JSON")
	}
	if len(plan.Tasks) != 1 || plan.Tasks[0].TaskID != target ||
		plan.Tasks[0].Hash == "" || len(plan.Tasks[0].Hash) > 1024 ||
		strings.ContainsAny(plan.Tasks[0].Hash, "/\x00\r\n") ||
		len(plan.Tasks[0].Dependencies) != 0 ||
		plan.Tasks[0].ResolvedTaskDefinition.Cache == nil ||
		!*plan.Tasks[0].ResolvedTaskDefinition.Cache ||
		plan.Tasks[0].ResolvedTaskDefinition.Persistent {
		return "", errors.New("Turbo Public Build requires exactly one dependency-free fully qualified cacheable non-persistent task")
	}
	return plan.Tasks[0].Hash, nil
}

func (capture *turboCapture) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path == "/v8/artifacts/status" && request.Method == http.MethodGet {
		writeHTTPJSON(writer, http.StatusOK, map[string]string{"status": "enabled"})
		return
	}
	if request.URL.Path == "/v8/artifacts/events" && request.Method == http.MethodPost {
		_, _ = io.Copy(io.Discard, io.LimitReader(request.Body, 1<<20))
		writeHTTPJSON(writer, http.StatusOK, map[string]bool{"accepted": true})
		return
	}
	key := strings.TrimPrefix(request.URL.Path, "/v8/artifacts/")
	if key == request.URL.Path || key == "" || len(key) > 1024 || strings.ContainsAny(key, "/\x00\r\n") {
		http.Error(writer, "invalid artifact key", http.StatusBadRequest)
		return
	}
	if key != capture.expectedKey {
		http.Error(writer, "artifact key does not match the planned Turbo task", http.StatusConflict)
		return
	}
	if request.Method == http.MethodHead || request.Method == http.MethodGet {
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	if request.Method != http.MethodPut {
		writer.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	capture.mu.Lock()
	if capture.busy || capture.nativeKey != "" {
		capture.err = errors.New("maintained Turbo task produced more than one cache artifact")
		capture.mu.Unlock()
		http.Error(writer, "only one cache artifact is supported", http.StatusConflict)
		return
	}
	capture.busy = true
	capture.mu.Unlock()
	file, err := os.OpenFile(capture.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		var written int64
		written, err = io.Copy(file, io.LimitReader(request.Body, capture.maxBytes+1))
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err == nil && written > capture.maxBytes {
			err = fmt.Errorf("Turbo artifact exceeds %d bytes", capture.maxBytes)
		}
	}
	capture.mu.Lock()
	capture.busy = false
	if err != nil {
		capture.err = err
		_ = os.Remove(capture.path)
	} else {
		capture.nativeKey = key
	}
	capture.mu.Unlock()
	if err != nil {
		http.Error(writer, "artifact collection failed", http.StatusInsufficientStorage)
		return
	}
	writeHTTPJSON(writer, http.StatusOK, map[string]bool{"stored": true})
}

func runBuildKitRecipe(
	ctx context.Context,
	request executeRequest,
	recipe sandbox.RecipeContract,
	contract sandbox.GuestContract,
	workspace string,
) (guestOutput, error) {
	stateRoot := filepath.Join(workMountPath, "buildkitd")
	resultPath := filepath.Join(workMountPath, "buildkit-result")
	socketPath := filepath.Join(workMountPath, "buildkitd.sock")
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		return guestOutput{}, err
	}
	daemon := exec.CommandContext(ctx, sandbox.GuestBuildkitdPath,
		"--addr", "unix://"+socketPath,
		"--root", stateRoot,
		"--containerd-worker=false",
		"--oci-worker=true",
		"--oci-worker-snapshotter=native",
		"--oci-worker-binary="+sandbox.GuestRuncPath,
		"--oci-worker-gc=false",
	)
	daemon.Env = guestEnvironment(workMountPath)
	daemonLog := &boundedLog{remaining: maximumToolLog}
	daemon.Stdout = daemonLog
	daemon.Stderr = daemonLog
	if err := daemon.Start(); err != nil {
		return guestOutput{}, fmt.Errorf("start private BuildKit daemon: %w", err)
	}
	daemonDone := make(chan error, 1)
	go func() { daemonDone <- daemon.Wait() }()
	stopDaemon := func() error {
		if daemon.Process != nil {
			_ = daemon.Process.Signal(syscall.SIGTERM)
		}
		select {
		case err := <-daemonDone:
			return err
		case <-time.After(10 * time.Second):
			if daemon.Process != nil {
				_ = daemon.Process.Kill()
			}
			return <-daemonDone
		}
	}
	stopped := false
	defer func() {
		if !stopped {
			_ = stopDaemon()
		}
	}()
	if err := waitForUnixSocket(ctx, socketPath, daemon.Process); err != nil {
		return guestOutput{}, fmt.Errorf("private BuildKit daemon did not become ready: %w: %s", err, daemonLog.String())
	}
	command := exec.CommandContext(ctx, sandbox.GuestBuildctlPath,
		"--addr", "unix://"+socketPath,
		"build",
		"--progress=plain",
		"--frontend=dockerfile.v0",
		"--local", "context="+workspace,
		"--local", "dockerfile="+workspace,
		"--opt", "target="+request.Target,
		"--opt", "platform="+string(contract.Platform),
		"--export-cache", "type=local,dest="+resultPath+",mode=max,oci-mediatypes=true,image-manifest=true",
	)
	command.Dir = workspace
	command.Env = guestEnvironment(workMountPath)
	buildLog := &boundedLog{remaining: maximumToolLog}
	command.Stdout = buildLog
	command.Stderr = buildLog
	if err := command.Run(); err != nil {
		return guestOutput{}, fmt.Errorf("maintained BuildKit target failed: %w: %s", err, buildLog.String())
	}
	_ = stopDaemon()
	stopped = true
	if err := removeEmptyBuildKitIngest(resultPath); err != nil {
		return guestOutput{}, err
	}
	archivePath := filepath.Join(workMountPath, "buildkit-result.tar")
	if err := writeDeterministicTar(resultPath, archivePath, recipe.MaxBytes); err != nil {
		return guestOutput{}, err
	}
	info, err := os.Stat(archivePath)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 {
		return guestOutput{}, errors.New("BuildKit did not produce a complete OCI result")
	}
	buildRequest := publicbuild.BuildRequest{
		Repository: request.Repository, Commit: request.Commit, Integration: request.Integration,
		Target: request.Target, RecipeDigest: request.RecipeDigest,
		Platform: contract.Platform,
		Inputs:   append([]publicbuild.DeclaredInput(nil), request.Inputs...),
	}
	nativeKey, err := publicbuild.BuildKitPublicNativeKey(buildRequest)
	if err != nil {
		return guestOutput{}, err
	}
	return guestOutput{
		nativeKey: nativeKey, mediaType: sandbox.OCIImageLayoutTarMediaType,
		path: archivePath, size: info.Size(),
	}, nil
}

func removeEmptyBuildKitIngest(resultPath string) error {
	ingestPath := filepath.Join(resultPath, "ingest")
	info, err := os.Lstat(ingestPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect BuildKit ingest staging: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("BuildKit ingest staging is not a real directory")
	}
	entries, err := os.ReadDir(ingestPath)
	if err != nil {
		return fmt.Errorf("inspect BuildKit ingest staging contents: %w", err)
	}
	if len(entries) != 0 {
		return errors.New("BuildKit left non-empty ingest staging after export")
	}
	if err := os.Remove(ingestPath); err != nil {
		return fmt.Errorf("remove empty BuildKit ingest staging: %w", err)
	}
	return nil
}

func runActionsRecipe(
	ctx context.Context,
	request executeRequest,
	recipe sandbox.RecipeContract,
	contract sandbox.GuestContract,
	workspace string,
) (guestOutput, error) {
	buildRequest := publicbuild.BuildRequest{
		Repository: request.Repository, Commit: request.Commit, Integration: request.Integration,
		Target: request.Target, RecipeDigest: request.RecipeDigest, Platform: contract.Platform,
		Inputs: append([]publicbuild.DeclaredInput(nil), request.Inputs...),
	}
	nativeKey, err := publicbuild.ActionsPublicNativeKey(buildRequest, contract.Toolchain, contract.Builder)
	if err != nil {
		return guestOutput{}, err
	}
	inputs := make(map[string]string, len(request.Inputs))
	for _, input := range request.Inputs {
		inputs[input.Name] = input.Value
	}
	expandedLimit := request.Resources.DiskBytes / 2
	if maximum := int64(100 << 30); expandedLimit > maximum {
		expandedLimit = maximum
	}
	if expandedLimit <= 0 {
		return guestOutput{}, errors.New("maintained Actions job has no expanded-output budget")
	}
	resultDirectory := filepath.Join(workMountPath, "actions-result")
	if err := os.Mkdir(resultDirectory, 0o700); err != nil {
		return guestOutput{}, err
	}
	requestPath := filepath.Join(workMountPath, "actions-request.json")
	encoded, err := json.Marshal(actionsjob.Request{
		Source: workspace, Target: request.Target, Platform: string(contract.Platform),
		Repository: request.Repository, Commit: request.Commit, RecipeDigest: request.RecipeDigest,
		Compatibility: inputs["compatibility"], Key: inputs["actions.key"],
		Ref: inputs["actions.ref"], Version: inputs["actions.version"],
		Toolchain: contract.Toolchain, Builder: contract.Builder,
		MaxArchiveBytes: recipe.MaxBytes, MaxExpandedBytes: expandedLimit,
		MaxProcesses: contract.MaxProcesses,
	})
	if err != nil || os.WriteFile(requestPath, encoded, 0o600) != nil {
		return guestOutput{}, errors.New("stage maintained Actions job request")
	}
	command := exec.CommandContext(ctx, sandbox.GuestActionsRunnerPath,
		"--request", requestPath, "--output", resultDirectory,
	)
	command.Env = guestEnvironment(workMountPath)
	log := &boundedLog{remaining: maximumToolLog}
	command.Stdout = log
	command.Stderr = log
	runErr := command.Run()
	cleanupErr := killGuestUIDProcesses(untrustedUID)
	if runErr != nil || cleanupErr != nil {
		return guestOutput{}, fmt.Errorf(
			"maintained Actions job failed: %w: %s",
			errors.Join(runErr, cleanupErr),
			log.String(),
		)
	}
	archivePath := filepath.Join(resultDirectory, "archive.bin")
	info, err := os.Lstat(archivePath)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > recipe.MaxBytes {
		return guestOutput{}, errors.New("maintained Actions runner did not produce one bounded native archive")
	}
	return guestOutput{
		nativeKey: nativeKey, mediaType: recipe.MediaType,
		path: archivePath, size: info.Size(),
	}, nil
}

func waitForUnixSocket(ctx context.Context, socketPath string, process *os.Process) error {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		connection, err := net.DialTimeout("unix", socketPath, 100*time.Millisecond)
		if err == nil {
			connection.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if process == nil || process.Signal(syscall.Signal(0)) != nil {
				return errors.New("BuildKit daemon exited")
			}
		}
	}
}

func writeDeterministicTar(sourceRoot, destination string, maximumBytes int64) error {
	paths := make([]string, 0)
	if err := filepath.WalkDir(sourceRoot, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != sourceRoot {
			paths = append(paths, path)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("inspect BuildKit OCI result: %w", err)
	}
	sort.Strings(paths)
	file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	limited := &maximumWriter{writer: file, remaining: maximumBytes}
	writer := tar.NewWriter(limited)
	for _, path := range paths {
		info, err := os.Lstat(path)
		if err != nil {
			file.Close()
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			file.Close()
			return fmt.Errorf("BuildKit OCI result contains non-regular path %q", path)
		}
		relative, err := filepath.Rel(sourceRoot, path)
		if err != nil {
			file.Close()
			return err
		}
		header := &tar.Header{
			Name: filepath.ToSlash(relative), ModTime: time.Unix(0, 0), AccessTime: time.Unix(0, 0),
			ChangeTime: time.Unix(0, 0), Uid: 0, Gid: 0,
		}
		if info.IsDir() {
			header.Typeflag = tar.TypeDir
			header.Mode = 0o700
		} else {
			header.Typeflag = tar.TypeReg
			header.Mode = 0o600
			header.Size = info.Size()
		}
		if err := writer.WriteHeader(header); err != nil {
			file.Close()
			return fmt.Errorf("archive BuildKit OCI result: %w", err)
		}
		if info.Mode().IsRegular() {
			input, err := os.Open(path)
			if err != nil {
				file.Close()
				return err
			}
			_, copyErr := io.Copy(writer, input)
			closeErr := input.Close()
			if copyErr != nil || closeErr != nil {
				file.Close()
				return errors.Join(copyErr, closeErr)
			}
		}
	}
	return errors.Join(writer.Close(), file.Sync(), file.Close())
}

func streamOutput(writer io.Writer, output guestOutput, maximumBytes int64) error {
	if output.nativeKey == "" || len(output.nativeKey) > 1024 || strings.ContainsAny(output.nativeKey, "\x00\r\n") {
		return errors.New("maintained recipe produced an invalid native cache key")
	}
	info, err := os.Lstat(output.path)
	if err != nil || !info.Mode().IsRegular() || info.Size() != output.size || output.size <= 0 || output.size > maximumBytes {
		return errors.New("maintained recipe output changed or exceeds its byte limit")
	}
	if err := writeJSONLine(writer, map[string]any{
		"type": "output", "nativeKey": output.nativeKey,
		"mediaType": output.mediaType, "sizeBytes": output.size,
	}); err != nil {
		return err
	}
	file, err := os.Open(output.path)
	if err != nil {
		return err
	}
	defer file.Close()
	written, err := io.CopyN(writer, file, output.size)
	if err != nil || written != output.size {
		return errors.New("stream maintained recipe output")
	}
	return nil
}

func writeJSONLine(writer io.Writer, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	for len(encoded) > 0 {
		written, err := writer.Write(encoded)
		if err != nil {
			return err
		}
		if written <= 0 {
			return io.ErrShortWrite
		}
		encoded = encoded[written:]
	}
	return nil
}

func writeFailed(writer io.Writer, cause error) error {
	return writeJSONLine(writer, map[string]string{"type": "failed", "message": cleanMessage(cause.Error())})
}

func writeHTTPJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func guestEnvironment(temporaryDirectory string) []string {
	return []string{
		"HOME=" + workMountPath,
		"LANG=C", "LC_ALL=C",
		"PATH=/opt/layercache/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"TMPDIR=" + temporaryDirectory,
	}
}

func prepareUntrustedWorkspace(workspace, prefix string) (string, string, func(), error) {
	err := filepath.Walk(workspace, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			relative, err := filepath.Rel(workspace, path)
			if err != nil {
				return err
			}
			linkTarget, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := sandbox.ValidateSourceSymlink(filepath.ToSlash(relative), filepath.ToSlash(linkTarget)); err != nil {
				return fmt.Errorf("untrusted workspace symlink %q: %w", relative, err)
			}
			return os.Lchown(path, untrustedUID, untrustedGID)
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("untrusted workspace path %q is not a regular file or directory", path)
		}
		return os.Chown(path, untrustedUID, untrustedGID)
	})
	if err != nil {
		return "", "", func() {}, fmt.Errorf("assign untrusted workspace: %w", err)
	}
	home := filepath.Join(workMountPath, prefix+"-home")
	temporary := filepath.Join(workMountPath, prefix+"-tmp")
	cleanup := func() {
		_ = os.RemoveAll(home)
		_ = os.RemoveAll(temporary)
	}
	for _, directory := range []string{home, temporary} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			cleanup()
			return "", "", func() {}, err
		}
		if err := os.Chown(directory, untrustedUID, untrustedGID); err != nil {
			cleanup()
			return "", "", func() {}, err
		}
	}
	return home, temporary, cleanup, nil
}

func runUntrustedCommand(command *exec.Cmd) error {
	command.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: untrustedUID, Gid: untrustedGID},
		Setpgid:    true,
	}
	command.WaitDelay = time.Second
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		err := unix.Kill(-command.Process.Pid, unix.SIGKILL)
		if errors.Is(err, unix.ESRCH) {
			return nil
		}
		return err
	}
	runtime.LockOSThread()
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		runtime.UnlockOSThread()
		return fmt.Errorf("disable untrusted task privilege gains: %w", err)
	}
	startErr := command.Start()
	runtime.UnlockOSThread()
	if startErr != nil {
		return startErr
	}
	waitErr := command.Wait()
	groupErr := unix.Kill(-command.Process.Pid, unix.SIGKILL)
	if errors.Is(groupErr, unix.ESRCH) {
		groupErr = nil
	}
	cleanupErr := killGuestUIDProcesses(untrustedUID)
	return errors.Join(waitErr, groupErr, cleanupErr)
}

func killGuestUIDProcesses(uid int) error {
	for attempt := 0; attempt < 16; attempt++ {
		entries, err := os.ReadDir("/proc")
		if err != nil {
			return err
		}
		found := false
		for _, entry := range entries {
			pid, err := strconv.Atoi(entry.Name())
			if err != nil || pid <= 1 {
				continue
			}
			encoded, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "status"))
			if err != nil || !processHasUID(encoded, uid) {
				continue
			}
			found = true
			if err := unix.Kill(pid, unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
				return err
			}
		}
		for {
			pid, err := unix.Wait4(-1, nil, unix.WNOHANG, nil)
			if pid <= 0 || err != nil {
				break
			}
		}
		if !found {
			return nil
		}
	}
	return errors.New("untrusted task left guest processes running")
}

func processHasUID(status []byte, uid int) bool {
	prefix := "Uid:\t" + strconv.Itoa(uid) + "\t"
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

func cleanMessage(value string) string {
	value = strings.Map(func(character rune) rune {
		if character == '\n' || character == '\t' || character >= 0x20 {
			return character
		}
		return -1
	}, strings.TrimSpace(value))
	if len(value) > 4096 {
		value = value[:4096]
	}
	if value == "" {
		return "isolated guest recipe failed"
	}
	return value
}

type boundedLog struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	remaining int64
}

type boundedBuffer struct {
	buffer    bytes.Buffer
	remaining int64
}

func (buffer *boundedBuffer) Write(data []byte) (int, error) {
	if int64(len(data)) > buffer.remaining {
		return 0, errors.New("bounded output exceeded its limit")
	}
	written, err := buffer.buffer.Write(data)
	buffer.remaining -= int64(written)
	return written, err
}

func (buffer *boundedBuffer) Bytes() []byte {
	return buffer.buffer.Bytes()
}

func (log *boundedLog) Write(data []byte) (int, error) {
	log.mu.Lock()
	defer log.mu.Unlock()
	length := len(data)
	if log.remaining > 0 {
		keep := min(int64(length), log.remaining)
		_, _ = log.buffer.Write(data[:keep])
		log.remaining -= keep
	}
	return length, nil
}

func (log *boundedLog) String() string {
	log.mu.Lock()
	defer log.mu.Unlock()
	return cleanMessage(log.buffer.String())
}

type maximumWriter struct {
	writer    io.Writer
	remaining int64
}

func (writer *maximumWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > writer.remaining {
		return 0, errors.New("result exceeds maintained recipe byte limit")
	}
	written, err := writer.writer.Write(data)
	writer.remaining -= int64(written)
	return written, err
}

func shutdownGuest() {
	unix.Sync()
	_ = unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF)
	for {
		time.Sleep(time.Hour)
	}
}
