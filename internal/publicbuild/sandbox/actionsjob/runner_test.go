//go:build linux

package actionsjob

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const (
	checkoutCommit = "cccccccccccccccccccccccccccccccccccccccc"
	cacheCommit    = "dddddddddddddddddddddddddddddddddddddddd"
)

func TestRunMaintainedJob(t *testing.T) {
	source := t.TempDir()
	output := t.TempDir()
	writeWorkflow(t, source, maintainedWorkflow("cache/", `
      - name: Build cache
        shell: sh
        run: |
          test "$GITHUB_JOB" = public-cache
          test "$GITHUB_WORKFLOW_REF" = acme/project/.github/workflows/public-cache.yml@refs/heads/main
          umask 022
          mkdir -p cache
          printf 'public-cache-payload' > cache/artifact.txt`))

	request := validRequest(source, "cache/")
	result, err := run(
		context.Background(), request, output, uint32(os.Geteuid()), uint32(os.Getegid()),
	)
	if err != nil {
		t.Fatalf("run maintained Actions job: %v", err)
	}
	if result.Compression != compressionGzip {
		t.Fatalf("compression = %q, want %q", result.Compression, compressionGzip)
	}
	if len(result.Paths) != 1 || result.Paths[0] != "cache/" {
		t.Fatalf("paths = %#v, want [cache/]", result.Paths)
	}

	members := readGzipArchive(t, result.ArchivePath)
	if got := string(members["cache/artifact.txt"]); got != "public-cache-payload" {
		t.Fatalf("cache/artifact.txt = %q, want public-cache-payload", got)
	}
	if got := gzipArchiveMode(t, result.ArchivePath, "cache/artifact.txt"); got != 0o644 {
		t.Fatalf("cache/artifact.txt archive mode = %04o, want 0644", got)
	}
}

func TestPublicArchiveIgnoresOutputModificationTimes(t *testing.T) {
	source := t.TempDir()
	cache := filepath.Join(source, "cache")
	if err := os.Mkdir(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(cache, "artifact.txt")
	if err := os.WriteFile(artifact, []byte("deterministic-public-output"), 0o600); err != nil {
		t.Fatal(err)
	}
	write := func(name string, timestamp time.Time) []byte {
		t.Helper()
		for _, path := range []string{cache, artifact} {
			if err := os.Chtimes(path, timestamp, timestamp); err != nil {
				t.Fatal(err)
			}
		}
		paths, err := collectArchivePaths(
			source,
			[]declaredPath{{version: "cache/", clean: "cache"}},
			1<<20,
		)
		if err != nil {
			t.Fatal(err)
		}
		destination := filepath.Join(t.TempDir(), name)
		if err := writeArchive(destination, source, paths, compressionGzip, 1<<20, 1<<20); err != nil {
			t.Fatal(err)
		}
		encoded, err := os.ReadFile(destination)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	first := write("first.tgz", time.Unix(1_700_000_000, 123))
	second := write("second.tgz", time.Unix(1_800_000_000, 456))
	if !bytes.Equal(first, second) {
		t.Fatal("maintained Actions archive changed only because output mtimes changed")
	}
}

func TestRunUsesGitHubBashDefaults(t *testing.T) {
	tests := []struct {
		name        string
		shell       string
		wantFailure bool
	}{
		{name: "omitted shell", wantFailure: false},
		{name: "explicit bash", shell: "\n        shell: bash", wantFailure: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := t.TempDir()
			output := t.TempDir()
			writeWorkflow(t, source, maintainedWorkflow("cache", fmt.Sprintf(`
      - name: Check pipeline behavior%s
        run: |
          false | true
          mkdir -p cache`, test.shell)))

			_, err := run(
				context.Background(), validRequest(source, "cache"), output,
				uint32(os.Geteuid()), uint32(os.Getegid()),
			)
			if test.wantFailure && err == nil {
				t.Fatal("explicit bash pipeline succeeded, want pipefail rejection")
			}
			if !test.wantFailure && err != nil {
				t.Fatalf("default bash pipeline failed: %v", err)
			}
		})
	}
}

func TestRawDeclaredPathBindsCacheVersion(t *testing.T) {
	source := t.TempDir()
	writeWorkflow(t, source, maintainedWorkflow("cache/", `
      - name: Build cache
        run: mkdir -p cache`))

	request := validRequest(source, "cache/")
	job, err := loadWorkflowJob(request)
	if err != nil {
		t.Fatalf("load workflow with exact raw-path version: %v", err)
	}
	if got := versionPaths(job.paths); len(got) != 1 || got[0] != "cache/" {
		t.Fatalf("version paths = %#v, want [cache/]", got)
	}

	request.Version = CacheVersion([]string{"cache"}, compressionGzip)
	if _, err := loadWorkflowJob(request); err == nil || !strings.Contains(err.Error(), "actions.version") {
		t.Fatalf("load workflow with normalized-path version error = %v, want actions.version rejection", err)
	}
}

func TestMaintainedTargetSelectsOneWorkflowFile(t *testing.T) {
	source := t.TempDir()
	writeWorkflow(t, source, maintainedWorkflow("cache", `
      - name: Build selected cache
        run: mkdir -p cache`))
	writeWorkflowAt(t, source, "other.yml", maintainedWorkflow("cache", `
      - name: Unsupported duplicate job
        run: echo ${{ secrets.NOT_ALLOWED }}`))

	request := validRequest(source, "cache")
	if _, err := loadWorkflowJob(request); err != nil {
		t.Fatalf("selected workflow was affected by another file: %v", err)
	}
	request.Target = ".github/workflows/other.yml#public-cache"
	if _, err := loadWorkflowJob(request); err == nil || !strings.Contains(err.Error(), "literal") {
		t.Fatalf("explicit other workflow error = %v, want unsupported expression", err)
	}
}

func TestMaintainedJobRejectsExpressionsAndUnsupportedUses(t *testing.T) {
	tests := []struct {
		name  string
		steps string
		want  string
	}{
		{
			name: "expression",
			steps: `
      - name: Build cache
        run: echo ${{ secrets.CACHE_TOKEN }}`,
			want: "literal",
		},
		{
			name: "unsupported uses",
			steps: `
      - name: Build cache
        run: mkdir -p cache
      - name: Foreign action
        uses: acme/foreign-action@eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee`,
			want: "Layer Cache",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := t.TempDir()
			writeWorkflow(t, source, maintainedWorkflow("cache", test.steps))
			if _, err := loadWorkflowJob(validRequest(source, "cache")); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("load workflow error = %v, want rejection containing %q", err, test.want)
			}
		})
	}
}

func TestCollectArchivePathsPreservesSafeSymlink(t *testing.T) {
	source := t.TempDir()
	cache := filepath.Join(source, "cache")
	if err := os.Mkdir(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "artifact.txt"), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("artifact.txt", filepath.Join(cache, "current")); err != nil {
		t.Fatal(err)
	}
	paths, err := collectArchivePaths(
		source,
		[]declaredPath{{version: "cache", clean: "cache"}},
		1<<20,
	)
	if err != nil {
		t.Fatalf("collect safe symlink: %v", err)
	}
	archivePath := filepath.Join(t.TempDir(), "archive.bin")
	if err := writeArchive(archivePath, source, paths, compressionGzip, 1<<20, 1<<20); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer compressed.Close()
	tape := tar.NewReader(compressed)
	found := false
	for {
		header, err := tape.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == "cache/current" {
			found = header.Typeflag == tar.TypeSymlink && header.Linkname == "artifact.txt"
		}
	}
	if !found {
		t.Fatal("safe Actions output symlink was not preserved")
	}

	escape := filepath.Join(cache, "escape")
	if err := os.Symlink("/etc/passwd", escape); err != nil {
		t.Fatal(err)
	}
	if _, err := collectArchivePaths(
		source,
		[]declaredPath{{version: "cache", clean: "cache"}},
		1<<20,
	); err == nil || !strings.Contains(err.Error(), "safe relative path") {
		t.Fatalf("collect escaping symlink error = %v", err)
	}
}

func TestRunKillsBackgroundProcessesBeforeArchiving(t *testing.T) {
	if _, err := os.Stat("/usr/bin/setsid"); err != nil {
		t.Skip("setsid is unavailable")
	}
	source := t.TempDir()
	output := t.TempDir()
	writeWorkflow(t, source, maintainedWorkflow("cache", `
      - name: Build cache
        shell: sh
        run: |
          setsid sh -c 'while :; do printf x >> background.log; sleep 0.01; done' >/dev/null 2>&1 &
          printf '%s' "$!" > background.pid
          mkdir -p cache
          printf payload > cache/artifact.txt`))

	_, err := run(
		context.Background(), validRequest(source, "cache"), output,
		uint32(os.Geteuid()), uint32(os.Getegid()),
	)
	pidPath := filepath.Join(source, "background.pid")
	pidBytes, readErr := os.ReadFile(pidPath)
	if err != nil {
		if pid, parseErr := parsePID(pidBytes); readErr == nil && parseErr == nil {
			_ = unix.Kill(pid, unix.SIGKILL)
		}
		t.Fatalf("run job with background process: %v", err)
	}
	if readErr != nil {
		t.Fatalf("read background pid: %v", readErr)
	}
	pid, err := parsePID(pidBytes)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for processExists(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if processExists(pid) {
		_ = unix.Kill(pid, unix.SIGKILL)
		t.Fatalf("background process %d survived the run step", pid)
	}
}

func maintainedWorkflow(path, precedingSteps string) string {
	return fmt.Sprintf(`name: Maintained Public Build
on: workflow_dispatch
jobs:
  public-cache:
    runs-on: ubuntu-24.04
    steps:
      - name: Checkout sealed source
        uses: actions/checkout@%s
        with:
          persist-credentials: false%s
      - name: Publish cache
        uses: layercache/layercache/action/cache@%s
        with:
          path: %s
          key: fixture-key
          compatibility: linux-amd64-schema1
          public-cache-mode: verified
          public-recipe-digest: sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
          public-platform: linux/amd64
          public-toolchain: actions/cache@6.2.0
          public-builder: layercache-actions-builder-v1
`, checkoutCommit, precedingSteps, cacheCommit, path)
}

func validRequest(source, path string) Request {
	return Request{
		Source: source, Target: ".github/workflows/public-cache.yml#public-cache", Platform: "linux/amd64",
		Repository:    "https://github.com/acme/project",
		Commit:        "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		RecipeDigest:  "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Compatibility: "linux-amd64-schema1", Key: "fixture-key", Ref: "refs/heads/main",
		Version:   CacheVersion([]string{path}, compressionGzip),
		Toolchain: "actions/cache@6.2.0", Builder: "layercache-actions-builder-v1",
		MaxArchiveBytes: 8 << 20, MaxExpandedBytes: 8 << 20, MaxProcesses: 64,
	}
}

func writeWorkflow(t *testing.T, source, contents string) {
	t.Helper()
	writeWorkflowAt(t, source, "public-cache.yml", contents)
}

func writeWorkflowAt(t *testing.T, source, name, contents string) {
	t.Helper()
	directory := filepath.Join(source, ".github", "workflows")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, name), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readGzipArchive(t *testing.T, path string) map[string][]byte {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer compressed.Close()
	members := make(map[string][]byte)
	archive := tar.NewReader(compressed)
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(archive)
		if err != nil {
			t.Fatal(err)
		}
		members[header.Name] = body
	}
	return members
}

func gzipArchiveMode(t *testing.T, path, name string) int64 {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer compressed.Close()
	archive := tar.NewReader(compressed)
	for {
		header, err := archive.Next()
		if err == io.EOF {
			t.Fatalf("archive member %s was not found", name)
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == name {
			return header.Mode
		}
	}
}

func parsePID(encoded []byte) (int, error) {
	value := strings.TrimSpace(string(encoded))
	pid, err := strconv.Atoi(value)
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("invalid background pid %q", value)
	}
	return pid, nil
}

func processExists(pid int) bool {
	err := unix.Kill(pid, 0)
	return err == nil || err == unix.EPERM
}
