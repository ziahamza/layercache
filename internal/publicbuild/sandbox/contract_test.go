package sandbox

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/layercache/layercache/internal/publicbuild"
)

func TestGuestContractRequiresIsolationFeaturesAndCoversAdvertisedRecipes(t *testing.T) {
	t.Parallel()
	recipe, err := publicbuild.MaintainedRecipeDigest(publicbuild.IntegrationTurbo, "@acme/widgets#build")
	if err != nil {
		t.Fatal(err)
	}
	contract := testGuestContract(recipe)
	path := filepath.Join(t.TempDir(), "contract.json")
	encoded, err := json.Marshal(contract)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	loaded, err := LoadGuestContract(path, "sha256:"+hex.EncodeToString(digest[:]))
	if err != nil {
		t.Fatal(err)
	}
	otherRecipe := "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	if err := loaded.CoversServerRecipes([]string{recipe, otherRecipe}); err != nil {
		t.Fatalf("worker image was incorrectly required to contain every globally admitted recipe: %v", err)
	}
	loaded.Features = loaded.Features[:len(loaded.Features)-1]
	if err := loaded.Validate(); err == nil {
		t.Fatal("contract without no-network feature was accepted")
	}
}

func TestGuestContractRequiresCompleteOCIResultForBuildKit(t *testing.T) {
	t.Parallel()
	contract := testGuestContract("sha256:" + strings.Repeat("c", 64))
	contract.Recipes[0].Integration = publicbuild.IntegrationBuildKit
	contract.Recipes[0].MediaType = integrationMediaType(publicbuild.IntegrationBuildKit)
	contract.Recipes[0].Executor = ExecutorBuildKitOCIV1
	contract.Recipes[0].ResultFormat = ResultFormatNativeBlob
	if err := contract.Validate(); err == nil || !strings.Contains(err.Error(), "complete OCI result") {
		t.Fatalf("BuildKit guest contract error = %v", err)
	}
	contract.Recipes[0].ResultFormat = ResultFormatOCIImageLayout
	if err := contract.Validate(); err != nil {
		t.Fatalf("complete BuildKit OCI result was rejected: %v", err)
	}
}

func TestGuestContractCapsActionsArchiveAtClientLimit(t *testing.T) {
	t.Parallel()
	const target = ".github/workflows/public-cache.yml#public-cache"
	recipe, err := publicbuild.MaintainedRecipeDigest(publicbuild.IntegrationActions, target)
	if err != nil {
		t.Fatal(err)
	}
	contract := testGuestContract(recipe)
	contract.Recipes[0] = RecipeContract{
		Integration: publicbuild.IntegrationActions,
		Target:      target,
		Digest:      recipe,
		MediaType:   integrationMediaType(publicbuild.IntegrationActions),
		Executor:    ExecutorActionsJobV2,
		MaxBytes:    10<<30 + 1,
	}
	if err := contract.Validate(); err == nil || !strings.Contains(err.Error(), "10 GiB") {
		t.Fatalf("oversized Actions guest contract error = %v", err)
	}
	contract.Recipes[0].MaxBytes = 10 << 30
	if err := contract.Validate(); err != nil {
		t.Fatalf("Actions guest contract at client limit was rejected: %v", err)
	}
}

func TestSourceExtractionRejectsTraversalAndLinks(t *testing.T) {
	t.Parallel()
	for name, header := range map[string]tar.Header{
		"traversal": {Name: "repo/../../escape", Typeflag: tar.TypeReg, Size: 1},
		"symlink":   {Name: "repo/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
	} {
		t.Run(name, func(t *testing.T) {
			var archive bytes.Buffer
			writer := tar.NewWriter(&archive)
			if err := writer.WriteHeader(&header); err != nil {
				t.Fatal(err)
			}
			if header.Size > 0 {
				_, _ = writer.Write([]byte("x"))
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if err := extractSourceTar(filepath.Join(t.TempDir(), "source"), bytes.NewReader(archive.Bytes())); err == nil {
				t.Fatal("unsafe source archive was accepted")
			}
		})
	}
}

func TestSourceExtractionPreservesRelativeSymlinkInsideRepository(t *testing.T) {
	t.Parallel()
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	for _, header := range []tar.Header{
		{Name: "repo", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "repo/pkg", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "repo/pkg/data.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4},
	} {
		if err := writer.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		if header.Size != 0 {
			_, _ = writer.Write([]byte("data"))
		}
	}
	if err := writer.WriteHeader(&tar.Header{
		Name: "repo/link.txt", Typeflag: tar.TypeSymlink, Linkname: "pkg/data.txt", Mode: 0o777,
	}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "source")
	if err := extractSourceTar(destination, bytes.NewReader(archive.Bytes())); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(filepath.Join(destination, "pkg"), 0o700)
		_ = os.Chmod(destination, 0o700)
	})
	if target, err := os.Readlink(filepath.Join(destination, "link.txt")); err != nil || target != "pkg/data.txt" {
		t.Fatalf("source symlink target = %q, %v", target, err)
	}
	if contents, err := os.ReadFile(filepath.Join(destination, "link.txt")); err != nil || string(contents) != "data" {
		t.Fatalf("source symlink contents = %q, %v", contents, err)
	}
}

func TestSourceExtractionCountsImplicitDirectoriesAgainstInodeBudget(t *testing.T) {
	t.Parallel()
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(&tar.Header{Name: "repo", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteHeader(&tar.Header{Name: "repo/a/b/payload", Typeflag: 0, Mode: 0o644}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	err := extractSourceTarWithInodeLimit(filepath.Join(t.TempDir(), "source"), bytes.NewReader(archive.Bytes()), 2)
	if err == nil || !strings.Contains(err.Error(), "inode budget") {
		t.Fatalf("inode exhaustion error = %v", err)
	}
}

func testGuestContract(recipe string) GuestContract {
	return GuestContract{
		Protocol: ProtocolV1, Platform: publicbuild.PlatformLinuxAMD64,
		Agent:   "/sbin/layercache-public-build-guest",
		Builder: "layercache-qemu-v1", Toolchain: "linux-amd64-glibc",
		ControlPort: "org.layercache.public-build.1", MaxProcesses: 256,
		Features: []string{
			featureReadOnlySource, featureEphemeralDisk, featureProcessLimit,
			featureStreamedOutput, featureNoNetwork,
		},
		Recipes: []RecipeContract{{
			Integration: publicbuild.IntegrationTurbo, Target: "@acme/widgets#build", Digest: recipe,
			MediaType: integrationMediaType(publicbuild.IntegrationTurbo), Executor: ExecutorTurboCaptureV1,
			MaxBytes: 1 << 20,
		}},
	}
}
