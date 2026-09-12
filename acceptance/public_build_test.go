package acceptance_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type publicBuildView struct {
	ID      string `json:"id"`
	State   string `json:"state"`
	Failure string `json:"failure"`
	Request struct {
		Inputs []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"inputs"`
	} `json:"request"`
	Publication *struct {
		PublicCachePublication string `json:"publicCachePublication"`
	} `json:"publication"`
}

type publicBuildRequestResult struct {
	Build  publicBuildView `json:"build"`
	Reused bool            `json:"reused"`
}

type publicBuildLeaseResult struct {
	LeaseToken string          `json:"leaseToken"`
	WorkerID   string          `json:"workerId"`
	Build      publicBuildView `json:"build"`
}

func TestPublicBuildControlPlaneCompletesPersistsAndFencesClients(t *testing.T) {
	const (
		compileTarget = "@acme/widget#compile"
		cancelTarget  = "@acme/widget#cancel-me"
		failTarget    = "@acme/widget#fail-me"
	)
	root := t.TempDir()
	binary := buildLayerCache(t)
	address := availableAddress(t)
	configPath := filepath.Join(root, "public.json")
	githubAPI := acceptingGitHubAPI(t)
	compileRecipe := publicFixtureRecipe("turbo", compileTarget)
	cancelRecipe := publicFixtureRecipe("turbo", cancelTarget)
	failRecipe := publicFixtureRecipe("turbo", failTarget)
	runBinary(t, binary,
		"setup", "--config", configPath,
		"--data-dir", filepath.Join(root, "public-cache"),
		"--listen", address,
		"--role", "public",
		"--project", "github.com/acme/widget",
		"--publisher-token", "publisher-secret",
		"--github-api-url", githubAPI,
		"--public-build-repository", publicFixtureRepository,
		"--public-build-approved-ref", "refs/heads/main",
		"--public-build-recipe", compileRecipe,
		"--public-build-recipe", cancelRecipe,
		"--public-build-recipe", failRecipe,
		"--max-size", "10485760",
		"--non-interactive", "--json",
	)
	var credentials struct {
		LocalToken string `json:"localToken"`
	}
	configuration, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(configuration, &credentials); err != nil {
		t.Fatal(err)
	}
	server := startLayerCache(t, binary, configPath, address)

	requestArgs := []string{
		"public-build", "request", "--config", configPath,
		"--repository", publicFixtureRepository,
		"--commit", publicFixtureCommit,
		"--integration", "turbo",
		"--target", compileTarget,
		"--recipe", compileRecipe,
		"--input", "compatibility=linux-amd64-acceptance-v1",
		"--input", "feature=enabled",
		"--input", "runtime=node@24",
		"--platform", "linux/amd64",
		"--cpu-millis", "1000",
		"--memory-bytes", "1073741824",
		"--disk-bytes", "2147483648",
		"--timeout", "2m",
		"--json",
	}
	var requested publicBuildRequestResult
	if output := runBinary(t, binary, requestArgs...); json.Unmarshal(output, &requested) != nil {
		t.Fatalf("decode Public Build request: %s", output)
	}
	if requested.Reused || requested.Build.ID == "" || requested.Build.State != "queued" {
		t.Fatalf("request result = %#v", requested)
	}
	if len(requested.Build.Request.Inputs) != 3 || requested.Build.Request.Inputs[0].Name != "compatibility" ||
		requested.Build.Request.Inputs[1].Name != "feature" || requested.Build.Request.Inputs[2].Name != "runtime" {
		t.Fatalf("canonical declared inputs = %#v", requested.Build.Request.Inputs)
	}

	var duplicate publicBuildRequestResult
	if output := runBinary(t, binary, requestArgs...); json.Unmarshal(output, &duplicate) != nil {
		t.Fatalf("decode duplicate Public Build request: %s", output)
	}
	if !duplicate.Reused || duplicate.Build.ID != requested.Build.ID {
		t.Fatalf("duplicate request = %#v", duplicate)
	}

	var lease publicBuildLeaseResult
	leaseOutput := runBinary(t, binary,
		"public-build", "worker", "lease", "--config", configPath,
		"--worker-id", "worker-linux-amd64",
		"--integration", "turbo", "--platform", "linux/amd64", "--json",
	)
	if err := json.Unmarshal(leaseOutput, &lease); err != nil {
		t.Fatalf("decode Public Build lease: %v\n%s", err, leaseOutput)
	}
	if lease.LeaseToken == "" || lease.Build.ID != requested.Build.ID || lease.Build.State != "running" {
		t.Fatalf("lease result = %#v", lease)
	}
	runBinary(t, binary,
		"public-build", "worker", "append-log", "--config", configPath,
		"--id", requested.Build.ID, "--worker-id", lease.WorkerID,
		"--lease-token", lease.LeaseToken,
		"--message", "in /workspace/project authenticated with publisher-secret", "--json",
	)

	artifactFile := filepath.Join(root, "public-artifact.bin")
	if err := os.WriteFile(artifactFile, []byte("trusted-public-build-output"), 0o600); err != nil {
		t.Fatal(err)
	}
	var published struct {
		Identity string `json:"identity"`
	}
	publishOutput := runBinary(t, binary,
		"public", "publish", "--config", configPath,
		"--file", artifactFile,
		"--hash", "public-build-output",
		"--repository", publicFixtureRepository,
		"--commit", publicFixtureCommit,
		"--recipe", compileRecipe,
		"--target", compileTarget,
		"--platform", "linux/amd64",
		"--compatibility", "linux-amd64-acceptance-v1",
		"--toolchain", "turbo@2.10.9",
		"--builder", "layercache-worker@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		"--builder-image-digest", "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
		"--build-id", requested.Build.ID,
		"--worker-id", lease.WorkerID,
		"--lease-token", lease.LeaseToken,
		"--duration", "2300", "--json",
	)
	if err := json.Unmarshal(publishOutput, &published); err != nil || published.Identity == "" {
		t.Fatalf("decode Public Cache publication: %v\n%s", err, publishOutput)
	}

	var completed publicBuildView
	completeOutput := runBinary(t, binary,
		"public-build", "worker", "complete", "--config", configPath,
		"--id", requested.Build.ID, "--worker-id", lease.WorkerID,
		"--lease-token", lease.LeaseToken,
		"--publication-identity", published.Identity, "--json",
	)
	if err := json.Unmarshal(completeOutput, &completed); err != nil {
		t.Fatalf("decode Public Build completion: %v\n%s", err, completeOutput)
	}
	if completed.State != "succeeded" || completed.Publication == nil || completed.Publication.PublicCachePublication != published.Identity {
		t.Fatalf("completed build = %#v", completed)
	}

	server.stop(t)
	server = startLayerCache(t, binary, configPath, address)
	defer server.stop(t)
	var persisted publicBuildView
	statusOutput := runBinary(t, binary,
		"public-build", "status", "--config", configPath,
		"--id", requested.Build.ID, "--json",
	)
	if err := json.Unmarshal(statusOutput, &persisted); err != nil {
		t.Fatalf("decode persisted Public Build: %v\n%s", err, statusOutput)
	}
	if persisted.State != "succeeded" || persisted.Publication == nil || persisted.Publication.PublicCachePublication != published.Identity {
		t.Fatalf("persisted build = %#v", persisted)
	}
	if len(persisted.Request.Inputs) != 3 || persisted.Request.Inputs[2].Value != "node@24" {
		t.Fatalf("persisted declared inputs = %#v", persisted.Request.Inputs)
	}
	var logs struct {
		Logs []struct {
			Message string `json:"message"`
		} `json:"logs"`
	}
	logsOutput := runBinary(t, binary,
		"public-build", "logs", "--config", configPath,
		"--id", requested.Build.ID, "--json",
	)
	if err := json.Unmarshal(logsOutput, &logs); err != nil {
		t.Fatalf("decode persisted logs: %v\n%s", err, logsOutput)
	}
	if len(logs.Logs) != 1 || logs.Logs[0].Message != "in [WORKSPACE] authenticated with [REDACTED]" {
		t.Fatalf("sanitized logs = %#v", logs.Logs)
	}

	cancelArgs := append([]string(nil), requestArgs...)
	replacePublicBuildFlagValue(cancelArgs, "--target", cancelTarget)
	replacePublicBuildFlagValue(cancelArgs, "--recipe", cancelRecipe)
	var cancellationCandidate publicBuildRequestResult
	if output := runBinary(t, binary, cancelArgs...); json.Unmarshal(output, &cancellationCandidate) != nil {
		t.Fatalf("decode cancellation candidate: %s", output)
	}
	var cancelled publicBuildView
	cancelOutput := runBinary(t, binary,
		"public-build", "cancel", "--config", configPath,
		"--id", cancellationCandidate.Build.ID, "--json",
	)
	if err := json.Unmarshal(cancelOutput, &cancelled); err != nil || cancelled.State != "cancelled" {
		t.Fatalf("cancel result = %#v, error = %v\n%s", cancelled, err, cancelOutput)
	}

	failArgs := append([]string(nil), requestArgs...)
	replacePublicBuildFlagValue(failArgs, "--target", failTarget)
	replacePublicBuildFlagValue(failArgs, "--recipe", failRecipe)
	var failureCandidate publicBuildRequestResult
	if output := runBinary(t, binary, failArgs...); json.Unmarshal(output, &failureCandidate) != nil {
		t.Fatalf("decode failure candidate: %s", output)
	}
	var failureLease publicBuildLeaseResult
	failureLeaseOutput := runBinary(t, binary,
		"public-build", "worker", "lease", "--config", configPath,
		"--worker-id", "worker-linux-amd64",
		"--integration", "turbo", "--platform", "linux/amd64", "--json",
	)
	if err := json.Unmarshal(failureLeaseOutput, &failureLease); err != nil {
		t.Fatalf("decode failure lease: %v\n%s", err, failureLeaseOutput)
	}
	var failed publicBuildView
	failOutput := runBinary(t, binary,
		"public-build", "worker", "fail", "--config", configPath,
		"--id", failureCandidate.Build.ID, "--worker-id", failureLease.WorkerID,
		"--lease-token", failureLease.LeaseToken,
		"--reason", "publisher-secret failed", "--json",
	)
	if err := json.Unmarshal(failOutput, &failed); err != nil || failed.State != "failed" || failed.Failure != "[REDACTED] failed" {
		t.Fatalf("fail result = %#v, error = %v\n%s", failed, err, failOutput)
	}
	var retried publicBuildRequestResult
	if output := runBinary(t, binary, failArgs...); json.Unmarshal(output, &retried) != nil {
		t.Fatalf("decode failure retry: %s", output)
	}
	if retried.Reused || retried.Build.ID == failureCandidate.Build.ID {
		t.Fatalf("failure retry = %#v", retried)
	}

	requestBody := `{"repository":"https://github.com/acme/widget"}`
	if status := publicBuildHTTPStatus(t, address, "", http.MethodPost, "/v1/public-builds", requestBody); status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated request status = %d, want 401", status)
	}
	if status := publicBuildHTTPStatus(t, address, credentials.LocalToken, http.MethodPost, "/v1/public-build-worker/lease", `{}`); status != http.StatusUnauthorized {
		t.Fatalf("client worker-lease status = %d, want 401", status)
	}
	if status := publicBuildHTTPStatus(t, address, credentials.LocalToken, http.MethodPost, "/v1/public/publish", "client bytes"); status != http.StatusUnauthorized {
		t.Fatalf("client publication status = %d, want 401", status)
	}
	disallowed := strings.Replace(publicBuildRequestJSON(), "https://github.com/acme/widget", "https://github.com/acme/not-allowlisted", 1)
	if status := publicBuildHTTPStatus(t, address, credentials.LocalToken, http.MethodPost, "/v1/public-builds", disallowed); status != http.StatusBadRequest {
		t.Fatalf("non-allowlisted Public Build status = %d, want 400", status)
	}
	mutable := strings.Replace(publicBuildRequestJSON(), "0123456789abcdef0123456789abcdef01234567", "main", 1)
	if status := publicBuildHTTPStatus(t, address, credentials.LocalToken, http.MethodPost, "/v1/public-builds", mutable); status != http.StatusBadRequest {
		t.Fatalf("mutable Public Build status = %d, want 400", status)
	}
	unknownBytesBody := strings.TrimSuffix(publicBuildRequestJSON(), "}") + `,"artifactBytes":"Y2xpZW50IHBvaXNvbg=="}`
	if status := publicBuildHTTPStatus(t, address, credentials.LocalToken, http.MethodPost, "/v1/public-builds", unknownBytesBody); status != http.StatusBadRequest {
		t.Fatalf("request API byte injection status = %d, want 400", status)
	}
}

func publicBuildRequestJSON() string {
	return `{"repository":"` + publicFixtureRepository + `","commit":"` + publicFixtureCommit + `","integration":"turbo","target":"@acme/widget#compile","recipeDigest":"` + publicFixtureRecipe("turbo", "@acme/widget#compile") + `","platform":"linux/amd64","inputs":[{"name":"runtime","value":"node@24"}],"resources":{"cpuMillis":1000,"memoryBytes":1073741824,"diskBytes":2147483648,"timeoutMilliseconds":120000}}`
}

func replacePublicBuildFlagValue(arguments []string, name, value string) {
	for index := 0; index+1 < len(arguments); index++ {
		if arguments[index] == name {
			arguments[index+1] = value
			return
		}
	}
	panic("missing Public Build test flag " + name)
}

func publicBuildHTTPStatus(t *testing.T, address, token, method, path, body string) int {
	t.Helper()
	request, err := http.NewRequest(method, "http://"+address+path, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	return response.StatusCode
}
