package acceptance_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

type turboConnection struct {
	APIURL string `json:"apiUrl"`
	Token  string `json:"token"`
	Team   string `json:"team"`
}

func TestTurboGatewayPersistsOpaqueArtifactOutsideWorkspace(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.json")
	dataDir := filepath.Join(root, "host-cache")
	address := availableAddress(t)

	runLayerCache(t,
		"setup",
		"--config", configPath,
		"--data-dir", dataDir,
		"--listen", address,
		"--max-size", "10485760",
		"--non-interactive",
		"--json",
	)

	binary := buildLayerCache(t)
	var preview struct {
		APIURL            string `json:"apiUrl"`
		Token             string `json:"token"`
		Preview           bool   `json:"preview"`
		CredentialStorage string `json:"credentialStorage"`
	}
	output := runBinary(t, binary, "integration", "turbo", "--config", configPath, "--json")
	if err := json.Unmarshal(output, &preview); err != nil {
		t.Fatalf("decode Turbo connection: %v\n%s", err, output)
	}
	if preview.APIURL != "http://"+address {
		t.Fatalf("apiUrl = %q, want %q", preview.APIURL, "http://"+address)
	}
	if !preview.Preview || preview.Token != "" || preview.CredentialStorage != "none" {
		t.Fatalf("Turbo preview exposed or persisted a credential: %+v", preview)
	}

	server := startLayerCache(t, binary, configPath, address)
	connection := captureTurboConnection(t, binary, configPath)

	artifactURL := connection.APIURL + "/v8/artifacts/6f64a64d"
	request, err := http.NewRequest(http.MethodHead, artifactURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+connection.Token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("cold HEAD status = %d, want 404", response.StatusCode)
	}

	want := []byte("opaque-turbo-archive\x00with-binary-data")
	request, err = http.NewRequest(http.MethodPut, artifactURL, bytes.NewReader(want))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+connection.Token)
	request.Header.Set("Content-Length", fmt.Sprint(len(want)))
	request.Header.Set("x-artifact-duration", "2750")
	request.Header.Set("x-artifact-tag", "fixture-signature")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200", response.StatusCode)
	}

	server.stop(t)
	os.RemoveAll(filepath.Join(root, "workspace-a"))
	server = startLayerCache(t, binary, configPath, address)
	defer server.stop(t)

	request, err = http.NewRequest(http.MethodGet, artifactURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+connection.Token)
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", response.StatusCode)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("restored bytes = %q, want %q", got, want)
	}
	if response.Header.Get("x-artifact-duration") != "2750" {
		t.Fatalf("duration = %q, want 2750", response.Header.Get("x-artifact-duration"))
	}
	if response.Header.Get("x-layercache-source") != "local" {
		t.Fatalf("source = %q, want local", response.Header.Get("x-layercache-source"))
	}
}

type runningLayerCache struct {
	command *exec.Cmd
	output  bytes.Buffer
}

func buildLayerCache(t *testing.T) string {
	t.Helper()
	if candidate := os.Getenv("LAYERCACHE_ACCEPTANCE_BINARY"); candidate != "" {
		candidate, err := filepath.Abs(candidate)
		if err != nil {
			t.Fatalf("resolve LAYERCACHE_ACCEPTANCE_BINARY: %v", err)
		}
		info, err := os.Stat(candidate)
		if err != nil {
			t.Fatalf("inspect LAYERCACHE_ACCEPTANCE_BINARY %s: %v", candidate, err)
		}
		if info.IsDir() || info.Mode().Perm()&0o111 == 0 {
			t.Fatalf("LAYERCACHE_ACCEPTANCE_BINARY %s is not an executable file", candidate)
		}
		return candidate
	}
	name := "layercache"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binary := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("go", "build", "-o", binary, "./cmd/layercache")
	cmd.Dir = repoRoot(t)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build layercache: %v\n%s", err, output)
	}
	return binary
}

func startLayerCache(t *testing.T, binary, configPath, address string) *runningLayerCache {
	t.Helper()
	process := &runningLayerCache{}
	process.command = exec.Command(binary, "serve", "--config", configPath)
	process.command.Stdout = &process.output
	process.command.Stderr = &process.output
	if err := process.command.Start(); err != nil {
		t.Fatalf("start layercache: %v", err)
	}
	t.Cleanup(func() {
		if process.command.ProcessState == nil || !process.command.ProcessState.Exited() {
			_ = process.command.Process.Kill()
			_, _ = process.command.Process.Wait()
		}
	})

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		response, err := http.Get("http://" + address + "/healthz")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return process
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("layercache did not become healthy: %s", process.output.String())
	return nil
}

func (process *runningLayerCache) stop(t *testing.T) {
	t.Helper()
	if process.command.ProcessState != nil && process.command.ProcessState.Exited() {
		return
	}
	if err := process.command.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("stop layercache: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- process.command.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("layercache exited after interrupt: %v\n%s", err, process.output.String())
		}
	case <-time.After(5 * time.Second):
		_ = process.command.Process.Kill()
		t.Fatal("layercache did not stop after interrupt")
	}
}

func availableAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}
