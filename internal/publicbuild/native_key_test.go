package publicbuild_test

import (
	"strings"
	"testing"

	"github.com/layercache/layercache/internal/publicbuild"
)

func TestBuildKitPublicNativeKeyIsCanonicalAndBindsInputs(t *testing.T) {
	t.Parallel()
	request := publicbuild.BuildRequest{
		Repository: "https://github.com/acme/widget", Commit: strings.Repeat("a", 40),
		Integration: publicbuild.IntegrationBuildKit, Target: "release",
		RecipeDigest: "sha256:" + strings.Repeat("b", 64), Platform: publicbuild.PlatformLinuxAMD64,
		Inputs: []publicbuild.DeclaredInput{{Name: "z", Value: "last"}, {Name: "a", Value: "first"}},
	}
	first, err := publicbuild.BuildKitPublicNativeKey(request)
	if err != nil {
		t.Fatal(err)
	}
	request.Inputs[0], request.Inputs[1] = request.Inputs[1], request.Inputs[0]
	second, err := publicbuild.BuildKitPublicNativeKey(request)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || !strings.HasPrefix(first, "sha256:") {
		t.Fatalf("canonical identities = %q and %q", first, second)
	}
	request.Inputs[0].Value = "changed"
	changed, err := publicbuild.BuildKitPublicNativeKey(request)
	if err != nil {
		t.Fatal(err)
	}
	if changed == first {
		t.Fatal("BuildKit native identity did not bind declared inputs")
	}
}

func TestActionsPublicNativeKeyRequiresCompleteNativeIdentity(t *testing.T) {
	t.Parallel()
	request := publicbuild.BuildRequest{
		Repository: "https://github.com/acme/widget", Commit: strings.Repeat("a", 40),
		Integration: publicbuild.IntegrationActions, Target: ".github/workflows/public-cache.yml#build-job",
		RecipeDigest: "sha256:" + strings.Repeat("b", 64), Platform: publicbuild.PlatformLinuxAMD64,
		Inputs: []publicbuild.DeclaredInput{
			{Name: "compatibility", Value: "linux-amd64-node24"},
			{Name: "actions.ref", Value: "refs/heads/main"},
			{Name: "actions.key", Value: "pnpm-lock"},
			{Name: "actions.version", Value: "paths-v1"},
		},
	}
	key, err := publicbuild.ActionsPublicNativeKey(request, "actions/cache@6.2.0", "layercache-actions-v1")
	if err != nil || !strings.HasPrefix(key, "sha256:") {
		t.Fatalf("Actions native identity = %q, %v", key, err)
	}
	request.Inputs = request.Inputs[:3]
	if _, err := publicbuild.ActionsPublicNativeKey(request, "actions/cache@6.2.0", "layercache-actions-v1"); err == nil {
		t.Fatal("incomplete Actions native identity was accepted")
	}
}
