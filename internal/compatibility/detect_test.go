package compatibility

import (
	"context"
	"strings"
	"testing"
)

func TestDetectedCompatibilityIsCanonicalAndIncludesSchema(t *testing.T) {
	identity := Detect(context.Background())
	if err := Validate(identity); err != nil {
		t.Fatalf("Detect() = %q: %v", identity, err)
	}
	if !strings.Contains(identity, "schema1") {
		t.Fatalf("Detect() = %q", identity)
	}
}
