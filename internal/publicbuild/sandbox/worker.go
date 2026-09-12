package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/layercache/layercache/internal/publicbuild"
)

type QEMUConfig struct {
	WorkerID           string
	QEMUPath           string
	QEMUImagePath      string
	Mke2fsPath         string
	DebugFSPath        string
	KernelPath         string
	KernelSHA256       string
	RootFSPath         string
	RootFSSHA256       string
	ContractPath       string
	ContractSHA256     string
	CgroupRoot         string
	WorkRoot           string
	SandboxUID         uint32
	SandboxGID         uint32
	MaxScratchBytes    int64
	SourceArchiveBytes int64
	ConnectTimeout     time.Duration
	ShutdownTimeout    time.Duration
	SourceFetcher      SourceFetcher
}

type SourceFetcher interface {
	Fetch(context.Context, string, string, string, int64) error
}

type CollectedOutput struct {
	NativeKey       string
	MediaType       string
	ResultFormat    string
	ResultMediaType string
	Digest          string
	SizeBytes       int64
	Path            string
}

func (output CollectedOutput) Open() (*os.File, error) {
	file, err := os.Open(output.Path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() != output.SizeBytes {
		file.Close()
		return nil, errors.New("collected Public Build output changed before publication")
	}
	return file, nil
}

// QEMUWorker is the production adapter at the Public Build Worker seam. It
// keeps guest execution and host collection separate: Execute returns only
// descriptors, while Collected exposes the staged bytes to a trusted host
// collector after QEMU has exited.
type QEMUWorker struct {
	config             QEMUConfig
	contract           GuestContract
	builderImageDigest string

	mu                  sync.Mutex
	results             map[string]collectedResult
	startupCleanupOnce  sync.Once
	startupCleanupError error
	scratchPrefix       string
	cgroupPrefix        string
	startupLock         *os.File
}

// Close releases the process lock which prevents two workers with the same
// identity from reclaiming each other's live scratch directories.
func (worker *QEMUWorker) Close() error {
	if worker == nil || worker.startupLock == nil {
		return nil
	}
	err := worker.startupLock.Close()
	worker.startupLock = nil
	return err
}

type collectedResult struct {
	output   CollectedOutput
	cleanup  func() error
	duration time.Duration
}

func NewQEMUWorker(config QEMUConfig) (*QEMUWorker, error) {
	if config.WorkerID == "" {
		return nil, errors.New("Public Build worker ID is required")
	}
	contract, err := LoadGuestContract(config.ContractPath, config.ContractSHA256)
	if err != nil {
		return nil, err
	}
	builderImageDigest, err := publicbuild.BuilderImageDigest(
		config.KernelSHA256,
		config.RootFSSHA256,
		config.ContractSHA256,
	)
	if err != nil {
		return nil, err
	}
	if config.SourceArchiveBytes <= 0 {
		config.SourceArchiveBytes = 1 << 30
	}
	if config.ConnectTimeout <= 0 {
		config.ConnectTimeout = 30 * time.Second
	}
	if config.ShutdownTimeout <= 0 {
		config.ShutdownTimeout = 15 * time.Second
	}
	if config.SourceFetcher == nil {
		config.SourceFetcher = &GitHubSourceFetcher{}
	}
	prefix := workerDirectoryIdentity(config.WorkerID)
	return &QEMUWorker{
		config: config, contract: contract, builderImageDigest: builderImageDigest,
		results:       make(map[string]collectedResult),
		scratchPrefix: "build-" + prefix + "-",
		cgroupPrefix:  "layercache-build-" + prefix + "-",
	}, nil
}

func workerDirectoryIdentity(workerID string) string {
	digest := sha256.Sum256([]byte(workerID))
	return hex.EncodeToString(digest[:8])
}

func (worker *QEMUWorker) ID() string {
	if worker == nil {
		return ""
	}
	return worker.config.WorkerID
}

func (worker *QEMUWorker) Capabilities() publicbuild.WorkerCapabilities {
	if worker == nil {
		return publicbuild.WorkerCapabilities{}
	}
	return worker.contract.Capabilities()
}

func (worker *QEMUWorker) Contract() GuestContract {
	if worker == nil {
		return GuestContract{}
	}
	return worker.contract
}

func (worker *QEMUWorker) BuilderImageDigest() string {
	if worker == nil {
		return ""
	}
	return worker.builderImageDigest
}

func (worker *QEMUWorker) Collected(buildID string) (CollectedOutput, time.Duration, error) {
	if worker == nil {
		return CollectedOutput{}, 0, errors.New("Public Build worker is nil")
	}
	worker.mu.Lock()
	defer worker.mu.Unlock()
	result, found := worker.results[buildID]
	if !found {
		return CollectedOutput{}, 0, errors.New("Public Build has no collected output")
	}
	return result.output, result.duration, nil
}

func (worker *QEMUWorker) Discard(buildID string) error {
	if worker == nil {
		return nil
	}
	worker.mu.Lock()
	result, found := worker.results[buildID]
	delete(worker.results, buildID)
	worker.mu.Unlock()
	if !found || result.cleanup == nil {
		return nil
	}
	return result.cleanup()
}

func (worker *QEMUWorker) remember(buildID string, result collectedResult) error {
	worker.mu.Lock()
	defer worker.mu.Unlock()
	if _, exists := worker.results[buildID]; exists {
		return fmt.Errorf("Public Build %q already has an uncollected output", buildID)
	}
	worker.results[buildID] = result
	return nil
}

func validateNativeKey(
	request publicbuild.BuildRequest,
	contract GuestContract,
	nativeKey string,
) error {
	var expected string
	var err error
	switch request.Integration {
	case publicbuild.IntegrationBuildKit:
		expected, err = publicbuild.BuildKitPublicNativeKey(request)
	case publicbuild.IntegrationActions:
		expected, err = publicbuild.ActionsPublicNativeKey(request, contract.Toolchain, contract.Builder)
	case publicbuild.IntegrationTurbo:
		return nil
	default:
		return errors.New("collected output has an unsupported integration")
	}
	if err != nil {
		return fmt.Errorf("derive collected output native cache key: %w", err)
	}
	if nativeKey != expected {
		return errors.New("guest output native cache key does not match the leased Public Build")
	}
	return nil
}

func copyExactly(destination io.Writer, source io.Reader, size int64) error {
	if size < 0 {
		return errors.New("negative byte count")
	}
	written, err := io.CopyN(destination, source, size)
	if err != nil {
		return err
	}
	if written != size {
		return io.ErrUnexpectedEOF
	}
	return nil
}

var _ publicbuild.Worker = (*QEMUWorker)(nil)
