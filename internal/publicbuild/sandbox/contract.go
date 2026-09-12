package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/layercache/layercache/internal/publicbuild"
)

const ProtocolV1 = "layercache.public-build/v1"

const (
	ResultFormatNativeBlob     = "native-blob"
	ResultFormatOCIImageLayout = "oci-image-layout-tar"
	OCIImageLayoutTarMediaType = "application/vnd.layercache.oci-image-layout.v1.tar"
	GuestMKFSPath              = "/sbin/mke2fs"
	GuestTurboPath             = "/opt/layercache/bin/turbo"
	GuestBuildkitdPath         = "/opt/layercache/bin/buildkitd"
	GuestBuildctlPath          = "/opt/layercache/bin/buildctl"
	GuestRuncPath              = "/opt/layercache/bin/runc"
	GuestActionsRunnerPath     = "/opt/layercache/bin/layercache-actions-public-job"
	GuestBashPath              = "/bin/bash"
	GuestShellPath             = "/bin/dash"
	ExecutorTurboCaptureV1     = publicbuild.MaintainedTurboExecutorSemantics
	ExecutorBuildKitOCIV1      = publicbuild.MaintainedBuildKitExecutorSemantics
	ExecutorActionsJobV2       = publicbuild.MaintainedActionsExecutorSemantics
)

const (
	featureReadOnlySource = "read-only-source"
	featureEphemeralDisk  = "ephemeral-work-disk"
	featureProcessLimit   = "process-limit"
	featureStreamedOutput = "streamed-output"
	featureNoNetwork      = "no-network"
	maximumContractBytes  = 1 << 20
	embeddedContractPath  = "/etc/layercache/public-build-contract.json"
)

var (
	digestPattern       = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	commitPattern       = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	guestPathPattern    = regexp.MustCompile(`^/[A-Za-z0-9._/+:-]+$`)
	namePattern         = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	identityPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/@+:-]{0,255}$`)
	legacyTargetPattern = regexp.MustCompile(`^[A-Za-z0-9@][A-Za-z0-9._/@#+:-]{0,127}$`)
)

// GuestContract is shipped next to an immutable worker image. Its digest is
// pinned independently so preflight can reject an image that does not promise
// the isolation and transport behavior required by the host worker.
type GuestContract struct {
	Protocol     string               `json:"protocol"`
	Platform     publicbuild.Platform `json:"platform"`
	Agent        string               `json:"agent"`
	Builder      string               `json:"builder"`
	Toolchain    string               `json:"toolchain"`
	ControlPort  string               `json:"controlPort"`
	MaxProcesses int64                `json:"maxProcesses"`
	Features     []string             `json:"features"`
	Recipes      []RecipeContract     `json:"recipes"`
}

func (contract GuestContract) RequiredExecutables() []string {
	paths := []string{contract.Agent}
	for _, recipe := range contract.Recipes {
		if recipe.Executor != "" {
			paths = append(paths, GuestMKFSPath)
		}
		switch recipe.Integration {
		case publicbuild.IntegrationTurbo:
			if recipe.Executor == ExecutorTurboCaptureV1 {
				paths = append(paths, GuestTurboPath)
			}
		case publicbuild.IntegrationBuildKit:
			if recipe.Executor == ExecutorBuildKitOCIV1 {
				paths = append(paths, GuestBuildkitdPath, GuestBuildctlPath, GuestRuncPath)
			}
		case publicbuild.IntegrationActions:
			if recipe.Executor == ExecutorActionsJobV2 {
				paths = append(paths, GuestActionsRunnerPath, GuestBashPath, GuestShellPath)
			}
		}
	}
	slices.Sort(paths)
	return slices.Compact(paths)
}

type RecipeContract struct {
	Integration  publicbuild.Integration `json:"integration"`
	Target       string                  `json:"target"`
	Digest       string                  `json:"digest"`
	MediaType    string                  `json:"mediaType"`
	ResultFormat string                  `json:"resultFormat,omitempty"`
	Executor     string                  `json:"executor,omitempty"`
	MaxBytes     int64                   `json:"maxBytes"`
}

func LoadGuestContract(path, expectedDigest string) (GuestContract, error) {
	if path == "" {
		return GuestContract{}, errors.New("guest contract path is required")
	}
	if !digestPattern.MatchString(expectedDigest) {
		return GuestContract{}, errors.New("guest contract SHA-256 must be a lowercase sha256 digest")
	}
	file, err := os.Open(path)
	if err != nil {
		return GuestContract{}, fmt.Errorf("open guest contract: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return GuestContract{}, fmt.Errorf("inspect guest contract: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maximumContractBytes {
		return GuestContract{}, fmt.Errorf("guest contract must be a non-empty regular file no larger than %d bytes", maximumContractBytes)
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		return GuestContract{}, fmt.Errorf("read guest contract: %w", err)
	}
	digest := sha256.Sum256(encoded)
	if "sha256:"+hex.EncodeToString(digest[:]) != expectedDigest {
		return GuestContract{}, errors.New("guest contract SHA-256 does not match the configured digest")
	}
	var contract GuestContract
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&contract); err != nil {
		return GuestContract{}, fmt.Errorf("decode guest contract: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return GuestContract{}, errors.New("guest contract contains trailing JSON")
	}
	if err := contract.Validate(); err != nil {
		return GuestContract{}, err
	}
	return contract, nil
}

func (contract GuestContract) Validate() error {
	if contract.Protocol != ProtocolV1 {
		return fmt.Errorf("guest contract protocol must be %q", ProtocolV1)
	}
	if contract.Platform != publicbuild.PlatformLinuxAMD64 && contract.Platform != publicbuild.PlatformLinuxARM64 {
		return errors.New("guest contract platform must be linux/amd64 or linux/arm64")
	}
	if !guestPathPattern.MatchString(contract.Agent) || strings.Contains(contract.Agent, "..") {
		return errors.New("guest contract agent must be a safe absolute path")
	}
	if !identityPattern.MatchString(contract.Builder) || !identityPattern.MatchString(contract.Toolchain) ||
		strings.Contains(contract.Builder, "..") || strings.Contains(contract.Toolchain, "..") {
		return errors.New("guest contract builder and toolchain must be safe non-empty names")
	}
	if !namePattern.MatchString(contract.ControlPort) {
		return errors.New("guest contract control port is invalid")
	}
	if contract.MaxProcesses < 16 || contract.MaxProcesses > 4096 {
		return errors.New("guest contract maxProcesses must be between 16 and 4096")
	}
	requiredFeatures := []string{
		featureReadOnlySource,
		featureEphemeralDisk,
		featureProcessLimit,
		featureStreamedOutput,
		featureNoNetwork,
	}
	for _, feature := range requiredFeatures {
		if !slices.Contains(contract.Features, feature) {
			return fmt.Errorf("guest contract does not declare required feature %q", feature)
		}
	}
	if len(contract.Recipes) == 0 {
		return errors.New("guest contract must declare at least one maintained recipe")
	}
	seen := make(map[string]struct{}, len(contract.Recipes))
	for _, recipe := range contract.Recipes {
		switch recipe.Integration {
		case publicbuild.IntegrationTurbo, publicbuild.IntegrationActions:
		case publicbuild.IntegrationBuildKit:
		default:
			return fmt.Errorf("guest contract recipe has unsupported integration %q", recipe.Integration)
		}
		if recipe.Executor == "" {
			if !legacyTargetPattern.MatchString(recipe.Target) || strings.Contains(recipe.Target, "..") {
				return errors.New("guest contract legacy recipe target is invalid")
			}
		} else if err := publicbuild.ValidateMaintainedTarget(recipe.Integration, recipe.Target); err != nil {
			return fmt.Errorf("guest contract recipe target is invalid: %w", err)
		}
		if !digestPattern.MatchString(recipe.Digest) {
			return errors.New("guest contract recipe digest must be a lowercase sha256 digest")
		}
		if strings.TrimSpace(recipe.MediaType) != recipe.MediaType || recipe.MediaType == "" || len(recipe.MediaType) > 255 {
			return errors.New("guest contract recipe mediaType is invalid")
		}
		if recipe.MediaType != integrationMediaType(recipe.Integration) {
			return errors.New("guest contract recipe mediaType does not match its integration")
		}
		format := recipe.ResultFormat
		if format == "" {
			format = ResultFormatNativeBlob
		}
		switch recipe.Integration {
		case publicbuild.IntegrationBuildKit:
			if format != ResultFormatOCIImageLayout {
				return errors.New("BuildKit guest recipe must return a complete OCI result as an image layout tar")
			}
		default:
			if format != ResultFormatNativeBlob {
				return errors.New("Turbo and Actions guest recipes must return native cache bytes")
			}
		}
		if recipe.Executor != "" && recipe.Executor != maintainedRecipeExecutor(recipe.Integration) {
			return errors.New("guest recipe executor does not match its integration")
		}
		if recipe.MaxBytes <= 0 {
			return errors.New("guest contract recipe maxBytes must be positive")
		}
		if recipe.Integration == publicbuild.IntegrationActions && recipe.MaxBytes > 10<<30 {
			return errors.New("Actions guest contract recipe maxBytes must not exceed 10 GiB")
		}
		identity := strings.Join([]string{string(recipe.Integration), recipe.Target, recipe.Digest}, "\x00")
		if _, duplicate := seen[identity]; duplicate {
			return errors.New("guest contract contains a duplicate recipe")
		}
		seen[identity] = struct{}{}
	}
	return nil
}

func maintainedRecipeExecutor(integration publicbuild.Integration) string {
	switch integration {
	case publicbuild.IntegrationTurbo:
		return ExecutorTurboCaptureV1
	case publicbuild.IntegrationBuildKit:
		return ExecutorBuildKitOCIV1
	case publicbuild.IntegrationActions:
		return ExecutorActionsJobV2
	default:
		return ""
	}
}

func integrationMediaType(integration publicbuild.Integration) string {
	switch integration {
	case publicbuild.IntegrationTurbo:
		return "application/vnd.layercache.turbo"
	case publicbuild.IntegrationBuildKit:
		return "application/vnd.oci.image.manifest.v1+json"
	case publicbuild.IntegrationActions:
		return "application/vnd.layercache.actions-cache"
	default:
		return ""
	}
}

func (contract GuestContract) Capabilities() publicbuild.WorkerCapabilities {
	integrations := make([]publicbuild.Integration, 0, len(contract.Recipes))
	recipes := make([]publicbuild.WorkerRecipeCapability, 0, len(contract.Recipes))
	for _, recipe := range contract.Recipes {
		if !slices.Contains(integrations, recipe.Integration) {
			integrations = append(integrations, recipe.Integration)
		}
		recipes = append(recipes, publicbuild.WorkerRecipeCapability{
			Integration: recipe.Integration, Target: recipe.Target, RecipeDigest: recipe.Digest,
		})
	}
	return publicbuild.WorkerCapabilities{
		Integrations: integrations,
		Platforms:    []publicbuild.Platform{contract.Platform},
		Recipes:      recipes,
	}
}

func (contract GuestContract) Recipe(build publicbuild.Build) (RecipeContract, error) {
	for _, recipe := range contract.Recipes {
		if recipe.Integration == build.Request.Integration && recipe.Digest == build.Request.RecipeDigest &&
			recipe.Target == build.Request.Target {
			return recipe, nil
		}
	}
	return RecipeContract{}, errors.New("the immutable guest image does not contain the leased recipe")
}

// CoversServerRecipes rejects a guest image that advertises a recipe the
// control-plane configuration does not admit. Other admitted recipes may be
// served by workers with different immutable images.
func (contract GuestContract) CoversServerRecipes(digests []string) error {
	for _, recipe := range contract.Recipes {
		if recipe.Executor != maintainedRecipeExecutor(recipe.Integration) {
			return fmt.Errorf("guest contract recipe %s/%s does not use the maintained executor", recipe.Integration, recipe.Target)
		}
		maintainedDigest, err := publicbuild.MaintainedRecipeDigest(recipe.Integration, recipe.Target)
		if err != nil || recipe.Digest != maintainedDigest {
			return fmt.Errorf("guest contract recipe %s/%s is not a maintained executable recipe", recipe.Integration, recipe.Target)
		}
		if !slices.Contains(digests, recipe.Digest) {
			return fmt.Errorf("guest contract recipe %s is not admitted by the Public Cache configuration", recipe.Digest)
		}
	}
	return nil
}
