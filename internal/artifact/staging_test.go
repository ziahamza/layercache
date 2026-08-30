package artifact

import (
	"errors"
	"testing"
)

func TestUploadStagingIsBoundedAcrossConcurrentReservations(t *testing.T) {
	store, err := Open(t.Context(), t.TempDir(), 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	first, err := store.ReserveUploadStaging(4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReserveUploadStaging(1); !errors.Is(err, ErrStagingQuota) {
		t.Fatalf("second reservation error = %v, want ErrStagingQuota", err)
	}
	first.Release()

	second, err := store.ReserveUploadStaging(4)
	if err != nil {
		t.Fatalf("reservation after release: %v", err)
	}
	second.Release()
	second.Release()
}

func TestTotalTransientStagingAllowsOneUploadAndOneCASAssembly(t *testing.T) {
	store, err := Open(t.Context(), t.TempDir(), 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	upload, err := store.ReserveUploadStaging(4)
	if err != nil {
		t.Fatal(err)
	}
	defer upload.Release()
	assembly := store.beginArtifactStaging()
	defer assembly.Release()
	if err := assembly.grow(4); err != nil {
		t.Fatalf("full-size CAS assembly beside upload staging: %v", err)
	}
	if err := assembly.grow(1); !errors.Is(err, ErrStagingQuota) {
		t.Fatalf("staging beyond two times MaxBytes error = %v, want ErrStagingQuota", err)
	}
}
