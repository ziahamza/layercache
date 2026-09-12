package publicbuild

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

const builderImageManifestVersion = "https://layercache.dev/public-build/builder-image/v1"

// BuilderImageDigest identifies the complete immutable guest image reviewed by
// the trusted host. The labels make the manifest ordering explicit so the
// digest cannot be confused with a digest over an arbitrary concatenation.
func BuilderImageDigest(kernelSHA256, rootFSSHA256, contractSHA256 string) (string, error) {
	components := []struct {
		name   string
		digest string
	}{
		{name: "kernel", digest: kernelSHA256},
		{name: "rootfs", digest: rootFSSHA256},
		{name: "contract", digest: contractSHA256},
	}
	parts := []string{builderImageManifestVersion}
	for _, component := range components {
		if !validBuilderImageComponentDigest(component.digest) {
			return "", errors.New("Public Build " + component.name + " digest must be a lowercase sha256 digest")
		}
		parts = append(parts, component.name, component.digest)
	}
	digest := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func ValidateBuilderImageDigest(value string) error {
	if !validBuilderImageComponentDigest(value) {
		return errors.New("builder image digest must be a lowercase sha256 digest")
	}
	return nil
}

func validBuilderImageComponentDigest(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	encoded := strings.TrimPrefix(value, "sha256:")
	if encoded != strings.ToLower(encoded) {
		return false
	}
	_, err := hex.DecodeString(encoded)
	return err == nil
}
