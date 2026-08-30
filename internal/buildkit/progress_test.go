package buildkit_test

import (
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
