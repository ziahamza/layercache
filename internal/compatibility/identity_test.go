package compatibility_test

import (
	"strings"
	"testing"

	"github.com/layercache/layercache/internal/compatibility"
)

func TestValidateAcceptsBoundedCanonicalIdentity(t *testing.T) {
	t.Parallel()
	for _, identity := range []string{
		"linux-amd64-schema1",
		"darwin-arm64-schema1-node@24.7.0",
		"sha256:0123456789abcdef",
		"linux-amd64-musl_1.2+node@24-schema1",
	} {
		if err := compatibility.Validate(identity); err != nil {
			t.Errorf("Validate(%q): %v", identity, err)
		}
	}
}

func TestValidateRejectsAmbiguousOrUnboundedIdentity(t *testing.T) {
	t.Parallel()
	for _, identity := range []string{
		"", " linux-amd64-schema1", "Linux-amd64-schema1", "linux amd64", "linux,amd64",
		"linux/amd64", strings.Repeat("a", compatibility.MaxLength+1),
	} {
		if err := compatibility.Validate(identity); err == nil {
			t.Errorf("Validate(%q) succeeded", identity)
		}
	}
}
