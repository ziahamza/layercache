package acceptance_test

import (
	"bytes"
	"net/http"
	"path/filepath"
	"testing"
)

func TestLocalCacheEvictsLeastRecentlyUsedEntry(t *testing.T) {
	root := t.TempDir()
	binary := buildLayerCache(t)
	address := availableAddress(t)
	configPath := filepath.Join(root, "config.json")
	runLayerCache(t,
		"setup", "--config", configPath,
		"--data-dir", filepath.Join(root, "cache"),
		"--listen", address,
		"--max-size", "10",
		"--non-interactive", "--json",
	)
	server := startLayerCache(t, binary, configPath, address)
	defer server.stop(t)
	connection := captureTurboConnection(t, binary, configPath)

	putTurboArtifact(t, connection, "artifact-a", []byte("aaaa"))
	putTurboArtifact(t, connection, "artifact-b", []byte("bbbb"))
	if got, _ := fetchTurboArtifact(t, connection.APIURL+"/v8/artifacts/artifact-a", connection.Token); !bytes.Equal(got, []byte("aaaa")) {
		t.Fatalf("touch A returned %q", got)
	}
	putTurboArtifact(t, connection, "artifact-c", []byte("cccccc"))

	assertTurboStatus(t, connection, "artifact-a", http.StatusOK)
	assertTurboStatus(t, connection, "artifact-b", http.StatusNotFound)
	assertTurboStatus(t, connection, "artifact-c", http.StatusOK)
}

func putTurboArtifact(t *testing.T, connection turboConnection, hash string, body []byte) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPut, connection.APIURL+"/v8/artifacts/"+hash, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+connection.Token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("PUT %s status = %d, want 200", hash, response.StatusCode)
	}
}

func assertTurboStatus(t *testing.T, connection turboConnection, hash string, want int) {
	t.Helper()
	request, err := http.NewRequest(http.MethodHead, connection.APIURL+"/v8/artifacts/"+hash, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+connection.Token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != want {
		t.Fatalf("HEAD %s status = %d, want %d", hash, response.StatusCode, want)
	}
}
