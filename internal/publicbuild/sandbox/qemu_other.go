//go:build !linux

package sandbox

import (
	"context"
	"errors"

	"github.com/layercache/layercache/internal/publicbuild"
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

func (worker *QEMUWorker) Preflight(context.Context) (PreflightReport, error) {
	return PreflightReport{}, errors.New("QEMU/KVM Public Build workers require Linux")
}

func (worker *QEMUWorker) Execute(context.Context, publicbuild.Build, publicbuild.LogSink) (publicbuild.Publication, error) {
	return publicbuild.Publication{}, errors.New("QEMU/KVM Public Build workers require Linux")
}
