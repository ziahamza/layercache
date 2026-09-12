package acceptance_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/layercache/layercache/internal/config"
	"github.com/layercache/layercache/internal/publicbuild"
)

const (
	publicFixtureRepository = "https://github.com/acme/widget"
	publicFixtureCommit     = "0123456789abcdef0123456789abcdef01234567"
)

func publicFixtureRecipe(integration, target string) string {
	digest, err := publicbuild.MaintainedRecipeDigest(publicbuild.Integration(integration), target)
	if err != nil {
		panic(err)
	}
	return digest
}

func acceptingGitHubAPI(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/repos/acme/widget/git/ref/heads/main" {
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"ref":    "refs/heads/main",
				"object": map[string]string{"type": "commit", "sha": "fedcba9876543210fedcba9876543210fedcba98"},
			})
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]string{"status": "ahead"})
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func requestAndLeasePublicBuild(
	t *testing.T,
	configPath string,
	integration string,
	target string,
	platform string,
	extraInputs ...string,
) (buildID string, workerID string, leaseToken string) {
	t.Helper()
	runtimeConfig, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var requested struct {
		Build struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"build"`
	}
	arguments := []string{
		"public-build", "request", "--config", configPath,
		"--repository", publicFixtureRepository,
		"--commit", publicFixtureCommit,
		"--integration", integration,
		"--target", target,
		"--recipe", publicFixtureRecipe(integration, target),
		"--platform", platform,
		"--input", "compatibility=" + runtimeConfig.CompatibilityID,
		"--cpu-millis", "1000",
		"--memory-bytes", "1073741824",
		"--disk-bytes", "2147483648",
		"--timeout", "2m",
		"--json",
	}
	for _, input := range extraInputs {
		arguments = append(arguments, "--input", input)
	}
	requestOutput := runLayerCache(t, arguments...)
	if err := json.Unmarshal(requestOutput, &requested); err != nil {
		t.Fatalf("decode Public Build fixture request: %v\n%s", err, requestOutput)
	}
	if requested.Build.ID == "" || requested.Build.State != "queued" {
		t.Fatalf("Public Build fixture request = %#v", requested)
	}

	var leased struct {
		LeaseToken string `json:"leaseToken"`
		WorkerID   string `json:"workerId"`
		Build      struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"build"`
	}
	leaseOutput := runLayerCache(t,
		"public-build", "worker", "lease", "--config", configPath,
		"--worker-id", "acceptance-"+integration+"-worker",
		"--integration", integration,
		"--platform", platform,
		"--json",
	)
	if err := json.Unmarshal(leaseOutput, &leased); err != nil {
		t.Fatalf("decode Public Build fixture lease: %v\n%s", err, leaseOutput)
	}
	if leased.LeaseToken == "" || leased.WorkerID == "" || leased.Build.ID != requested.Build.ID || leased.Build.State != "running" {
		t.Fatalf("Public Build fixture lease = %#v", leased)
	}
	return leased.Build.ID, leased.WorkerID, leased.LeaseToken
}
