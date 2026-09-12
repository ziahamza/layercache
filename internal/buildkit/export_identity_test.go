package buildkit_test

import (
	"regexp"
	"testing"

	"github.com/layercache/layercache/internal/buildkit"
)

func TestNewTeamExportIDMakesEveryPublicationRefUnique(t *testing.T) {
	t.Parallel()

	first, err := buildkit.NewTeamExportID("run-123")
	if err != nil {
		t.Fatalf("first export identity: %v", err)
	}
	second, err := buildkit.NewTeamExportID("run-123")
	if err != nil {
		t.Fatalf("second export identity: %v", err)
	}
	if first == second {
		t.Fatalf("two export identities matched: %q", first)
	}
	pattern := regexp.MustCompile(`^run-123-u[0-9a-f]{32}$`)
	if !pattern.MatchString(first) || !pattern.MatchString(second) {
		t.Fatalf("export identities do not contain 128-bit hex suffixes: %q %q", first, second)
	}
}
