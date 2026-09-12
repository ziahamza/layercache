package acceptance_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/access"
	"github.com/layercache/layercache/internal/config"
	"github.com/layercache/layercache/internal/measurement"
)

func TestRunReportExplainsTurboMissThenLocalHit(t *testing.T) {
	root := t.TempDir()
	binary := buildLayerCache(t)
	address := availableAddress(t)
	configPath := filepath.Join(root, "config.json")
	runBinary(t, binary,
		"setup", "--config", configPath,
		"--data-dir", filepath.Join(root, "cache"),
		"--listen", address,
		"--project", "github.com/acme/widget",
		"--non-interactive", "--json",
	)
	runBinary(t, binary, "start", "--config", configPath, "--json")
	t.Cleanup(func() { _, _ = runBinaryResult(binary, "stop", "--config", configPath, "--json") })

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	artifactURL := "http://" + address + "/v8/artifacts/report-fixture"
	want := []byte("reportable-turbo-artifact")

	missRun := "run-report-miss"
	putTurboForRun(t, cfg, missRun, artifactURL, want, 250*time.Millisecond)
	assertTurboRunIsProvisional(t, cfg, missRun)
	reconcileTurboRun(t, cfg, missRun, "report-fixture", measurement.ResultMiss, 250*time.Millisecond)
	miss := reportForRun(t, binary, configPath, missRun)
	if miss.Eligible != 1 || miss.Hits != 0 || miss.Misses != 1 {
		t.Fatalf("miss report counts = %+v", miss)
	}
	if miss.Outcomes[0].Result != measurement.ResultMiss || miss.Outcomes[0].ExecutionDurationMS == nil || *miss.Outcomes[0].ExecutionDurationMS != 250 {
		t.Fatalf("miss outcome = %+v", miss.Outcomes[0])
	}
	if miss.Bytes.Uploaded != int64(len(want)) {
		t.Fatalf("miss uploaded bytes = %d, want %d", miss.Bytes.Uploaded, len(want))
	}

	hitRun := "run-report-hit"
	got, source := getTurboForRun(t, cfg, hitRun, artifactURL)
	if !bytes.Equal(got, want) || source != "local" {
		t.Fatalf("restored %q from %q", got, source)
	}
	assertTurboRunIsProvisional(t, cfg, hitRun)
	reconcileTurboRun(t, cfg, hitRun, "report-fixture", measurement.ResultHit, 10*time.Millisecond)
	hit := reportForRun(t, binary, configPath, hitRun)
	if hit.Eligible != 1 || hit.Hits != 1 || hit.Misses != 0 || hit.HitRate != 1 {
		t.Fatalf("hit report counts = %+v", hit)
	}
	if hit.Outcomes[0].Source != measurement.SourceLocalCache || hit.Outcomes[0].ProducerDurationMS == nil || *hit.Outcomes[0].ProducerDurationMS != 250 {
		t.Fatalf("hit outcome = %+v", hit.Outcomes[0])
	}
	if hit.Bytes.Downloaded != int64(len(want)) {
		t.Fatalf("hit downloaded bytes = %d, want %d", hit.Bytes.Downloaded, len(want))
	}
	if hit.NetEstimatedBuildTimeSaved.Milliseconds == nil {
		t.Fatal("hit report omitted the signed net time estimate")
	}
}

func putTurboForRun(t *testing.T, cfg config.Config, runID, artifactURL string, body []byte, duration time.Duration) {
	t.Helper()
	token, err := mintTurboCapabilityForRun(cfg, runID)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPut, artifactURL, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("x-artifact-duration", fmt.Sprint(duration.Milliseconds()))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(response.Body)
		t.Fatalf("Turbo PUT status = %d: %s", response.StatusCode, message)
	}
}

func getTurboForRun(t *testing.T, cfg config.Config, runID, artifactURL string) ([]byte, string) {
	t.Helper()
	token, err := mintTurboCapabilityForRun(cfg, runID)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodGet, artifactURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("Turbo GET status = %d: %s", response.StatusCode, body)
	}
	return body, response.Header.Get("x-layercache-source")
}

func mintTurboCapabilityForRun(cfg config.Config, runID string) (string, error) {
	now := time.Now().UTC()
	return access.MintCapabilityToken(cfg.LocalToken, access.Claims{
		Subject: cfg.InstallationID, RunID: runID, Project: cfg.ProjectID,
		Integration: "turbo", Compatibility: cfg.CompatibilityID,
		Capabilities: []access.Capability{access.CapabilityWrite}, ExpiresAt: now.Add(time.Hour),
	}, now)
}

func assertTurboRunIsProvisional(t *testing.T, cfg config.Config, runID string) {
	t.Helper()
	repository, err := measurement.OpenSQLiteRepository(filepath.Join(cfg.DataDir, "measurements.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if _, err := repository.RunReport(runID); !errors.Is(err, measurement.ErrRunNotFound) {
		t.Fatalf("raw Turbo transport observations produced a final run report: %v", err)
	}
}

func reconcileTurboRun(
	t *testing.T,
	cfg config.Config,
	runID string,
	hash string,
	result measurement.Result,
	execution time.Duration,
) {
	t.Helper()
	startedAt := time.Now().UTC()
	finishedAt := startedAt.Add(execution)
	hit := result == measurement.ResultHit
	timeSaved := int64(0)
	source := ""
	if hit {
		timeSaved = 250
		source = "REMOTE"
	}
	document, err := json.Marshal(map[string]any{
		"id": "summary-" + runID, "version": "1",
		"execution": map[string]any{
			"startTime": startedAt.UnixMilli(), "endTime": finishedAt.UnixMilli(),
		},
		"tasks": []any{map[string]any{
			"taskId": "pkg#build", "hash": hash, "dependencies": []string{},
			"cache": map[string]any{
				"local": false, "remote": hit, "status": string(result),
				"source": source, "timeSaved": timeSaved,
			},
			"execution": map[string]any{
				"startTime": startedAt.UnixMilli(), "endTime": finishedAt.UnixMilli(), "exitCode": 0,
			},
			"resolvedTaskDefinition": map[string]any{"cache": true},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := measurement.OpenSQLiteRepository(filepath.Join(cfg.DataDir, "measurements.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	count, err := repository.ReconcileTurboSummaries(runID, measurement.TurboReconcileOptions{
		Project: cfg.ProjectID, CompatibilityID: cfg.CompatibilityID,
	}, [][]byte{document})
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("reconciled Turbo tasks = %d, want 1", count)
	}
}

func reportForRun(t *testing.T, binary, configPath, runID string) measurement.RunReport {
	t.Helper()
	output := runBinary(t, binary, "report", "--config", configPath, "--run", runID, "--json")
	var report measurement.RunReport
	if err := json.Unmarshal(output, &report); err != nil {
		t.Fatalf("decode run report: %v\n%s", err, output)
	}
	return report
}

func runBinaryResult(binary string, args ...string) ([]byte, error) {
	command := exec.Command(binary, args...)
	return command.CombinedOutput()
}
