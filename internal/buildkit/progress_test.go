package buildkit_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/layercache/layercache/internal/buildkit"
)

func TestParseProgressCountsUniqueCompletedAndCachedVertices(t *testing.T) {
	t.Parallel()

	progress := strings.Join([]string{
		`{"id":"vertex-1","name":"load context","started":"2026-08-30T10:00:00Z"}`,
		`{"id":"vertex-1","name":"load context","completed":"2026-08-30T10:00:01Z","cached":true}`,
		`{"id":"vertex-2","name":"compile","completed":"2026-08-30T10:00:03Z"}`,
		`{"id":"vertex-2","name":"compile","completed":"2026-08-30T10:00:03Z"}`,
		`{"id":"vertex-3","name":"export image","started":"2026-08-30T10:00:03Z"}`,
	}, "\n")

	metrics, err := buildkit.ParseProgress(strings.NewReader(progress))
	if err != nil {
		t.Fatalf("parse progress: %v", err)
	}
	if metrics.Vertices != 3 {
		t.Fatalf("vertices = %d, want 3", metrics.Vertices)
	}
	if metrics.CompletedVertices != 2 {
		t.Fatalf("completed vertices = %d, want 2", metrics.CompletedVertices)
	}
	if metrics.CachedVertices != 1 {
		t.Fatalf("cached vertices = %d, want 1", metrics.CachedVertices)
	}
	if metrics.CacheHitRate != 0.5 {
		t.Fatalf("cache hit rate = %f, want 0.5", metrics.CacheHitRate)
	}
}

func TestParseProgressCountsCachedVerticesOnlyAfterCompletion(t *testing.T) {
	t.Parallel()

	metrics, err := buildkit.ParseProgress(strings.NewReader(strings.Join([]string{
		`{"id":"incomplete","cached":true}`,
		`{"id":"complete","completed":"2026-08-30T10:00:01Z","cached":true}`,
	}, "\n")))
	if err != nil {
		t.Fatalf("parse progress: %v", err)
	}
	if metrics.CompletedVertices != 1 || metrics.CachedVertices != 1 || metrics.CacheHitRate != 1 {
		t.Fatalf("metrics = %+v, want one completed cached vertex", metrics)
	}
}

func TestParseProgressBoundsOneMalformedLineAndContinues(t *testing.T) {
	t.Parallel()

	var progress bytes.Buffer
	progress.Write(bytes.Repeat([]byte("x"), 5<<20))
	progress.WriteByte('\n')
	progress.WriteString(`{"id":"complete","completed":"2026-08-30T10:00:01Z","cached":true}`)
	progress.WriteByte('\n')
	metrics, err := buildkit.ParseProgress(&progress)
	if err == nil || !strings.Contains(err.Error(), "progress line exceeded") {
		t.Fatalf("expected bounded-line warning, got %v", err)
	}
	if metrics.CompletedVertices != 1 || metrics.CachedVertices != 1 {
		t.Fatalf("valid progress after oversized line was lost: %+v", metrics)
	}
}

func TestParseProgressReadsBuildxRawJSONVertexEnvelope(t *testing.T) {
	t.Parallel()

	// Buildx 0.36 emits raw progress as SolveStatus envelopes. Vertices are in
	// the `vertexes` array and use their content digest as the stable identity.
	progress := strings.Join([]string{
		`{"vertexes":[{"digest":"sha256:cached","name":"RUN compile","started":"2026-08-30T10:00:00Z","completed":"2026-08-30T10:00:00Z","cached":true}]}`,
		`{"statuses":[{"id":"exporting layers","vertex":"sha256:export","current":0}]}`,
		`{"vertexes":[{"digest":"sha256:executed","name":"RUN test","started":"2026-08-30T10:00:00Z","completed":"2026-08-30T10:00:01Z"}]}`,
	}, "\n")

	metrics, err := buildkit.ParseProgress(strings.NewReader(progress))
	if err != nil {
		t.Fatalf("parse Buildx raw JSON: %v", err)
	}
	if metrics.Vertices != 2 || metrics.CompletedVertices != 2 || metrics.CachedVertices != 1 || metrics.CacheHitRate != 0.5 {
		t.Fatalf("metrics = %+v, want 2 completed vertices with one cache hit", metrics)
	}
}

func TestProgressGraphDigestExcludesPerRunExporterVertices(t *testing.T) {
	t.Parallel()

	parse := func(exportDigest string) buildkit.ProgressMetrics {
		metrics, err := buildkit.ParseProgress(strings.NewReader(strings.Join([]string{
			`{"vertexes":[{"digest":"sha256:stable","name":"[build 1/1] RUN compile","completed":"2026-08-30T10:00:01Z"}]}`,
			`{"vertexes":[{"digest":"` + exportDigest + `","name":"exporting to oci image format","completed":"2026-08-30T10:00:02Z"}]}`,
		}, "\n")))
		if err != nil {
			t.Fatal(err)
		}
		return metrics
	}
	first := parse("sha256:export-one")
	second := parse("sha256:export-two")
	if first.GraphDigest == "" || first.GraphDigest != second.GraphDigest {
		t.Fatalf("graph digests differ across exporter instances: %q and %q", first.GraphDigest, second.GraphDigest)
	}
}

func TestProgressGraphDigestCanonicalizesPerSolveVertexDigests(t *testing.T) {
	t.Parallel()

	parse := func(definition, context, copyVertex, exporter, copyName string) buildkit.ProgressMetrics {
		metrics, err := buildkit.ParseProgress(strings.NewReader(strings.Join([]string{
			`{"vertexes":[{"digest":"` + definition + `","name":"[internal] load build definition from Dockerfile","completed":"2026-08-30T10:00:01Z"}]}`,
			`{"vertexes":[{"digest":"` + context + `","name":"[internal] load build context","completed":"2026-08-30T10:00:01Z"}]}`,
			`{"vertexes":[{"digest":"` + copyVertex + `","inputs":["` + context + `"],"name":"` + copyName + `","completed":"2026-08-30T10:00:02Z"}]}`,
			`{"vertexes":[{"digest":"` + exporter + `","name":"exporting to oci image format","completed":"2026-08-30T10:00:03Z"}]}`,
		}, "\n")))
		if err != nil {
			t.Fatal(err)
		}
		return metrics
	}
	first := parse("sha256:def-one", "sha256:context-one", "sha256:copy-one", "sha256:export-one", "[1/1] COPY payload /payload")
	second := parse("sha256:def-two", "sha256:context-two", "sha256:copy-two", "sha256:export-two", "[1/1] COPY payload /payload")
	changed := parse("sha256:def-three", "sha256:context-three", "sha256:copy-three", "sha256:export-three", "[1/1] COPY other /payload")
	if first.GraphDigest == "" || first.GraphDigest != second.GraphDigest {
		t.Fatalf("graph identity retained per-solve digests: %q and %q", first.GraphDigest, second.GraphDigest)
	}
	if changed.GraphDigest == first.GraphDigest {
		t.Fatalf("different observed vertex graph kept identity %q", changed.GraphDigest)
	}
}
