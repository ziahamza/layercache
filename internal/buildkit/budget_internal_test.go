package buildkit

import "testing"

func TestEffectiveMinFreeBytesUsesFivePercentFilesystemFloor(t *testing.T) {
	t.Parallel()

	const gibibyte = int64(1 << 30)
	if got := effectiveMinFreeBytes(2*gibibyte, 100*gibibyte); got != 5*gibibyte {
		t.Fatalf("effective minimum free bytes = %d, want five-percent floor %d", got, 5*gibibyte)
	}
	if got := effectiveMinFreeBytes(8*gibibyte, 100*gibibyte); got != 8*gibibyte {
		t.Fatalf("effective minimum free bytes = %d, want configured override %d", got, 8*gibibyte)
	}
}
