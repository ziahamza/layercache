package buildkit_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/layercache/layercache/internal/buildkit"
)

func TestPlanBuildUsesPlatformScopedNativeCaches(t *testing.T) {
	t.Parallel()

	adapter, err := buildkit.New(buildkit.Config{
		DockerCommand:  "docker",
		BuilderName:    "layercache",
		TeamRepository: "cache.example/team/acme/widget",
		MaxTeamImports: 2,
	})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}

	publicDigest := "sha256:" + strings.Repeat("a", 64)
	plan, err := adapter.Plan(buildkit.BuildRequest{
		ContextPath:    ".",
		TargetPlatform: "linux/amd64",
		TeamImportTags: []string{"feature", "main"},
		TeamExportID:   "01k5abc123-u0123456789abcdef0123456789abcdef",
		PublicImports: []buildkit.PublicCache{
			{
				Repository:     "cache.example/public/widget",
				Digest:         publicDigest,
				PublicIdentity: strings.Repeat("b", 64),
			},
		},
		Output:    buildkit.OutputPush,
		ExtraArgs: []string{"--tag", "registry.example/widget:commit"},
	})
	if err != nil {
		t.Fatalf("plan build: %v", err)
	}

	want := buildkit.Command{
		Path: "docker",
		Args: []string{
			"buildx", "build",
			"--builder", "layercache",
			"--platform", "linux/amd64",
			"--progress=rawjson",
			"--cache-from", "type=registry,ref=cache.example/team/acme/widget/linux-amd64:feature",
			"--cache-from", "type=registry,ref=cache.example/team/acme/widget/linux-amd64:main",
			"--cache-from", "type=registry,ref=cache.example/public/widget/linux-amd64@" + publicDigest,
			"--cache-to", "type=registry,ref=cache.example/team/acme/widget/linux-amd64:build-01k5abc123-u0123456789abcdef0123456789abcdef,mode=max,oci-mediatypes=true,image-manifest=true,ignore-error=true",
			"--push",
			"--tag", "registry.example/widget:commit",
			".",
		},
	}
	if !reflect.DeepEqual(plan.Command, want) {
		t.Fatalf("unexpected build command\n got: %#v\nwant: %#v", plan.Command, want)
	}
}

func TestPlanAddsSerializedPromotionAfterUniqueTeamExport(t *testing.T) {
	t.Parallel()

	adapter, err := buildkit.New(buildkit.Config{
		BuilderName:    "layercache",
		TeamRepository: "cache.example/team/acme/widget",
	})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	exportID := "run-123-u0123456789abcdef0123456789abcdef"
	plan, err := adapter.Plan(buildkit.BuildRequest{
		ContextPath: ".", TargetPlatform: "linux/arm64", Output: buildkit.OutputPush,
		TeamExportID: exportID, TeamPromoteTag: "feature-branch",
	})
	if err != nil {
		t.Fatalf("plan build: %v", err)
	}
	if plan.Promotion == nil {
		t.Fatal("plan did not include Team Cache promotion")
	}
	want := buildkit.Command{Path: "docker", Args: []string{
		"buildx", "imagetools", "create", "--builder", "layercache",
		"--prefer-index=false", "--progress=plain", "--tag",
		"cache.example/team/acme/widget/linux-arm64:feature-branch",
		"cache.example/team/acme/widget/linux-arm64:build-" + exportID,
	}}
	if !reflect.DeepEqual(*plan.Promotion, want) {
		t.Fatalf("promotion command\n got: %#v\nwant: %#v", *plan.Promotion, want)
	}
}

func TestPlanRejectsPublicCacheWithoutSHA256Digest(t *testing.T) {
	t.Parallel()

	adapter, err := buildkit.New(buildkit.Config{BuilderName: "layercache"})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}

	_, err = adapter.Plan(buildkit.BuildRequest{
		ContextPath:    ".",
		TargetPlatform: "linux/amd64",
		PublicImports: []buildkit.PublicCache{
			{Repository: "cache.example/public/widget", Digest: "latest", PublicIdentity: strings.Repeat("b", 64)},
		},
		Output: buildkit.OutputLoad,
	})
	if err == nil || !strings.Contains(err.Error(), "SHA-256 digest") {
		t.Fatalf("expected a digest-pinning error, got %v", err)
	}
}

func TestPlanRejectsPublicCacheWithoutSignedPublicationIdentity(t *testing.T) {
	t.Parallel()

	adapter, err := buildkit.New(buildkit.Config{BuilderName: "layercache"})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	_, err = adapter.Plan(buildkit.BuildRequest{
		ContextPath: ".", TargetPlatform: "linux/amd64", Output: buildkit.OutputLoad,
		PublicImports: []buildkit.PublicCache{{
			Repository: "cache.example/public/widget", Digest: "sha256:" + strings.Repeat("a", 64),
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "signed publication identity") {
		t.Fatalf("expected a signed-publication identity error, got %v", err)
	}
}

func TestPlanRejectsPromotionWithoutImmutableExport(t *testing.T) {
	t.Parallel()

	adapter, err := buildkit.New(buildkit.Config{
		BuilderName: "layercache", TeamRepository: "cache.example/team/acme/widget",
	})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	_, err = adapter.Plan(buildkit.BuildRequest{
		ContextPath: ".", TargetPlatform: "linux/amd64", Output: buildkit.OutputLoad,
		TeamPromoteTag: "main",
	})
	if err == nil || !strings.Contains(err.Error(), "requires an immutable") {
		t.Fatalf("expected promotion-without-export error, got %v", err)
	}
}

func TestPlanRejectsMoreThanTheBoundedTeamImports(t *testing.T) {
	t.Parallel()

	adapter, err := buildkit.New(buildkit.Config{
		BuilderName:    "layercache",
		TeamRepository: "cache.example/team/acme/widget",
		MaxTeamImports: 2,
	})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}

	_, err = adapter.Plan(buildkit.BuildRequest{
		ContextPath:    ".",
		TargetPlatform: "linux/amd64",
		TeamImportTags: []string{"branch", "main", "fallback"},
		Output:         buildkit.OutputLoad,
	})
	if err == nil || !strings.Contains(err.Error(), "at most 2 Team Cache imports") {
		t.Fatalf("expected a bounded-import error, got %v", err)
	}
}

func TestPlanRejectsCallerOverridesOfManagedCacheFlags(t *testing.T) {
	t.Parallel()

	adapter, err := buildkit.New(buildkit.Config{BuilderName: "layercache"})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}

	for _, managedFlag := range []string{
		"--builder",
		"--platform=linux/arm64",
		"--progress",
		"--cache-from=type=registry,ref=untrusted.example/cache:latest",
		"--cache-to",
		"--push",
		"--load",
		"--output=type=local,dest=out",
		"-o",
	} {
		t.Run(managedFlag, func(t *testing.T) {
			_, err := adapter.Plan(buildkit.BuildRequest{
				ContextPath:    ".",
				TargetPlatform: "linux/amd64",
				Output:         buildkit.OutputLoad,
				ExtraArgs:      []string{managedFlag},
			})
			if err == nil || !strings.Contains(err.Error(), "managed buildx flag") {
				t.Fatalf("expected managed-flag error for %q, got %v", managedFlag, err)
			}
		})
	}
}

func TestPlanRequiresARepositoryForTeamCacheReferences(t *testing.T) {
	t.Parallel()

	adapter, err := buildkit.New(buildkit.Config{BuilderName: "layercache"})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}

	_, err = adapter.Plan(buildkit.BuildRequest{
		ContextPath:    ".",
		TargetPlatform: "linux/amd64",
		TeamImportTags: []string{"main"},
		Output:         buildkit.OutputLoad,
	})
	if err == nil || !strings.Contains(err.Error(), "Team Cache repository") {
		t.Fatalf("expected missing Team Cache repository error, got %v", err)
	}
}

func TestPlanRejectsUnsafeTeamCacheReferenceParts(t *testing.T) {
	t.Parallel()

	adapter, err := buildkit.New(buildkit.Config{
		BuilderName:    "layercache",
		TeamRepository: "cache.example/team/acme/widget",
	})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}

	tests := []buildkit.BuildRequest{
		{
			ContextPath:    ".",
			TargetPlatform: "linux/amd64",
			TeamImportTags: []string{"main,mode=max"},
			Output:         buildkit.OutputLoad,
		},
		{
			ContextPath:    ".",
			TargetPlatform: "linux/amd64",
			TeamExportID:   "shared,ignore-error=false-u0123456789abcdef0123456789abcdef",
			Output:         buildkit.OutputLoad,
		},
	}
	for _, request := range tests {
		if _, err := adapter.Plan(request); err == nil || !strings.Contains(err.Error(), "safe OCI tag") {
			t.Fatalf("expected unsafe Team Cache reference to fail, got %v", err)
		}
	}
}

func TestPlanRejectsUnsafeTargetPlatform(t *testing.T) {
	t.Parallel()

	adapter, err := buildkit.New(buildkit.Config{BuilderName: "layercache"})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}

	_, err = adapter.Plan(buildkit.BuildRequest{
		ContextPath:    ".",
		TargetPlatform: "linux/amd64,ref=untrusted",
		Output:         buildkit.OutputLoad,
	})
	if err == nil || !strings.Contains(err.Error(), "target platform") {
		t.Fatalf("expected unsafe target platform error, got %v", err)
	}
}

func TestPlanRejectsCacheRepositoriesThatInjectCSVOptions(t *testing.T) {
	t.Parallel()

	if _, err := buildkit.New(buildkit.Config{
		BuilderName:    "layercache",
		TeamRepository: "cache.example/team/widget,mode=min",
	}); err == nil || !strings.Contains(err.Error(), "Team Cache repository") {
		t.Fatalf("expected unsafe Team Cache repository error, got %v", err)
	}

	adapter, err := buildkit.New(buildkit.Config{BuilderName: "layercache"})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	_, err = adapter.Plan(buildkit.BuildRequest{
		ContextPath:    ".",
		TargetPlatform: "linux/amd64",
		PublicImports: []buildkit.PublicCache{
			{
				Repository:     "cache.example/public/widget,mode=min",
				Digest:         "sha256:" + strings.Repeat("a", 64),
				PublicIdentity: strings.Repeat("b", 64),
			},
		},
		Output: buildkit.OutputLoad,
	})
	if err == nil || !strings.Contains(err.Error(), "Public Cache repository") {
		t.Fatalf("expected unsafe Public Cache repository error, got %v", err)
	}
}
