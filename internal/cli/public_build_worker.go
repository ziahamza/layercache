package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/layercache/layercache/internal/config"
	"github.com/layercache/layercache/internal/publicbuild"
	"github.com/layercache/layercache/internal/publicbuild/sandbox"
	"github.com/layercache/layercache/internal/publictrust"
)

const defaultWorkerPollInterval = 5 * time.Second

func runPublicBuildWorkerRun(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags, configPath, jsonOutput, err := newPublicBuildFlagSet("public-build worker run", stderr)
	if err != nil {
		return err
	}
	workerID := flags.String("worker-id", envOrEmpty("LAYER_CACHE_PUBLIC_BUILD_WORKER_ID"), "stable worker identity")
	qemuPath := flags.String("qemu", envOrEmpty("LAYER_CACHE_PUBLIC_BUILD_QEMU"), "trusted qemu-system executable")
	qemuImagePath := flags.String("qemu-img", envOrEmpty("LAYER_CACHE_PUBLIC_BUILD_QEMU_IMG"), "trusted qemu-img executable")
	mke2fsPath := flags.String("mke2fs", envOrEmpty("LAYER_CACHE_PUBLIC_BUILD_MKE2FS"), "trusted mke2fs executable")
	debugFSPath := flags.String("debugfs", envOrEmpty("LAYER_CACHE_PUBLIC_BUILD_DEBUGFS"), "trusted debugfs executable")
	kernelPath := flags.String("kernel", envOrEmpty("LAYER_CACHE_PUBLIC_BUILD_KERNEL"), "immutable guest kernel")
	kernelSHA256 := flags.String("kernel-sha256", envOrEmpty("LAYER_CACHE_PUBLIC_BUILD_KERNEL_SHA256"), "pinned guest kernel SHA-256")
	rootFSPath := flags.String("rootfs", envOrEmpty("LAYER_CACHE_PUBLIC_BUILD_ROOTFS"), "immutable raw guest rootfs")
	rootFSSHA256 := flags.String("rootfs-sha256", envOrEmpty("LAYER_CACHE_PUBLIC_BUILD_ROOTFS_SHA256"), "pinned guest rootfs SHA-256")
	contractPath := flags.String("guest-contract", envOrEmpty("LAYER_CACHE_PUBLIC_BUILD_GUEST_CONTRACT"), "immutable guest protocol contract")
	contractSHA256 := flags.String("guest-contract-sha256", envOrEmpty("LAYER_CACHE_PUBLIC_BUILD_GUEST_CONTRACT_SHA256"), "pinned guest contract SHA-256")
	cgroupRoot := flags.String("cgroup-root", envOrEmpty("LAYER_CACHE_PUBLIC_BUILD_CGROUP_ROOT"), "delegated cgroup v2 directory")
	workRoot := flags.String("work-root", envOrEmpty("LAYER_CACHE_PUBLIC_BUILD_WORK_ROOT"), "root-owned worker scratch directory")
	sandboxUID := flags.Uint("sandbox-uid", envUint("LAYER_CACHE_PUBLIC_BUILD_SANDBOX_UID"), "dedicated non-root QEMU UID")
	sandboxGID := flags.Uint("sandbox-gid", envUint("LAYER_CACHE_PUBLIC_BUILD_SANDBOX_GID"), "dedicated QEMU GID with KVM access")
	sourceBytes := flags.Int64("max-source-bytes", 1<<30, "maximum compressed and expanded source archive bytes")
	maxScratchBytes := flags.Int64("max-scratch-bytes", 0, "maximum aggregate scratch bytes for one Public Build")
	pollInterval := flags.Duration("poll-interval", defaultWorkerPollInterval, "queue poll interval")
	preflightOnly := flags.Bool("preflight", false, "validate the production isolation gate without leasing work")
	once := flags.Bool("once", false, "process at most one lease and exit")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("public-build worker run does not accept positional arguments")
	}
	if *workerID == "" {
		return errors.New("--worker-id is required")
	}
	if *pollInterval <= 0 || *sourceBytes <= 0 {
		return errors.New("--poll-interval and --max-source-bytes must be positive")
	}
	if uint64(*sandboxUID) > uint64(^uint32(0)) || uint64(*sandboxGID) > uint64(^uint32(0)) {
		return errors.New("sandbox UID or GID is outside the uint32 range")
	}
	cfg, err := loadPublicBuildCollectorConfig(*configPath)
	if err != nil {
		return err
	}
	if cfg.PublicBuildWorkerToken == "" {
		return errors.New("configuration has no Public Build worker credential")
	}
	if *kernelPath == "" {
		*kernelPath = cfg.PublicBuildKernelPath
	}
	if *kernelSHA256 == "" {
		*kernelSHA256 = cfg.PublicBuildKernelSHA256
	}
	if *rootFSPath == "" {
		*rootFSPath = cfg.PublicBuildRootFSPath
	}
	if *rootFSSHA256 == "" {
		*rootFSSHA256 = cfg.PublicBuildRootFSSHA256
	}
	if *contractPath == "" {
		*contractPath = cfg.PublicBuildGuestContract
	}
	if *contractSHA256 == "" {
		*contractSHA256 = cfg.PublicBuildContractSHA256
	}
	if *cgroupRoot == "" {
		*cgroupRoot = cfg.PublicBuildCgroupRoot
	}
	if *maxScratchBytes == 0 {
		*maxScratchBytes = cfg.PublicBuildMaxScratchBytes
	}
	if *workRoot == "" {
		*workRoot = cfg.PublicBuildWorkRoot
	}
	if *sandboxUID == 0 {
		*sandboxUID = uint(cfg.PublicBuildSandboxUID)
	}
	if *sandboxGID == 0 {
		*sandboxGID = uint(cfg.PublicBuildSandboxGID)
	}
	if *qemuPath == "" {
		*qemuPath = lookupWorkerTool(qemuSystemBinary(runtime.GOARCH))
	}
	if *qemuImagePath == "" {
		*qemuImagePath = lookupWorkerTool("qemu-img")
	}
	if *mke2fsPath == "" {
		*mke2fsPath = lookupWorkerTool("mke2fs")
	}
	if *debugFSPath == "" {
		*debugFSPath = lookupWorkerTool("debugfs")
	}
	if *workRoot == "" {
		*workRoot = filepath.Join(cfg.DataDir, "public-build-worker")
	}
	worker, err := sandbox.NewQEMUWorker(sandbox.QEMUConfig{
		WorkerID: *workerID, QEMUPath: *qemuPath, QEMUImagePath: *qemuImagePath,
		Mke2fsPath: *mke2fsPath, DebugFSPath: *debugFSPath,
		KernelPath: *kernelPath, KernelSHA256: *kernelSHA256,
		RootFSPath: *rootFSPath, RootFSSHA256: *rootFSSHA256,
		ContractPath: *contractPath, ContractSHA256: *contractSHA256,
		CgroupRoot: *cgroupRoot, WorkRoot: *workRoot,
		SandboxUID: uint32(*sandboxUID), SandboxGID: uint32(*sandboxGID),
		MaxScratchBytes:    *maxScratchBytes,
		SourceArchiveBytes: *sourceBytes,
	})
	if err != nil {
		return fmt.Errorf("configure QEMU Public Build worker: %w", err)
	}
	defer worker.Close()
	if err := worker.Contract().CoversServerRecipes(cfg.PublicBuildRecipeDigests); err != nil {
		return err
	}
	for _, integration := range worker.Contract().Capabilities().Integrations {
		if integration == publicbuild.IntegrationBuildKit {
			if err := sandbox.ValidateOCIRepository(cfg.BuildkitPublicRepository); err != nil {
				return fmt.Errorf("QEMU Public Build preflight failed: %w", err)
			}
			break
		}
	}
	report, err := worker.Preflight(ctx)
	if err != nil {
		return fmt.Errorf("QEMU Public Build preflight failed: %w", err)
	}
	if *preflightOnly {
		return printResult(stdout, *jsonOutput, report, "QEMU Public Build isolation preflight passed")
	}
	runContext, stopSignals := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	if err := writePublicBuildWorkerEvent(stdout, *jsonOutput, "ready", map[string]any{"preflight": report}); err != nil {
		return err
	}
	for {
		lease, leased, err := leasePublicBuild(runContext, cfg, worker)
		if err != nil {
			return err
		}
		if !leased {
			if *once {
				return writePublicBuildWorkerEvent(stdout, *jsonOutput, "idle", map[string]any{"leased": false})
			}
			select {
			case <-runContext.Done():
				return nil
			case <-time.After(*pollInterval):
				continue
			}
		}
		if err := writePublicBuildWorkerEvent(stdout, *jsonOutput, "leased", map[string]any{"buildId": lease.Build.ID}); err != nil {
			return err
		}
		outcome, err := executePublicBuildLease(runContext, cfg, worker, lease)
		if err != nil {
			return err
		}
		if err := writePublicBuildWorkerEvent(stdout, *jsonOutput, outcome.state, outcome.fields); err != nil {
			return err
		}
		if *once {
			return nil
		}
	}
}

type publicBuildWorkerLease struct {
	Token     string
	WorkerID  string
	ExpiresAt time.Time
	Build     publicbuild.Build
}

type publicBuildLeaseWire struct {
	LeaseToken string    `json:"leaseToken"`
	WorkerID   string    `json:"workerId"`
	ExpiresAt  time.Time `json:"expiresAt"`
	Build      struct {
		ID      string            `json:"id"`
		State   publicbuild.State `json:"state"`
		Request struct {
			Repository   string                      `json:"repository"`
			Commit       string                      `json:"commit"`
			Integration  publicbuild.Integration     `json:"integration"`
			Target       string                      `json:"target"`
			RecipeDigest string                      `json:"recipeDigest"`
			Platform     publicbuild.Platform        `json:"platform"`
			Inputs       []publicbuild.DeclaredInput `json:"inputs,omitempty"`
			Resources    struct {
				CPUMillis           int64 `json:"cpuMillis"`
				MemoryBytes         int64 `json:"memoryBytes"`
				DiskBytes           int64 `json:"diskBytes"`
				TimeoutMilliseconds int64 `json:"timeoutMilliseconds"`
			} `json:"resources"`
		} `json:"request"`
	} `json:"build"`
}

func leasePublicBuild(ctx context.Context, cfg config.Config, worker *sandbox.QEMUWorker) (publicBuildWorkerLease, bool, error) {
	capabilities := worker.Capabilities()
	body := map[string]any{
		"workerId": worker.ID(), "integrations": capabilities.Integrations,
		"platforms": capabilities.Platforms, "recipes": capabilities.Recipes,
	}
	encoded, status, err := callPublicBuild(ctx, cfg, http.MethodPost, "/v1/public-build-worker/lease", cfg.PublicBuildWorkerToken, body)
	if err != nil {
		return publicBuildWorkerLease{}, false, err
	}
	if status == http.StatusNoContent {
		return publicBuildWorkerLease{}, false, nil
	}
	var wire publicBuildLeaseWire
	if err := json.Unmarshal(encoded, &wire); err != nil {
		return publicBuildWorkerLease{}, false, fmt.Errorf("decode Public Build lease: %w", err)
	}
	if wire.LeaseToken == "" || wire.WorkerID != worker.ID() || wire.Build.ID == "" || wire.Build.State != publicbuild.StateRunning {
		return publicBuildWorkerLease{}, false, errors.New("Public Build server returned an incomplete lease")
	}
	if wire.Build.Request.Resources.TimeoutMilliseconds <= 0 {
		return publicBuildWorkerLease{}, false, errors.New("Public Build server returned an invalid timeout")
	}
	build := publicbuild.Build{
		ID: wire.Build.ID, State: wire.Build.State, WorkerID: wire.WorkerID,
		Request: publicbuild.BuildRequest{
			Repository: wire.Build.Request.Repository, Commit: wire.Build.Request.Commit,
			Integration: wire.Build.Request.Integration, Target: wire.Build.Request.Target,
			RecipeDigest: wire.Build.Request.RecipeDigest, Platform: wire.Build.Request.Platform,
			Inputs: append([]publicbuild.DeclaredInput(nil), wire.Build.Request.Inputs...),
			Resources: publicbuild.Resources{
				CPUMillis:   wire.Build.Request.Resources.CPUMillis,
				MemoryBytes: wire.Build.Request.Resources.MemoryBytes,
				DiskBytes:   wire.Build.Request.Resources.DiskBytes,
				Timeout:     time.Duration(wire.Build.Request.Resources.TimeoutMilliseconds) * time.Millisecond,
			},
		},
	}
	return publicBuildWorkerLease{Token: wire.LeaseToken, WorkerID: wire.WorkerID, ExpiresAt: wire.ExpiresAt, Build: build}, true, nil
}

type workerOutcome struct {
	state  string
	fields map[string]any
}

func executePublicBuildLease(
	ctx context.Context,
	cfg config.Config,
	worker *sandbox.QEMUWorker,
	lease publicBuildWorkerLease,
) (workerOutcome, error) {
	buildContext, cancelBuild := context.WithCancel(ctx)
	heartbeatContext, stopHeartbeat := context.WithCancel(ctx)
	heartbeatDone := make(chan error, 1)
	go func() {
		heartbeatDone <- heartbeatPublicBuild(heartbeatContext, cfg, lease, cancelBuild)
	}()
	heartbeatStopped := false
	stop := func() error {
		if heartbeatStopped {
			return nil
		}
		heartbeatStopped = true
		stopHeartbeat()
		return <-heartbeatDone
	}
	defer func() {
		cancelBuild()
		_ = stop()
	}()
	logSink := func(logContext context.Context, message string) error {
		body := map[string]string{"workerId": lease.WorkerID, "leaseToken": lease.Token, "message": message}
		_, _, err := callPublicBuild(logContext, cfg, http.MethodPost,
			"/v1/public-build-worker/"+url.PathEscape(lease.Build.ID)+"/logs", cfg.PublicBuildWorkerToken, body)
		return err
	}
	failLease := func(cause error) (workerOutcome, error) {
		heartbeatErr := stop()
		reason := truncateWorkerFailure(cause.Error())
		body := map[string]string{"workerId": lease.WorkerID, "leaseToken": lease.Token, "reason": reason}
		_, _, failErr := callPublicBuild(ctx, cfg, http.MethodPost,
			"/v1/public-build-worker/"+url.PathEscape(lease.Build.ID)+"/fail", cfg.PublicBuildWorkerToken, body)
		if failErr != nil {
			if heartbeatErr != nil || errors.Is(ctx.Err(), context.Canceled) {
				return workerOutcome{state: "cancelled", fields: map[string]any{"buildId": lease.Build.ID}}, nil
			}
			return workerOutcome{}, fmt.Errorf("Public Build failed (%v) and failure transition failed: %w", cause, failErr)
		}
		return workerOutcome{state: "failed", fields: map[string]any{
			"buildId": lease.Build.ID,
			"reason":  reason,
		}}, nil
	}
	_, executeErr := worker.Execute(buildContext, lease.Build, logSink)
	if executeErr != nil {
		return failLease(executeErr)
	}
	defer worker.Discard(lease.Build.ID)
	output, duration, err := worker.Collected(lease.Build.ID)
	if err != nil {
		return failLease(fmt.Errorf("collect isolated Public Build output: %w", err))
	}
	published, err := publishCollectedPublicBuild(
		buildContext,
		cfg,
		worker.Contract(),
		worker.BuilderImageDigest(),
		lease,
		output,
		duration,
		stop,
	)
	if err != nil {
		if published.Identity != "" {
			reconciled, reconcileErr := reconcileCommittedPublicBuild(ctx, cfg, lease.Build.ID, published)
			if reconciled {
				return completedPublicBuildOutcome(lease.Build.ID, published), nil
			}
			if reconcileErr != nil {
				err = errors.Join(err, reconcileErr)
			}
			retried, retryErr := publishCollectedPublicBuild(
				buildContext,
				cfg,
				worker.Contract(),
				worker.BuilderImageDigest(),
				lease,
				output,
				duration,
				nil,
			)
			if retryErr == nil {
				return completedPublicBuildOutcome(lease.Build.ID, retried), nil
			}
			err = errors.Join(err, fmt.Errorf("retry trusted publication after ambiguous response: %w", retryErr))
			if retried.Identity != "" {
				reconciled, reconcileErr = reconcileCommittedPublicBuild(ctx, cfg, lease.Build.ID, retried)
				if reconciled {
					return completedPublicBuildOutcome(lease.Build.ID, retried), nil
				}
				if reconcileErr != nil {
					err = errors.Join(err, reconcileErr)
				}
			}
		}
		return failLease(fmt.Errorf("publish isolated Public Build output: %w", err))
	}
	// The collector publish endpoint atomically stores, signs, and commits the
	// leased build. A second completion request would create an ambiguous
	// success/failure outcome if its response were lost after that commit.
	return completedPublicBuildOutcome(lease.Build.ID, published), nil
}

func completedPublicBuildOutcome(buildID string, published trustedPublicationResult) workerOutcome {
	fields := map[string]any{
		"buildId": buildID, "publicationIdentity": published.Identity,
		"nativeKey": published.NativeKey, "digest": published.Digest, "sizeBytes": published.SizeBytes,
	}
	if published.PublicImportSelector != "" {
		fields["publicImportSelector"] = published.PublicImportSelector
	}
	return workerOutcome{state: "completed", fields: fields}
}

func reconcileCommittedPublicBuild(
	ctx context.Context,
	cfg config.Config,
	buildID string,
	expected trustedPublicationResult,
) (bool, error) {
	encoded, _, err := callPublicBuild(
		ctx,
		cfg,
		http.MethodGet,
		"/v1/public-build-worker/"+url.PathEscape(buildID),
		cfg.PublicBuildWorkerToken,
		nil,
	)
	if err != nil {
		return false, fmt.Errorf("reconcile ambiguous Public Build publication: %w", err)
	}
	var status struct {
		State       string `json:"state"`
		Publication *struct {
			Identity  string `json:"publicCachePublication"`
			NativeKey string `json:"nativeKey"`
			Digest    string `json:"digest"`
			SizeBytes int64  `json:"sizeBytes"`
		} `json:"publication"`
	}
	if err := json.Unmarshal(encoded, &status); err != nil {
		return false, fmt.Errorf("decode reconciled Public Build publication: %w", err)
	}
	if status.State != string(publicbuild.StateSucceeded) {
		return false, nil
	}
	if status.Publication == nil || status.Publication.Identity != expected.Identity ||
		status.Publication.NativeKey != "" && status.Publication.NativeKey != expected.NativeKey ||
		status.Publication.Digest != expected.Digest ||
		status.Publication.SizeBytes != expected.SizeBytes {
		return false, errors.New("committed Public Build does not match the ambiguously acknowledged publication")
	}
	encoded, _, err = callPublicBuild(
		ctx,
		cfg,
		http.MethodPost,
		"/v1/public/resolve",
		"",
		expected.Coordinate,
	)
	if err != nil {
		return false, fmt.Errorf("reconcile Public Cache publication registry: %w", err)
	}
	var resolution struct {
		Envelope publictrust.Envelope `json:"envelope"`
	}
	if err := json.Unmarshal(encoded, &resolution); err != nil {
		return false, fmt.Errorf("decode reconciled Public Cache publication: %w", err)
	}
	publicKey, err := publictrust.DecodePublicKey(cfg.PublicTrustKey)
	if err != nil {
		return false, fmt.Errorf("decode Public Cache trust key for reconciliation: %w", err)
	}
	if _, err := publictrust.Verify(
		publicKey, resolution.Envelope, expected.Verification, time.Now().UTC(),
	); err != nil {
		return false, fmt.Errorf("verify reconciled Public Cache publication: %w", err)
	}
	return true, nil
}

func heartbeatPublicBuild(
	ctx context.Context,
	cfg config.Config,
	lease publicBuildWorkerLease,
	cancelBuild context.CancelFunc,
) error {
	interval := 30 * time.Second
	if remaining := time.Until(lease.ExpiresAt); remaining > 0 && remaining/3 < interval {
		interval = remaining / 3
	}
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			body := map[string]string{"workerId": lease.WorkerID, "leaseToken": lease.Token}
			_, _, err := callPublicBuild(ctx, cfg, http.MethodPost,
				"/v1/public-build-worker/"+url.PathEscape(lease.Build.ID)+"/heartbeat", cfg.PublicBuildWorkerToken, body)
			if err != nil {
				cancelBuild()
				return err
			}
		}
	}
}

func publishCollectedPublicBuild(
	ctx context.Context,
	cfg config.Config,
	contract sandbox.GuestContract,
	builderImageDigest string,
	lease publicBuildWorkerLease,
	output sandbox.CollectedOutput,
	duration time.Duration,
	beforeRequest func() error,
) (trustedPublicationResult, error) {
	build := lease.Build
	compatibility, err := publicbuild.CompatibilityIdentity(build.Request)
	if err != nil {
		return trustedPublicationResult{}, err
	}
	project, err := publicbuild.PublicationProjectIdentity(
		build.Request.Integration,
		cfg.ProjectID,
		cfg.ActionsRepository,
		build.Request.Repository,
	)
	if err != nil {
		return trustedPublicationResult{}, fmt.Errorf("derive Public Cache project identity: %w", err)
	}
	file, err := output.Open()
	if err != nil {
		return trustedPublicationResult{}, fmt.Errorf("open collected Public Build output: %w", err)
	}
	defer file.Close()
	hasher := sha256.New()
	written, err := io.Copy(hasher, &workerContextReader{ctx: ctx, reader: file})
	if err != nil {
		return trustedPublicationResult{}, fmt.Errorf("hash collected Public Build output: %w", err)
	}
	digest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	if written != output.SizeBytes || digest != output.Digest {
		return trustedPublicationResult{}, errors.New("collected Public Build output changed before publication")
	}
	payload := io.Reader(file)
	publishedDigest := output.Digest
	publishedSize := output.SizeBytes
	publishedMediaType := output.ResultMediaType
	if publishedMediaType == "" {
		publishedMediaType = output.MediaType
	}
	if build.Request.Integration == publicbuild.IntegrationBuildKit {
		ociResult, err := sandbox.PublishOCIImageLayout(ctx, output, cfg.BuildkitPublicRepository)
		if err != nil {
			return trustedPublicationResult{}, err
		}
		if ociResult.NativeKey != output.NativeKey {
			return trustedPublicationResult{}, errors.New("OCI result changed its native cache key")
		}
		payload = bytes.NewReader(ociResult.Manifest)
		publishedDigest = ociResult.Digest
		publishedSize = ociResult.SizeBytes
		publishedMediaType = ociResult.MediaType
	} else if _, err := file.Seek(0, io.SeekStart); err != nil {
		return trustedPublicationResult{}, fmt.Errorf("rewind collected Public Build output: %w", err)
	}
	if publishedMediaType == "" {
		return trustedPublicationResult{}, errors.New("collected Public Build output has no result media type")
	}
	if beforeRequest != nil {
		if err := beforeRequest(); err != nil {
			return trustedPublicationResult{}, fmt.Errorf("Public Build lease was lost before trusted publication: %w", err)
		}
	}
	expectedPublication := publictrust.Publication{
		Integration: string(build.Request.Integration), Project: project,
		Compatibility: compatibility, NativeKey: output.NativeKey,
		Repository: build.Request.Repository, Commit: build.Request.Commit,
		RecipeDigest: build.Request.RecipeDigest, Target: build.Request.Target,
		Platform: string(build.Request.Platform), Inputs: publicTrustInputs(build.Request.Inputs),
		Toolchain: contract.Toolchain, Builder: contract.Builder,
		BuilderImageDigest: builderImageDigest,
		BuildID:            build.ID, Digest: strings.TrimPrefix(publishedDigest, "sha256:"), Size: publishedSize,
	}
	expectedResult := trustedPublicationResult{
		Identity: expectedPublication.Identity(), NativeKey: output.NativeKey,
		Digest: publishedDigest, SizeBytes: publishedSize,
		Coordinate: expectedPublication.Coordinate(),
		Verification: publictrust.Expected{
			Integration: expectedPublication.Integration, Project: expectedPublication.Project,
			Compatibility: expectedPublication.Compatibility, NativeKey: expectedPublication.NativeKey,
			Repository: expectedPublication.Repository, Commit: expectedPublication.Commit,
			RecipeDigest: expectedPublication.RecipeDigest, Target: expectedPublication.Target,
			Platform: expectedPublication.Platform, Inputs: expectedPublication.Inputs,
			Toolchain: expectedPublication.Toolchain, Builder: expectedPublication.Builder,
			BuilderImageDigest: expectedPublication.BuilderImageDigest,
			BuildID:            expectedPublication.BuildID, PublicIdentity: expectedPublication.Identity(),
			Digest: publishedDigest, Size: &publishedSize,
		},
	}
	if build.Request.Integration == publicbuild.IntegrationBuildKit {
		expectedResult.PublicImportSelector = output.NativeKey + "=" + expectedResult.Identity
	}
	baseURL := localRuntimeURL(cfg.Listen)
	client := newLocalCLIHTTPClient(publishRequestTimeout)
	if cfg.PublicURL != "" {
		baseURL = strings.TrimRight(cfg.PublicURL, "/")
		client = newCLIHTTPClient(publishRequestTimeout)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/public/publish", payload)
	if err != nil {
		return trustedPublicationResult{}, err
	}
	request.ContentLength = publishedSize
	request.Header.Set("Authorization", "Bearer "+cfg.PublicCollectorToken)
	request.Header.Set("Content-Type", publishedMediaType)
	request.Header.Set("x-layercache-integration", string(build.Request.Integration))
	request.Header.Set("x-layercache-project", project)
	request.Header.Set("x-layercache-compatibility", compatibility)
	request.Header.Set("x-layercache-native-key", output.NativeKey)
	request.Header.Set("x-layercache-repository", build.Request.Repository)
	request.Header.Set("x-layercache-commit", build.Request.Commit)
	request.Header.Set("x-layercache-recipe", build.Request.RecipeDigest)
	request.Header.Set("x-layercache-platform", string(build.Request.Platform))
	request.Header.Set("x-layercache-target", build.Request.Target)
	request.Header.Set("x-layercache-toolchain", contract.Toolchain)
	request.Header.Set("x-layercache-builder", contract.Builder)
	request.Header.Set("x-layercache-builder-image-digest", builderImageDigest)
	request.Header.Set("x-layercache-build-id", build.ID)
	request.Header.Set("x-layercache-worker-id", lease.WorkerID)
	request.Header.Set("x-layercache-lease-token", lease.Token)
	request.Header.Set("x-layercache-duration", strconv.FormatInt(duration.Milliseconds(), 10))
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("trusted Public Cache publication redirected")
	}
	response, err := client.Do(request)
	if err != nil {
		return expectedResult, fmt.Errorf("publish trusted Public Build output: %w", err)
	}
	defer response.Body.Close()
	encoded, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if readErr != nil {
		return expectedResult, fmt.Errorf("read trusted Public Build publication: %w", readErr)
	}
	if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusOK {
		return expectedResult, fmt.Errorf("trusted Public Build publication returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(encoded)))
	}
	var published struct {
		Identity string `json:"identity"`
		Digest   string `json:"digest"`
		Size     int64  `json:"size"`
	}
	if err := json.Unmarshal(encoded, &published); err != nil {
		return expectedResult, fmt.Errorf("decode trusted Public Build publication: %w", err)
	}
	if published.Identity != expectedResult.Identity || published.Digest != publishedDigest || published.Size != publishedSize {
		return expectedResult, errors.New("Public Cache returned publication metadata that does not match collected bytes")
	}
	return expectedResult, nil
}

type trustedPublicationResult struct {
	Identity             string
	NativeKey            string
	PublicImportSelector string
	Digest               string
	SizeBytes            int64
	Coordinate           publictrust.CacheCoordinate
	Verification         publictrust.Expected
}

func publicTrustInputs(inputs []publicbuild.DeclaredInput) []publictrust.DeclaredInput {
	result := make([]publictrust.DeclaredInput, len(inputs))
	for index, input := range inputs {
		result[index] = publictrust.DeclaredInput{Name: input.Name, Value: input.Value}
	}
	return result
}

type workerContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader *workerContextReader) Read(buffer []byte) (int, error) {
	select {
	case <-reader.ctx.Done():
		return 0, reader.ctx.Err()
	default:
		return reader.reader.Read(buffer)
	}
}

func truncateWorkerFailure(reason string) string {
	reason = strings.TrimSpace(reason)
	if len(reason) > 2048 {
		reason = reason[:2048]
	}
	if reason == "" {
		return "isolated Public Build failed"
	}
	return reason
}

func writePublicBuildWorkerEvent(stdout io.Writer, jsonOutput bool, state string, fields map[string]any) error {
	if fields == nil {
		fields = make(map[string]any)
	}
	fields["state"] = state
	if jsonOutput {
		return json.NewEncoder(stdout).Encode(fields)
	}
	switch state {
	case "ready":
		_, err := fmt.Fprintln(stdout, "QEMU Public Build worker ready")
		return err
	case "idle":
		_, err := fmt.Fprintln(stdout, "No compatible Public Build is queued")
		return err
	case "leased":
		_, err := fmt.Fprintf(stdout, "Public Build %s leased\n", fields["buildId"])
		return err
	case "completed":
		_, err := fmt.Fprintf(stdout, "Public Build %s published as %s\n", fields["buildId"], fields["publicationIdentity"])
		return err
	case "failed":
		_, err := fmt.Fprintf(stdout, "Public Build %s failed: %s\n", fields["buildId"], fields["reason"])
		return err
	case "cancelled":
		_, err := fmt.Fprintf(stdout, "Public Build %s lease was cancelled\n", fields["buildId"])
		return err
	default:
		return fmt.Errorf("unknown Public Build worker event %q", state)
	}
}

func envOrEmpty(name string) string {
	return strings.TrimSpace(os.Getenv(name))
}

func envUint(name string) uint {
	value := envOrEmpty(name)
	if value == "" {
		return 0
	}
	parsed, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return 0
	}
	return uint(parsed)
}

func lookupWorkerTool(name string) string {
	for _, directory := range []string{"/usr/bin", "/usr/sbin"} {
		candidate := filepath.Join(directory, name)
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return candidate
		}
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return absolute
}

func qemuSystemBinary(goarch string) string {
	switch goarch {
	case "amd64":
		return "qemu-system-x86_64"
	case "arm64":
		return "qemu-system-aarch64"
	default:
		return "qemu-system-" + goarch
	}
}
