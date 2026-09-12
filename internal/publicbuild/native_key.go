package publicbuild

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strings"
)

const actionsPublicIdentityVersion = "layercache/actions-cache/public-identity/v1"

// BuildKitPublicNativeKey is the stable selector for one maintained BuildKit
// Public Build result. The registry manifest digest remains the native content
// identity; this key selects its signed publication metadata.
func BuildKitPublicNativeKey(request BuildRequest) (string, error) {
	if request.Integration != IntegrationBuildKit {
		return "", reject("BuildKit native identity requires a BuildKit request")
	}
	inputs, err := canonicalDeclaredInputs(request.Inputs)
	if err != nil {
		return "", err
	}
	parts := []string{
		"https://layercache.dev/public-build/buildkit-native/v1",
		request.Repository,
		strings.ToLower(request.Commit),
		request.Target,
		strings.ToLower(request.RecipeDigest),
		string(request.Platform),
	}
	for _, input := range inputs {
		parts = append(parts, input.Name, input.Value)
	}
	digest := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

// ActionsPublicNativeKey derives the same native cache selector used by the
// Layer Cache Actions verifier. A maintained guest runner must declare the
// exact ref, key, and cache version as canonical Public Build inputs.
func ActionsPublicNativeKey(request BuildRequest, toolchain, builder string) (string, error) {
	if request.Integration != IntegrationActions {
		return "", reject("Actions native identity requires an Actions request")
	}
	inputs, err := canonicalDeclaredInputs(request.Inputs)
	if err != nil {
		return "", err
	}
	requiredNames := []string{"actions.key", "actions.ref", "actions.version", "compatibility"}
	if len(inputs) != len(requiredNames) {
		return "", reject("Actions Public Build requires exactly actions.key, actions.ref, actions.version, and compatibility")
	}
	values := make(map[string]string, len(inputs))
	for index, input := range inputs {
		if input.Name != requiredNames[index] {
			return "", reject("Actions Public Build requires exactly actions.key, actions.ref, actions.version, and compatibility")
		}
		values[input.Name] = input.Value
	}
	compatibility := values["compatibility"]
	ref := values["actions.ref"]
	key := values["actions.key"]
	version := values["actions.version"]
	if compatibility == "" || ref == "" || key == "" || version == "" || toolchain == "" || builder == "" {
		return "", reject("Actions Public Build requires compatibility, actions.ref, actions.key, actions.version, toolchain, and builder")
	}
	repository := strings.TrimPrefix(request.Repository, "https://github.com/")
	if repository == request.Repository || strings.Count(repository, "/") != 1 {
		return "", reject("Actions Public Build repository must be a canonical GitHub repository")
	}
	hasher := sha256.New()
	for _, value := range []string{
		actionsPublicIdentityVersion,
		repository,
		ref,
		key,
		version,
		compatibility,
		strings.ToLower(request.Commit),
		strings.ToLower(request.RecipeDigest),
		string(request.Platform),
		toolchain,
		builder,
	} {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		hasher.Write(length[:])
		hasher.Write([]byte(value))
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil)), nil
}
