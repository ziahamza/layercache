package publicbuild_test

import (
	"strings"
	"testing"

	"github.com/layercache/layercache/internal/publicbuild"
)

func TestBuilderImageDigestBindsEveryImmutableGuestAsset(t *testing.T) {
	t.Parallel()

	components := []string{
		"sha256:" + strings.Repeat("a", 64),
		"sha256:" + strings.Repeat("b", 64),
		"sha256:" + strings.Repeat("c", 64),
	}
	want, err := publicbuild.BuilderImageDigest(components[0], components[1], components[2])
	if err != nil {
		t.Fatal(err)
	}
	for index := range components {
		changed := append([]string(nil), components...)
		changed[index] = "sha256:" + strings.Repeat(string(rune('d'+index)), 64)
		got, err := publicbuild.BuilderImageDigest(changed[0], changed[1], changed[2])
		if err != nil {
			t.Fatal(err)
		}
		if got == want {
			t.Fatalf("component %d did not affect builder image digest", index)
		}
	}
	if _, err := publicbuild.BuilderImageDigest(components[0], "SHA256:"+strings.Repeat("b", 64), components[2]); err == nil {
		t.Fatal("non-canonical component digest was accepted")
	}
}
