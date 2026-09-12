package buildkit

import (
	"strings"
	"testing"
)

func TestProgressRemoteBytesRemainUnknownForZeroByteManifestStatus(t *testing.T) {
	t.Parallel()

	const reference = "registry.example/cache/linux-amd64:main"
	accumulator := newProgressAccumulator()
	accumulator.addLine([]byte(`{"vertexes":[{"digest":"sha256:import","name":"importing cache manifest from ` + reference + `"}]}`))
	accumulator.addLine([]byte(`{"statuses":[{"id":"inferred cache manifest type: application/vnd.oci.image.manifest.v1+json done","vertex":"sha256:import","current":0}]}`))

	metrics := accumulator.metrics([]string{reference})
	if metrics.RemoteBytesMeasured || metrics.RemoteBytes != 0 {
		t.Fatalf("metrics = %+v, want remote bytes to remain unknown", metrics)
	}
}

func TestProgressRemoteBytesUseMaximumTransferredBytesPerStatus(t *testing.T) {
	t.Parallel()

	const reference = "registry.example/cache/linux-amd64:main"
	accumulator := newProgressAccumulator()
	for _, line := range strings.Split(`{"vertexes":[{"digest":"sha256:import","name":"importing cache manifest from `+reference+`"}]}
{"statuses":[{"id":"fetch manifest","vertex":"sha256:import","current":512}]}
{"statuses":[{"id":"fetch manifest","vertex":"sha256:import","current":1024}]}`, "\n") {
		accumulator.addLine([]byte(line))
	}

	metrics := accumulator.metrics([]string{reference})
	if !metrics.RemoteBytesMeasured || metrics.RemoteBytes != 1024 {
		t.Fatalf("metrics = %+v, want one measured 1024-byte transfer", metrics)
	}
}

func TestProgressRemoteBytesRetainObservedImportDirection(t *testing.T) {
	t.Parallel()

	const reference = "registry.example/cache/linux-amd64:main"
	accumulator := newProgressAccumulator()
	accumulator.addLine([]byte(`{"vertexes":[{"digest":"sha256:import","name":"importing cache manifest from ` + reference + `"}]}`))
	accumulator.addLine([]byte(`{"statuses":[{"id":"fetch manifest","vertex":"sha256:import","current":2048}]}`))
	metrics := accumulator.metricsForScopes([]CacheScope{{Direction: "import", Reference: reference}})
	if metrics.RemoteDownloadedBytes != 2048 || metrics.RemoteUploadedBytes != 0 {
		t.Fatalf("metrics = %+v, want a measured 2048-byte import", metrics)
	}
}

func TestProgressRetainsIgnoredVertexFailureAsWarning(t *testing.T) {
	t.Parallel()

	accumulator := newProgressAccumulator()
	accumulator.addLine([]byte(`{"vertexes":[{"digest":"sha256:export","name":"exporting cache","error":"registry unavailable"}]}`))
	if err := accumulator.err(); err == nil || !strings.Contains(err.Error(), "vertex failure") {
		t.Fatalf("progress warning = %v, want ignored vertex failure", err)
	}
}
