package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/layercache/layercache/internal/publicbuild"
)

func TestSealedWorkflowUsesItsNativePlatformAndCompatibility(t *testing.T) {
	for _, platform := range []publicbuild.Platform{publicbuild.PlatformLinuxAMD64, publicbuild.PlatformLinuxARM64} {
		t.Run(string(platform), func(t *testing.T) {
			root := t.TempDir()
			fetcher := sourceFetcher{recipeDigest: "sha256:" + strings.Repeat("b", 64), platform: platform}
			if err := fetcher.Fetch(context.Background(), "https://github.com/acme/project", strings.Repeat("a", 40), root, 1<<20); err != nil {
				t.Fatal(err)
			}
			workflow, err := os.ReadFile(filepath.Join(root, ".github/workflows/public-cache.yml"))
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"public-platform: " + string(platform), "compatibility: " + actionsCompatibility(platform)} {
				if !strings.Contains(string(workflow), want) {
					t.Fatalf("sealed workflow does not contain %s", want)
				}
			}
			wantRunner := "runs-on: ubuntu-24.04\n"
			if platform == publicbuild.PlatformLinuxARM64 {
				wantRunner = "runs-on: ubuntu-24.04-arm\n"
			}
			if !strings.Contains(string(workflow), wantRunner) {
				t.Fatalf("sealed workflow runner would be rejected for %s", platform)
			}
		})
	}
}
