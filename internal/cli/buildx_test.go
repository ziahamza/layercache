package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/buildkit"
	"github.com/layercache/layercache/internal/config"
	"github.com/layercache/layercache/internal/measurement"
	"github.com/layercache/layercache/internal/publictrust"
)

func TestBuildxPlanContinuesWhenTeamCapabilityRefreshFails(t *testing.T) {
	t.Parallel()

	cfg, err := config.Defaults()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	cfg.DataDir = filepath.Join(root, "cache")
	cfg.TeamURL = "http://127.0.0.1:1"
	cfg.TeamToken = "expired-team-token"
	cfg.TeamTokenExpiresAt = time.Now().Add(-time.Hour)
	// An invalid account makes the refresh fail before consulting the host's
	// credential manager, keeping this regression test portable and isolated.
	cfg.GitHubCredentialAccount = "invalid account"
	configPath := filepath.Join(root, "config.json")
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	err = runBuildx(context.Background(), []string{
		"plan", "--config", configPath, root,
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("Buildx plan failed after capability refresh failure: %v", err)
	}
	if !strings.Contains(stderr.String(), "Team capability refresh failed; the build will continue") {
		t.Fatalf("missing fail-open refresh warning: %q", stderr.String())
	}
	var plan buildkit.Plan
	if err := json.Unmarshal(stdout.Bytes(), &plan); err != nil {
		t.Fatalf("decode Buildx plan: %v; output=%q", err, stdout.String())
	}
	if plan.Command.Path == "" || len(plan.Command.Args) == 0 {
		t.Fatalf("Buildx plan did not reach native command planning: %+v", plan)
	}
}

func TestRecordBuildkitMeasurementPersistsOneNativeVertexGroup(t *testing.T) {
	t.Parallel()

	cfg := config.Config{DataDir: t.TempDir(), ProjectID: "github.com/acme/widgets"}
	finishedAt := time.Date(2026, time.August, 31, 18, 0, 2, 0, time.UTC)
	err := recordBuildkitMeasurement(
		context.Background(), cfg, "run-buildkit-report", "sha256:workspace", "linux-amd64",
		finishedAt.Add(-2*time.Second), finishedAt,
		buildkit.ProgressMetrics{
			CompletedVertices: 4, CachedVertices: 2, CacheSource: "unattributed",
			BuildDurationMS: 1800, GraphDigest: "sha256:completed-graph",
			RemoteDownloadedBytes: 4096, RemoteUploadedBytes: 1024,
		},
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := measurement.OpenSQLiteRepository(filepath.Join(cfg.DataDir, "measurements.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	report, err := repository.RunReport("run-buildkit-report")
	if err != nil {
		t.Fatal(err)
	}
	if report.Groups != 1 || report.Eligible != 4 || report.Hits != 2 || report.Misses != 2 || report.HitRate != 0.5 {
		t.Fatalf("BuildKit vertex report = %#v", report)
	}
	if len(report.Integrations) != 1 || report.Integrations[0].Integration != measurement.IntegrationBuildkit ||
		report.Integrations[0].Groups != 1 || report.Integrations[0].Eligible != 4 || report.Integrations[0].Hits != 2 {
		t.Fatalf("BuildKit integration report = %#v", report.Integrations)
	}
	outcome := report.Outcomes[0]
	if outcome.Integration != measurement.IntegrationBuildkit || outcome.WorkspaceID != "sha256:workspace" ||
		outcome.Source != measurement.SourceUnattributed || !outcome.Degraded ||
		outcome.ExecutionDurationMS != nil || outcome.ProducerDurationMS != nil || outcome.Timing.RestoreMS != 1800 {
		t.Fatalf("BuildKit outcome = %#v", outcome)
	}
	if outcome.Bytes != (measurement.Bytes{Downloaded: 4096, Uploaded: 1024}) ||
		outcome.EligibleUnits != 4 || outcome.HitUnits != 2 || !strings.HasPrefix(outcome.ArtifactID, "sha256:") {
		t.Fatalf("BuildKit evidence = %#v", outcome)
	}
	period, err := repository.PeriodReport(finishedAt.Add(-time.Hour), finishedAt.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(period.Integrations) != 1 || period.Integrations[0] != report.Integrations[0] {
		t.Fatalf("historical BuildKit integration report = %#v", period.Integrations)
	}
}

func TestRecordBuildkitMeasurementUsesObservedColdGraphBaseline(t *testing.T) {
	t.Parallel()
	cfg := config.Config{DataDir: t.TempDir(), ProjectID: "github.com/acme/widgets"}
	finishedAt := time.Date(2026, time.August, 31, 19, 0, 0, 0, time.UTC)
	graph := "sha256:stable-completed-graph"
	if err := recordBuildkitMeasurement(
		context.Background(), cfg, "run-cold", "workspace", "linux-amd64",
		finishedAt.Add(-4*time.Second), finishedAt,
		buildkit.ProgressMetrics{CompletedVertices: 4, BuildDurationMS: 4000, GraphDigest: graph}, false,
	); err != nil {
		t.Fatal(err)
	}
	if err := recordBuildkitMeasurement(
		context.Background(), cfg, "run-warm", "workspace", "linux-amd64",
		finishedAt.Add(time.Minute-time.Second), finishedAt.Add(time.Minute),
		buildkit.ProgressMetrics{CompletedVertices: 4, CachedVertices: 4, BuildDurationMS: 1000, GraphDigest: graph}, false,
	); err != nil {
		t.Fatal(err)
	}
	repository, err := measurement.OpenSQLiteRepository(filepath.Join(cfg.DataDir, "measurements.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	report, err := repository.RunReport("run-warm")
	if err != nil {
		t.Fatal(err)
	}
	outcome := report.Outcomes[0]
	if outcome.ProducerDurationMS == nil || *outcome.ProducerDurationMS != 4000 || outcome.Timing.RestoreMS != 1000 {
		t.Fatalf("warm BuildKit baseline = %+v", outcome)
	}
	if report.NetEstimatedBuildTimeSaved.Milliseconds == nil || *report.NetEstimatedBuildTimeSaved.Milliseconds != 3000 {
		t.Fatalf("warm BuildKit net estimate = %+v", report.NetEstimatedBuildTimeSaved)
	}
}

func TestRecordBuildkitMeasurementKeepsMissingProgressEvidenceUnknown(t *testing.T) {
	t.Parallel()

	cfg := config.Config{DataDir: t.TempDir(), ProjectID: "github.com/acme/widgets"}
	finishedAt := time.Date(2026, time.August, 31, 18, 30, 0, 0, time.UTC)
	if err := recordBuildkitMeasurement(
		context.Background(), cfg, "run-buildkit-unknown", "sha256:workspace", "linux-amd64",
		finishedAt.Add(-time.Second), finishedAt,
		buildkit.ProgressMetrics{BuildDurationMS: 1000}, true,
	); err != nil {
		t.Fatal(err)
	}
	repository, err := measurement.OpenSQLiteRepository(filepath.Join(cfg.DataDir, "measurements.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	report, err := repository.RunReport("run-buildkit-unknown")
	if err != nil {
		t.Fatal(err)
	}
	if report.Groups != 1 || report.Eligible != 0 || report.Hits != 0 || report.Misses != 0 ||
		report.Outcomes[0].Result != measurement.ResultUnknown || !report.Outcomes[0].Degraded {
		t.Fatalf("missing BuildKit progress report = %#v", report)
	}
}

func TestResolveBuildkitPublicImportsBindsFullSignedIdentity(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		platform      string
		compatibility string
	}{
		{platform: "linux/amd64", compatibility: "linux-amd64"},
		{platform: "linux/arm64", compatibility: "linux-arm64"},
	} {
		t.Run(test.platform, func(t *testing.T) {
			testResolveBuildkitPublicImport(t, test.platform, test.compatibility)
		})
	}
}

func testResolveBuildkitPublicImport(t *testing.T, platform, compatibility string) {
	t.Helper()

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	now := time.Now().UTC()
	publication := publictrust.Publication{
		Integration: "buildkit", Project: "project-123", Compatibility: compatibility, NativeKey: "release-image",
		Repository: "https://github.com/acme/widget", Commit: strings.Repeat("a", 40),
		RecipeDigest: "sha256:" + strings.Repeat("b", 64), Target: "release-image", Platform: platform,
		Toolchain: "dockerfile-v1", Builder: "layercache-public-builder-v1",
		BuilderImageDigest: "sha256:" + strings.Repeat("e", 64),
		Digest:             strings.Repeat("c", 64), Size: 1024, DurationMS: 250, BuildID: "build-123",
		IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour),
	}
	envelope, err := publictrust.Sign(privateKey, publication)
	if err != nil {
		t.Fatalf("sign publication: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/public/resolve" {
			http.NotFound(writer, request)
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"envelope": envelope, "artifactUrl": "/v1/public/artifacts/" + publication.CacheIdentity(),
		})
	}))
	defer server.Close()
	cfg := config.Config{
		ProjectID: "project-123", PublicURL: server.URL, PublicTrustKey: publictrust.EncodePublicKey(publicKey),
		BuildkitPublicRepository: "registry.example/public/widget",
	}
	selector := publication.NativeKey + "=" + publication.Identity()
	imports, warnings, err := resolveBuildkitPublicImports(context.Background(), cfg, platform, []string{selector})
	if err != nil {
		t.Fatalf("resolve Public Cache import: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if len(imports) != 1 || imports[0].Digest != "sha256:"+publication.Digest ||
		imports[0].PublicIdentity != publication.Identity() {
		t.Fatalf("resolved imports = %+v", imports)
	}

	wrongIdentity := publication.NativeKey + "=" + strings.Repeat("d", 64)
	imports, warnings, err = resolveBuildkitPublicImports(context.Background(), cfg, platform, []string{wrongIdentity})
	if err != nil {
		t.Fatalf("resolve wrong Public Cache identity: %v", err)
	}
	if len(imports) != 0 || len(warnings) != 1 {
		t.Fatalf("wrong full identity was accepted: imports=%+v warnings=%v", imports, warnings)
	}
}

func TestParseBuildkitPublicSelectorRejectsRawDigestOrMutableTag(t *testing.T) {
	t.Parallel()

	for _, value := range []string{
		"registry.example/public/widget:latest",
		"registry.example/public/widget@sha256:" + strings.Repeat("a", 64),
		"native-key=" + strings.Repeat("z", 64),
	} {
		if _, err := parseBuildkitPublicSelector(value); err == nil {
			t.Fatalf("unsafe Public Cache selector %q was accepted", value)
		}
	}
}
